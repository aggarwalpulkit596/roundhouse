package builder

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/fsutil"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/image"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/rootfs"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/runtime"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/spec"
)

// Options configure one build.
type Options struct {
	ContextDir string
	File       string // default <ContextDir>/Railfile, then Dockerfile
	Tag        string
	Target     string
	BuildArgs  map[string]string
	NoCache    bool
	// Epoch makes the output reproducible (SOURCE_DATE_EPOCH).
	Epoch *time.Time
	// Network for RUN steps: "host" (default, like `docker build`'s
	// practical default behind proxies) or "none" (hermetic).
	Network string
	Out     io.Writer
}

// Builder builds images into a store.
type Builder struct {
	Store *image.Store
	// Dir holds the step cache and scratch space.
	Dir string
}

// Result summarizes a build.
type Result struct {
	Image    image.Image
	Steps    int
	Cached   int
	Duration time.Duration
}

// stageState is the evolving image of one stage.
type stageState struct {
	layers []image.Digest     // diff IDs, bottom first
	blobs  []image.Descriptor // compressed layers, parallel to layers
	cfg    image.Config
	key    string            // cache key of the last step
	args   map[string]string // ARG values in scope
}

type stageResult struct {
	done chan struct{}
	st   *stageState
	err  error
}

type cacheEntry struct {
	DiffID image.Digest     `json:"diffId"`
	Blob   image.Descriptor `json:"blob"`
}

// Build runs a build and tags the result.
func (b *Builder) Build(ctx context.Context, o Options) (*Result, error) {
	start := time.Now()
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Network == "" {
		o.Network = "host"
	}
	bc, err := newContext(o.ContextDir)
	if err != nil {
		return nil, err
	}
	file := o.File
	if file == "" {
		file = filepath.Join(bc.root, "Railfile")
		if _, err := os.Stat(file); err != nil {
			file = filepath.Join(bc.root, "Dockerfile")
		}
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	rf, err := Parse(bytes.NewReader(src), o.BuildArgs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	target, err := rf.Target(o.Target)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"cache", "tmp"} {
		if err := os.MkdirAll(filepath.Join(b.Dir, d), 0o755); err != nil {
			return nil, err
		}
	}

	run := &buildRun{b: b, o: o, bc: bc, rf: rf, out: &syncWriter{w: o.Out}, results: map[int]*stageResult{}}
	need := rf.Needed(target)
	for i := range need {
		run.results[i] = &stageResult{done: make(chan struct{})}
	}
	skipped := len(rf.Stages) - len(need)
	if skipped > 0 {
		run.log("skipping %d stage(s) the target does not depend on", skipped)
	}
	// Every needed stage starts at once; each blocks only on its own
	// dependencies, so independent stages build in parallel.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := range need {
		go run.buildStage(ctx, rf.Stages[i], cancel)
	}
	res := run.results[target.Index]
	<-res.done
	if res.err != nil {
		// Wait for siblings so no container outlives the build.
		for _, r := range run.results {
			<-r.done
		}
		return nil, res.err
	}
	st := res.st
	cfg := st.cfg
	cfg.RootFS = image.RootFS{Type: "layers", DiffIDs: st.layers}
	cfg.Architecture, cfg.OS = goruntime.GOARCH, "linux"
	created := time.Now().UTC()
	if o.Epoch != nil {
		created = o.Epoch.UTC()
	}
	cfg.Created = &created
	tag := o.Tag
	if tag == "" {
		tag = "localhost/" + strings.ToLower(filepath.Base(bc.root)) + ":latest"
	}
	im, err := b.Store.SaveImage(tag, cfg, st.blobs)
	if err != nil {
		return nil, err
	}
	for _, r := range run.results {
		<-r.done
	}
	run.log("naming to %s done, image %s", tag, im.Config.Short())
	return &Result{Image: im, Steps: int(run.steps.Load()), Cached: int(run.cached.Load()), Duration: time.Since(start)}, nil
}

type buildRun struct {
	b       *Builder
	o       Options
	bc      *buildContext
	rf      *Railfile
	out     *syncWriter
	results map[int]*stageResult
	seq     atomic.Int64
	steps   atomic.Int64
	cached  atomic.Int64
}

