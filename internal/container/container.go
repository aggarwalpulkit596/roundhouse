// Package container is Roundhouse's containerd: it turns "run image X with
// these settings" into a container record, a root filesystem, a network
// attachment and a supervised process, and it keeps enough state on disk
// that any other process (the CLI, the daemon after a restart) can find and
// manage the container again.
//
// On-disk layout, one directory per container:
//
//	<root>/containers/<id>/
//	  container.json   immutable record: what was asked for (the "spec")
//	  state.json       mutable status written by the shim
//	  container.log    JSON-lines stdout/stderr (detached containers)
//	  upper/ work/     overlayfs writable layer
//	  rootfs/          overlay mount point
package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aggarwalpulkit596/roundhouse/internal/cgroups"
	"github.com/aggarwalpulkit596/roundhouse/internal/image"
	"github.com/aggarwalpulkit596/roundhouse/internal/network"
	"github.com/aggarwalpulkit596/roundhouse/internal/rootfs"
	"github.com/aggarwalpulkit596/roundhouse/internal/spec"
)

// Network modes.
const (
	NetBridge = "bridge" // own netns, veth to rh0, NAT to the outside
	NetHost   = "host"   // share the host's network stack
	NetNone   = "none"   // own netns with only loopback
)

// Status values.
const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusExited  = "exited"
)

// PortMapping publishes a container port on the host.
type PortMapping struct {
	HostIP        string `json:"hostIp,omitempty"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
}

// Record is the immutable description of a container.
type Record struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Image    string            `json:"image"`
	ImageID  image.Digest      `json:"imageId"`
	Created  time.Time         `json:"created"`
	Spec     spec.Spec         `json:"spec"`
	Layers   []string          `json:"layers"` // lowerdirs, bottom first
	Network  string            `json:"network"`
	IP       string            `json:"ip,omitempty"`
	Ports    []PortMapping     `json:"ports,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	AutoRm   bool              `json:"autoRemove,omitempty"`
	StopWait time.Duration     `json:"stopTimeout,omitempty"`
}