func (r *buildRun) log(format string, args ...any) {
	fmt.Fprintf(r.out, "=> "+format+"\n", args...)
}

func (r *buildRun) buildStage(ctx context.Context, s *Stage, cancel context.CancelFunc) {
	res := r.results[s.Index]
	defer close(res.done)
	for _, d := range s.Deps {
		dr := r.results[d]
		<-dr.done
		if dr.err != nil {
			res.err = dr.err
			return
		}
	}
	st, err := r.runStage(ctx, s)
	if err != nil {
		res.err = err
		cancel()
		return
	}
	res.st = st
}

func (r *buildRun) stageLabel(s *Stage, i int) string {
	name := s.Name
	if name == "" {
		name = "stage-" + strconv.Itoa(s.Index)
	}
	return fmt.Sprintf("[%s %d/%d]", name, i+1, len(s.Steps)+1)
}

func (r *buildRun) runStage(ctx context.Context, s *Stage) (*stageState, error) {
	n := r.seq.Add(1)
	label := r.stageLabel(s, 0)
	t0 := time.Now()
	fmt.Fprintf(r.out, "#%d %s FROM %s\n", n, label, s.Base)
	st, err := r.base(ctx, s)
	if err != nil {
		fmt.Fprintf(r.out, "#%d ERROR %v\n", n, err)
		return nil, err
	}
	fmt.Fprintf(r.out, "#%d DONE %.1fs\n", n, time.Since(t0).Seconds())

	for i, ins := range s.Steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := r.seq.Add(1)
		r.steps.Add(1)
		label := r.stageLabel(s, i+1)
		fmt.Fprintf(r.out, "#%d %s %s\n", n, label, truncate(ins.Raw, 100))
		t0 := time.Now()
		cached, err := r.step(ctx, st, ins, fmt.Sprintf("#%d", n))
		if err != nil {
			fmt.Fprintf(r.out, "#%d ERROR %v\n", n, err)
			return nil, fmt.Errorf("%s %s: %w", label, ins.Cmd, err)
		}
		if cached {
			r.cached.Add(1)
			fmt.Fprintf(r.out, "#%d CACHED\n", n)
		} else {
			fmt.Fprintf(r.out, "#%d DONE %.1fs\n", n, time.Since(t0).Seconds())
		}
	}
	return st, nil
}

// base produces the starting state of a stage: an earlier stage's result,
// scratch, or a (pulled) image.
func (r *buildRun) base(ctx context.Context, s *Stage) (*stageState, error) {
	args := map[string]string{}
	for k, v := range r.rf.GlobalArgs {
		args[k] = v
	}
	for _, d := range s.Deps {
		ds := r.rf.Stages[d]
		if ds.key() == strings.ToLower(s.Base) {
			src := r.results[d].st
			return &stageState{
				layers: append([]image.Digest(nil), src.layers...),
				blobs:  append([]image.Descriptor(nil), src.blobs...),
				cfg:    cloneConfig(src.cfg),
				key:    src.key,
				args:   args,
			}, nil
		}
	}
	if s.Base == "scratch" {
		return &stageState{key: hashKey("scratch"), args: args}, nil
	}
	im, err := r.image(ctx, s.Base)
	if err != nil {
		return nil, err
	}
	cfg, err := r.b.Store.Config(im)
	if err != nil {
		return nil, err
	}
	man, _, err := r.b.Store.Manifest(im)
	if err != nil {
		return nil, err
	}
	cfg.History = nil
	return &stageState{
		layers: append([]image.Digest(nil), im.Layers...),
		blobs:  man.Layers,
		cfg:    cfg,
		key:    hashKey("image", string(im.Manifest)),
		args:   args,
	}, nil
}

func (r *buildRun) image(ctx context.Context, ref string) (image.Image, error) {
	if im, err := r.b.Store.Get(ref); err == nil {
		return im, nil
	}
	return r.b.Store.Pull(ctx, ref, func(f string, a ...any) { fmt.Fprintf(r.out, "   "+f+"\n", a...) })
}

// step applies one instruction. It returns whether the result came from
// the cache.
func (r *buildRun) step(ctx context.Context, st *stageState, ins Instruction, prefix string) (bool, error) {
	vars := map[string]string{}
	for k, v := range st.args {
		vars[k] = v
	}
	for _, kv := range st.cfg.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	arg := ""
	if len(ins.Args) > 0 {
		arg = ins.Args[0]
	}
	c := &st.cfg.Config
	meta := func(what string) {
		st.key = hashKey(st.key, what)
		st.cfg.History = append(st.cfg.History, image.History{CreatedBy: ins.Raw, EmptyLayer: true})
	}
	switch ins.Cmd {
	case "ENV":
		kvs, err := parseKV(expand(arg, vars))
		if err != nil {
			return false, err
		}
		for _, kv := range kvs {
			c.Env = container.MergeEnv(c.Env, []string{kv[0] + "=" + kv[1]})
		}
		meta("ENV " + strings.Join(c.Env, "\x00"))
	case "ARG":
		k, v, hasDefault := strings.Cut(arg, "=")
		if bv, ok := r.o.BuildArgs[k]; ok {
			v = bv
		} else if !hasDefault {
			v = r.rf.GlobalArgs[k]
		}
		st.args[k] = v
		meta("ARG " + k + "=" + v)
	case "WORKDIR":
		wd := expand(arg, vars)
		if !path.IsAbs(wd) {
			base := c.WorkingDir
			if base == "" {
				base = "/"
			}
			wd = path.Join(base, wd)
		}
		c.WorkingDir = path.Clean(wd)
		meta("WORKDIR " + c.WorkingDir)
	case "USER":
		c.User = expand(arg, vars)
		meta("USER " + c.User)
	case "CMD", "ENTRYPOINT":
		v := ins.Args
		if !ins.JSON {
			v = []string{"/bin/sh", "-c", arg}
		}
		if ins.Cmd == "CMD" {
			c.Cmd = v
		} else {
			c.Entrypoint = v
			c.Cmd = nil // Docker resets CMD when ENTRYPOINT is set
		}
		b, _ := json.Marshal(v)
		meta(ins.Cmd + " " + string(b))
	case "EXPOSE":
		if c.ExposedPorts == nil {
			c.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range splitWords(expand(arg, vars)) {
			if !strings.Contains(p, "/") {
				p += "/tcp"
			}
			c.ExposedPorts[p] = struct{}{}
		}
		meta("EXPOSE " + arg)
	case "LABEL":
		kvs, err := parseKV(expand(arg, vars))
		if err != nil {
			return false, err
		}
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		for _, kv := range kvs {
			c.Labels[kv[0]] = kv[1]
		}
		meta("LABEL " + arg)
	case "STOPSIGNAL":
		c.StopSignal = arg
		meta("STOPSIGNAL " + arg)
	case "RUN":
		return r.run(ctx, st, ins, vars, prefix)
	case "COPY", "ADD":
		return r.copy(ctx, st, ins, vars)
	default:
		return false, fmt.Errorf("unsupported instruction %s", ins.Cmd)
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// cache

func hashKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *buildRun) cacheLookup(key string) (cacheEntry, bool) {
	if r.o.NoCache {
		return cacheEntry{}, false
	}
	var e cacheEntry
	b, err := os.ReadFile(filepath.Join(r.b.Dir, "cache", key+".json"))
	if err != nil || json.Unmarshal(b, &e) != nil {
		return e, false
	}
	// A cache record is only good if its layer still exists.
	if !r.b.Store.HasLayer(e.DiffID) || !r.b.Store.HasBlob(e.Blob.Digest) {
		return e, false
	}
	return e, true
}

func (r *buildRun) cacheStore(key string, e cacheEntry) {
	b, _ := json.Marshal(e)
	_ = image.WriteFileAtomic(filepath.Join(r.b.Dir, "cache", key+".json"), b, 0o644)
}

func (st *stageState) addLayer(key, createdBy string, e cacheEntry) {
	st.key = key
	st.layers = append(st.layers, e.DiffID)
	st.blobs = append(st.blobs, e.Blob)
	st.cfg.History = append(st.cfg.History, image.History{CreatedBy: createdBy})
}

func (r *buildRun) layerDirs(st *stageState) []string {
	out := make([]string, len(st.layers))
	for i, d := range st.layers {
		out[i] = r.b.Store.LayerDir(d)
	}
	return out
}

// ---------------------------------------------------------------------------
// RUN

func (r *buildRun) run(ctx context.Context, st *stageState, ins Instruction, vars map[string]string, prefix string) (bool, error) {
	args := ins.Args
	if !ins.JSON {
		args = []string{"/bin/sh", "-c", ins.Args[0]}
	}
	// ARG values are visible to RUN as environment variables but are not
	// persisted in the image config; they are part of the cache key.
	env := append([]string(nil), st.cfg.Config.Env...)
	var argNames []string
	for k := range st.args {
		argNames = append(argNames, k)
	}
	sort.Strings(argNames)
	for _, k := range argNames {
		if !hasEnv(env, k) {
			env = append(env, k+"="+st.args[k])
		}
	}
	argsJSON, _ := json.Marshal(args)
	key := hashKey(st.key, "RUN", string(argsJSON), strings.Join(env, "\x00"), st.cfg.Config.WorkingDir, st.cfg.Config.User, r.o.Network)
	if e, ok := r.cacheLookup(key); ok {
		st.addLayer(key, ins.Raw, e)
		return true, nil
	}
	// Proxy settings are passed through (as Docker does for build args)
	// but never baked into the image or the cache key.
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" && !hasEnv(env, k) {
			env = append(env, k+"="+v)
		}
	}
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	tmp, err := os.MkdirTemp(filepath.Join(r.b.Dir, "tmp"), "run-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	lay := rootfs.NewLayout(tmp)
	if err := rootfs.Mount(r.layerDirs(st), lay); err != nil {
		return false, err
	}
	mounted := true
	defer func() {
		if mounted {
			_ = rootfs.Unmount(lay)
		}
	}()

	uid, gid, groups, err := container.ResolveUser(lay.Merged, st.cfg.Config.User)
	if err != nil {
		return false, err
	}
	if !hasEnv(env, "HOME") {
		// As runc does for docker build: HOME comes from the image's passwd.
		env = append(env, "HOME="+container.HomeFor(lay.Merged, uid))
	}
	sp := &spec.Spec{
		ID:       "build-" + randHex(6),
		RootFS:   lay.Merged,
		Hostname: "rh-build",
		Process: spec.Process{
			Args: args, Env: env, Cwd: st.cfg.Config.WorkingDir,
			UID: uid, GID: gid, AdditionalGIDs: groups,
		},
		Namespaces:   spec.Namespaces{PID: true, UTS: true, IPC: true, Mount: true, Net: r.o.Network == "none"},
		CgroupParent: "roundhouse-build",
	}
	// Give the step DNS without writing resolv.conf into the layer: bind
	// the host's files over ones that already exist in the image.
	var created []string
	if r.o.Network == "host" {
		for _, f := range []string{"/etc/resolv.conf", "/etc/hosts"} {
			p, err := fsutil.SecureJoin(lay.Merged, f)
			if err != nil {
				continue
			}
			if _, err := os.Lstat(p); err != nil {
				created = append(created, f)
			}
			sp.Mounts = append(sp.Mounts, spec.Mount{Source: f, Target: f, ReadOnly: true})
		}
	}
	pw := &prefixWriter{w: r.out, prefix: prefix + " "}
	opt := runtime.Options{Stdout: pw, Stderr: pw}
	if sp.Namespaces.Net {
		opt.BeforeRelease = func(pid int) error { return loopbackUp(pid) }
	}
	ctr, err := runtime.Start(sp, opt)
	if err != nil {
		return false, err
	}
	waitc := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ctr.Cgroup.Kill()
		case <-waitc:
		}
	}()
	code, werr := ctr.Wait()
	close(waitc)
	pw.Flush()
	_ = ctr.Cgroup.Destroy()
	if werr != nil && code < 0 {
		return false, werr
	}
	if code != 0 {
		return false, fmt.Errorf("process %q did not complete successfully: exit code %d", strings.Join(args, " "), code)
	}
	if err := rootfs.Unmount(lay); err != nil {
		return false, err
	}
	mounted = false
	// Remove the empty mount targets we had to create for resolv.conf/hosts.
	for _, f := range created {
		_ = os.Remove(filepath.Join(lay.Upper, f))
	}
	diffID, blob, err := r.b.Store.CommitLayer(lay.Upper, image.LayerOptions{Epoch: r.o.Epoch})
	if err != nil {
		return false, err
	}
	e := cacheEntry{DiffID: diffID, Blob: blob}
	r.cacheStore(key, e)
	st.addLayer(key, ins.Raw, e)
	return false, nil
}