// State is the live status, owned by whoever supervises the process.
type State struct {
	Status     string    `json:"status"`
	Pid        int       `json:"pid,omitempty"`
	PidStart   uint64    `json:"pidStart,omitempty"` // guards against pid reuse
	ShimPid    int       `json:"shimPid,omitempty"`
	ShimStart  uint64    `json:"shimStart,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	ExitCode   int       `json:"exitCode"`
	OOMKilled  bool      `json:"oomKilled,omitempty"`
	Error      string    `json:"error,omitempty"`
	Restarts   int       `json:"restarts,omitempty"`
}

// Info is a record plus its state.
type Info struct {
	Record
	State State `json:"state"`
}

// Manager creates and supervises containers.
type Manager struct {
	Root   string
	Images *image.Store
	Net    *network.Manager
}

// NewManager wires the image store and network manager under root
// (default /var/lib/roundhouse).
func NewManager(root string) (*Manager, error) {
	for _, d := range []string{"containers", "volumes"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	st, err := image.NewStore(filepath.Join(root, "images"))
	if err != nil {
		return nil, err
	}
	nm, err := network.New(filepath.Join(root, "network"), os.Getenv("RH_BRIDGE"), os.Getenv("RH_SUBNET"))
	if err != nil {
		return nil, err
	}
	return &Manager{Root: root, Images: st, Net: nm}, nil
}

// Dir is the container's state directory.
func (m *Manager) Dir(id string) string { return filepath.Join(m.Root, "containers", id) }

func (m *Manager) layout(id string) rootfs.Layout { return rootfs.NewLayout(m.Dir(id)) }

// CreateOptions is what a user asks for (`rh run` flags / a service spec).
type CreateOptions struct {
	Name       string
	Image      string
	Cmd        []string // replaces the image's Cmd
	Entrypoint []string // replaces the image's Entrypoint when non-nil
	Env        []string
	Workdir    string
	User       string
	Hostname   string
	Resources  spec.Resources
	Network    string
	Ports      []PortMapping
	Volumes    []spec.Mount
	Init       bool
	ReadOnly   bool
	Labels     map[string]string
	DNS        []string
	ExtraHosts map[string]string
	AutoRemove bool
	StopWait   time.Duration
}

// Create resolves the image, builds the root filesystem and writes the
// record. The container is not started.
func (m *Manager) Create(o CreateOptions) (*Info, error) {
	im, err := m.Images.Get(o.Image)
	if err != nil {
		return nil, err
	}
	cfg, err := m.Images.Config(im)
	if err != nil {
		return nil, err
	}
	if o.Network == "" {
		o.Network = NetBridge
	}
	switch o.Network {
	case NetBridge, NetHost, NetNone:
	default:
		return nil, fmt.Errorf("unknown network mode %q (bridge, host, none)", o.Network)
	}
	if o.Network != NetBridge && len(o.Ports) > 0 {
		return nil, fmt.Errorf("port publishing needs --net bridge")
	}

	id := newID()
	if o.Name == "" {
		o.Name = id[:12]
	}
	if err := validName(o.Name); err != nil {
		return nil, err
	}
	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := m.Get(o.Name); err == nil {
		return nil, fmt.Errorf("container name %q is already in use", o.Name)
	}

	layers := make([]string, len(im.Layers))
	for i, d := range im.Layers {
		layers[i] = m.Images.LayerDir(d)
	}
	dir := m.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fail := func(err error) (*Info, error) {
		_ = rootfs.Unmount(m.layout(id))
		_ = os.RemoveAll(dir)
		return nil, err
	}
	lay := m.layout(id)
	if err := rootfs.Mount(layers, lay); err != nil {
		return fail(err)
	}

	args := cfg.Config.Entrypoint
	if o.Entrypoint != nil {
		args = o.Entrypoint
	}
	cmd := cfg.Config.Cmd
	if len(o.Cmd) > 0 || o.Entrypoint != nil {
		cmd = o.Cmd
	}
	args = append(append([]string{}, args...), cmd...)
	if len(args) == 0 {
		return fail(errors.New("no command specified and the image has no default"))
	}

	hostname := o.Hostname
	if hostname == "" {
		hostname = id[:12]
	}
	user := o.User
	if user == "" {
		user = cfg.Config.User
	}
	uid, gid, groups, err := ResolveUser(lay.Merged, user)
	if err != nil {
		return fail(err)
	}
	workdir := o.Workdir
	if workdir == "" {
		workdir = cfg.Config.WorkingDir
	}
	env := MergeEnv(cfg.Config.Env, o.Env)
	env = MergeEnv(env, []string{"HOSTNAME=" + hostname})
	if !hasKey(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !hasKey(env, "HOME") {
		env = append(env, "HOME="+homeFor(lay.Merged, uid))
	}

	rec := &Record{
		ID:       id,
		Name:     o.Name,
		Image:    im.Name,
		ImageID:  im.Config,
		Created:  time.Now().UTC(),
		Layers:   layers,
		Network:  o.Network,
		Ports:    o.Ports,
		Labels:   o.Labels,
		AutoRm:   o.AutoRemove,
		StopWait: o.StopWait,
		Spec: spec.Spec{
			ID:       id,
			RootFS:   lay.Merged,
			Hostname: hostname,
			Process: spec.Process{
				Args:            args,
				Env:             env,
				Cwd:             workdir,
				UID:             uid,
				GID:             gid,
				AdditionalGIDs:  groups,
				NoNewPrivileges: true,
				Init:            o.Init,
			},
			Namespaces:     spec.AllNamespaces(),
			Resources:      o.Resources,
			Mounts:         o.Volumes,
			ReadonlyRootfs: o.ReadOnly,
			CgroupParent:   "roundhouse",
		},
	}
	if o.Network == NetHost {
		rec.Spec.Namespaces.Net = false
		rec.Spec.Namespaces.UTS = false
		rec.Spec.Hostname = ""
	}
	if o.Network == NetBridge {
		ip, err := m.Net.Allocate(id)
		if err != nil {
			return fail(err)
		}
		rec.IP = ip.String()
	}
	if err := writeEtc(lay.Merged, rec, o.DNS, o.ExtraHosts); err != nil {
		_ = m.Net.Release(id)
		return fail(err)
	}
	if err := writeJSON(filepath.Join(dir, "container.json"), rec); err != nil {
		_ = m.Net.Release(id)
		return fail(err)
	}
	st := State{Status: StatusCreated}
	if err := m.writeState(id, st); err != nil {
		_ = m.Net.Release(id)
		return fail(err)
	}
	return &Info{Record: *rec, State: st}, nil
}

// Get finds a container by full ID, unique ID prefix or name.
func (m *Manager) Get(ref string) (*Info, error) {
	if ref == "" {
		return nil, errors.New("empty container reference")
	}
	all, err := m.List()
	if err != nil {
		return nil, err
	}
	var match *Info
	for i := range all {
		c := &all[i]
		if c.ID == ref || c.Name == ref {
			return c, nil
		}
		if len(ref) >= 3 && strings.HasPrefix(c.ID, ref) {
			if match != nil {
				return nil, fmt.Errorf("container prefix %q is ambiguous", ref)
			}
			match = c
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no such container: %s", ref)
	}
	return match, nil
}

// List returns every container, newest first. A container recorded as
// running whose process is gone (e.g. its shim was SIGKILLed) is reported
// as exited.
func (m *Manager) List() ([]Info, error) {
	ents, err := os.ReadDir(filepath.Join(m.Root, "containers"))
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		var rec Record
		if err := readJSON(filepath.Join(m.Dir(e.Name()), "container.json"), &rec); err != nil {
			continue // being created or removed
		}
		st, _ := m.ReadState(rec.ID)
		if st.Status == StatusRunning && !Alive(st.Pid, st.PidStart) {
			// Re-read: the supervisor may have written the exit a moment ago.
			if st2, err := m.ReadState(rec.ID); err == nil && st2.Status != StatusRunning {
				st = st2
			} else if st.ShimPid > 0 && st.ShimPid != os.Getpid() && Alive(st.ShimPid, st.ShimStart) {
				// The init is gone but its shim is still recording the exit;
				// report it as still running for this brief window rather
				// than inventing an exit code.
			} else {
				st.Status = StatusExited
				st.ExitCode = -1
				st.Error = "process disappeared without a recorded exit (supervisor lost)"
			}
		}
		out = append(out, Info{Record: rec, State: st})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// ReadState loads state.json.
func (m *Manager) ReadState(id string) (State, error) {
	var st State
	err := readJSON(filepath.Join(m.Dir(id), "state.json"), &st)
	return st, err
}

func (m *Manager) writeState(id string, st State) error {
	return writeJSON(filepath.Join(m.Dir(id), "state.json"), st)
}

// Signal delivers sig to the container's init process.
func (m *Manager) Signal(ref string, sig syscall.Signal) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	if c.State.Status != StatusRunning {
		return fmt.Errorf("container %s is not running", c.Name)
	}
	return syscall.Kill(c.State.Pid, sig)
}

// Stop sends SIGTERM (or the image's stop signal), waits up to timeout, then
// kills everything in the cgroup. This is the graceful-shutdown contract
// every orchestrator implements (Kubernetes terminationGracePeriodSeconds).
func (m *Manager) Stop(ref string, timeout time.Duration) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	if c.State.Status != StatusRunning {
		return nil
	}
	if timeout <= 0 {
		timeout = c.StopWait
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	_ = syscall.Kill(c.State.Pid, syscall.SIGTERM)
	if m.waitExit(c.ID, timeout) {
		return nil
	}
	cg := cgroups.New(c.Spec.CgroupParent, c.ID)
	if err := cg.Kill(); err != nil {
		_ = syscall.Kill(c.State.Pid, syscall.SIGKILL)
	}
	if !m.waitExit(c.ID, 5*time.Second) {
		return fmt.Errorf("container %s did not exit after SIGKILL", c.Name)
	}
	return nil
}

// waitExit polls until the supervisor has recorded the exit.
func (m *Manager) waitExit(id string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		st, err := m.ReadState(id)
		if err != nil || st.Status != StatusRunning {
			return true
		}
		if !Alive(st.Pid, st.PidStart) {
			// Give the supervisor a moment to write the final state.
			time.Sleep(100 * time.Millisecond)
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Remove deletes a container. Running containers need force.
func (m *Manager) Remove(ref string, force bool) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	if c.State.Status == StatusRunning {
		if !force {
			return fmt.Errorf("container %s is running (stop it or use -f)", c.Name)
		}
		if err := m.Stop(c.ID, time.Second); err != nil {
			return err
		}
	}
	m.cleanupRuntime(c)
	if c.Network == NetBridge {
		m.Net.Detach(c.ID)
		_ = m.Net.Release(c.ID)
	}
	if err := rootfs.Unmount(m.layout(c.ID)); err != nil {
		return err
	}
	return os.RemoveAll(m.Dir(c.ID))
}

// cleanupRuntime removes leftovers of a finished process: its cgroup.
func (m *Manager) cleanupRuntime(c *Info) {
	cg := cgroups.New(c.Spec.CgroupParent, c.ID)
	if pids, err := cg.Procs(); err == nil && len(pids) > 0 {
		_ = cg.Kill()
		time.Sleep(50 * time.Millisecond)
	}
	_ = cg.Destroy()
}

// Stats reads the container's cgroup counters.
func (m *Manager) Stats(ref string) (cgroups.Stats, error) {
	c, err := m.Get(ref)
	if err != nil {
		return cgroups.Stats{}, err
	}
	return cgroups.New(c.Spec.CgroupParent, c.ID).Stats()
}

// LogPath is the JSON-lines log of a detached container.
func (m *Manager) LogPath(id string) string { return filepath.Join(m.Dir(id), "container.log") }

func (m *Manager) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(m.Root, "containers", ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// ---------------------------------------------------------------------------
// process identity

// Alive reports whether pid exists and is the same process that was started
// (its start time matches). Pids are recycled; a bare kill(pid, 0) check
// after a reboot or long uptime can match an unrelated process.
func Alive(pid int, start uint64) bool {
	if pid <= 0 {
		return false
	}
	st, err := ProcStartTime(pid)
	if err != nil {
		return false
	}
	if start != 0 && st != start {
		return false
	}
	// A zombie still has a /proc entry; treat it as dead.
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && len(s) > i+2 {
		return s[i+2] != 'Z' && s[i+2] != 'X'
	}
	return true
}

// ProcStartTime reads field 22 of /proc/<pid>/stat (start time in clock
// ticks since boot).
func ProcStartTime(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	// The command name (field 2) may contain spaces and ')' — skip past the
	// last ')'.
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0, errors.New("malformed stat")
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// ---------------------------------------------------------------------------
// helpers

func newID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func validName(n string) error {
	if len(n) > 63 {
		return fmt.Errorf("name %q is too long", n)
	}
	for i, r := range n {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || (i > 0 && (r == '-' || r == '_' || r == '.'))
		if !ok {
			return fmt.Errorf("invalid name %q: use letters, digits, '-', '_', '.'", n)
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return image.WriteFileAtomic(path, b, 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// MergeEnv overlays KEY=VALUE pairs from over onto base.
func MergeEnv(base, over []string) []string {
	out := append([]string{}, base...)
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, e := range out {
			if ek, _, _ := strings.Cut(e, "="); ek == k {
				out[i] = kv
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

func hasKey(env []string, k string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, k+"=") {
			return true
		}
	}
	return false
}

// BumpRestarts increments the restart counter of an exited container, so
// restart policies can compute backoff from observed state alone.
func (m *Manager) BumpRestarts(ref string) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	st := c.State
	if st.Status == StatusRunning {
		return fmt.Errorf("container %s is running", c.Name)
	}
	st.Restarts++
	return m.writeState(c.ID, st)
}