// ---------------------------------------------------------------------------
// COPY

func (r *buildRun) copy(ctx context.Context, st *stageState, ins Instruction, vars map[string]string) (bool, error) {
	var words []string
	if ins.JSON {
		words = ins.Args
	} else {
		words = splitWords(expand(ins.Args[0], vars))
	}
	if len(words) < 2 {
		return false, errors.New("COPY needs at least one source and a destination")
	}
	srcs, dst := words[:len(words)-1], words[len(words)-1]
	if !path.IsAbs(dst) {
		wd := st.cfg.Config.WorkingDir
		if wd == "" {
			wd = "/"
		}
		trailing := strings.HasSuffix(dst, "/")
		dst = path.Join(wd, dst)
		if trailing {
			dst += "/"
		}
	}
	chownSpec := expand(ins.Flags["chown"], vars)
	from := strings.ToLower(ins.Flags["from"])

	// Resolve the source tree: the build context, an earlier stage, or an
	// image.
	var groups [][]source
	var srcKey string
	var cleanup func()
	if from == "" {
		for _, s := range srcs {
			g, err := r.bc.resolve(s)
			if err != nil {
				return false, err
			}
			groups = append(groups, g)
		}
		h, err := hashSources(groups)
		if err != nil {
			return false, err
		}
		srcKey = "context:" + h
	} else {
		layers, key, err := r.fromSource(ctx, from)
		if err != nil {
			return false, err
		}
		root, done, err := r.mountReadOnly(layers)
		if err != nil {
			return false, err
		}
		cleanup = done
		for _, s := range srcs {
			g, err := resolveIn(root, s)
			if err != nil {
				done()
				return false, err
			}
			groups = append(groups, g)
		}
		srcKey = "from:" + key + ":" + strings.Join(srcs, "\x00")
	}
	if cleanup != nil {
		defer cleanup()
	}

	key := hashKey(st.key, "COPY", srcKey, dst, chownSpec)
	if e, ok := r.cacheLookup(key); ok {
		st.addLayer(key, ins.Raw, e)
		return true, nil
	}

	uid, gid, chown := 0, 0, chownSpec != ""
	if chown {
		var err error
		if uid, gid, err = r.resolveChown(st, chownSpec); err != nil {
			return false, err
		}
	}
	tmp, err := os.MkdirTemp(filepath.Join(r.b.Dir, "tmp"), "copy-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	layerDir := filepath.Join(tmp, "fs")
	if err := os.Mkdir(layerDir, 0o755); err != nil {
		return false, err
	}
	lower := r.layerDirs(st)
	if err := mkdirLike(layerDir, lower, "/"); err != nil {
		return false, err
	}

	total := 0
	for _, g := range groups {
		total += len(g)
	}
	single := len(groups) == 1 && len(groups[0]) == 1 && !groups[0][0].fi.IsDir() && !strings.HasSuffix(dst, "/") && !strings.ContainsAny(srcs[0], "*?[")
	for _, g := range groups {
		target := dst
		if single {
			// COPY file.txt /app/renamed.txt
			target = path.Dir(dst)
			g = []source{{abs: g[0].abs, rel: path.Base(dst), fi: g[0].fi}}
		}
		if err := mkdirLike(layerDir, lower, target); err != nil {
			return false, err
		}
		// Parents of nested files inherit ownership from --chown.
		if err := copyInto(g, filepath.Join(layerDir, filepath.FromSlash(target)), uid, gid, chown); err != nil {
			return false, err
		}
	}
	diffID, blob, err := r.b.Store.CommitLayer(layerDir, image.LayerOptions{Epoch: r.o.Epoch})
	if err != nil {
		return false, err
	}
	e := cacheEntry{DiffID: diffID, Blob: blob}
	r.cacheStore(key, e)
	st.addLayer(key, ins.Raw, e)
	return false, nil
}

// fromSource resolves COPY --from: a stage name/index or an image.
func (r *buildRun) fromSource(ctx context.Context, from string) ([]string, string, error) {
	for i, s := range r.rf.Stages {
		if s.key() == from {
			res := r.results[i]
			if res == nil || res.st == nil {
				return nil, "", fmt.Errorf("stage %s was not built", from)
			}
			return r.layerDirs(res.st), res.st.key, nil
		}
	}
	im, err := r.image(ctx, from)
	if err != nil {
		return nil, "", err
	}
	dirs := make([]string, len(im.Layers))
	for i, d := range im.Layers {
		dirs[i] = r.b.Store.LayerDir(d)
	}
	return dirs, string(im.Manifest), nil
}

// mountReadOnly assembles layers into a temporary merged view.
func (r *buildRun) mountReadOnly(layers []string) (string, func(), error) {
	tmp, err := os.MkdirTemp(filepath.Join(r.b.Dir, "tmp"), "view-")
	if err != nil {
		return "", nil, err
	}
	lay := rootfs.NewLayout(tmp)
	if err := rootfs.Mount(layers, lay); err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	return lay.Merged, func() {
		_ = rootfs.Unmount(lay)
		os.RemoveAll(tmp)
	}, nil
}

func (r *buildRun) resolveChown(st *stageState, s string) (int, int, error) {
	u, g, _ := strings.Cut(s, ":")
	if uid, err := strconv.Atoi(u); err == nil {
		gid := uid
		if g != "" {
			if gid, err = strconv.Atoi(g); err != nil {
				goto names
			}
		}
		return uid, gid, nil
	}
names:
	root, done, err := r.mountReadOnly(r.layerDirs(st))
	if err != nil {
		return 0, 0, err
	}
	defer done()
	uid, gid, _, err := container.ResolveUser(root, s)
	return int(uid), int(gid), err
}

// resolveIn is resolve() for a mounted image/stage view.
func resolveIn(root, src string) ([]source, error) {
	c := &buildContext{root: root}
	p, err := fsutil.SecureJoin(root, src)
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, p)
	return c.resolve(rel)
}

// mkdirLike creates dir (an absolute container path) inside layerDir,
// copying each parent's mode and ownership from the layers below. Without
// this, COPY into /tmp would create an upper /tmp with mode 0755 that
// shadows the image's 1777 /tmp.
func mkdirLike(layerDir string, lower []string, dir string) error {
	cur := layerDir
	for _, part := range strings.Split(strings.Trim(path.Clean(dir), "/"), "/") {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if _, err := os.Lstat(cur); err == nil {
			continue
		}
		rel, _ := filepath.Rel(layerDir, cur)
		mode, uid, gid := os.FileMode(0o755), 0, 0
		if fi := lookupInLayers(lower, rel); fi != nil && fi.IsDir() {
			mode = fi.Mode().Perm() | fi.Mode()&(os.ModeSticky|os.ModeSetgid)
			if s, ok := statOwner(fi); ok {
				uid, gid = s[0], s[1]
			}
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(cur, mode); err != nil {
			return err
		}
		_ = os.Lchown(cur, uid, gid)
	}
	return nil
}

// lookupInLayers finds rel in the topmost layer that has it.
func lookupInLayers(lower []string, rel string) os.FileInfo {
	for i := len(lower) - 1; i >= 0; i-- {
		fi, err := os.Lstat(filepath.Join(lower[i], rel))
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeCharDevice != 0 {
			return nil // whiteout: deleted in this layer
		}
		return fi
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers

func cloneConfig(c image.Config) image.Config {
	b, _ := json.Marshal(c)
	var out image.Config
	_ = json.Unmarshal(b, &out)
	return out
}

func hasEnv(env []string, k string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, k+"=") {
			return true
		}
	}
	return false
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// prefixWriter prefixes each line of step output with its step number, so
// interleaved output from parallel stages stays readable.
type prefixWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf[:i])
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

func (p *prefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) > 0 {
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf)
		p.buf = nil
	}
}
