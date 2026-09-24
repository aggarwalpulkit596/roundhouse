package web

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aggarwalpulkit596/roundhouse/internal/builder"
	"github.com/aggarwalpulkit596/roundhouse/internal/engine"
)

// Build statuses.
const (
	BuildQueued    = "QUEUED"
	BuildCloning   = "CLONING"
	BuildBuilding  = "BUILDING"
	BuildDeploying = "DEPLOYING"
	BuildDone      = "SUCCESS"
	BuildFailed    = "FAILED"
)

// BuildRequest is "build this source and deploy it as this service": what
// happens on Railway when you connect a repository.
type BuildRequest struct {
	// Source is a git URL (https://…, git@…) or an absolute directory on
	// the daemon's machine.
	Source string `json:"source"`
	// Ref is a branch or tag (git sources only). Empty means the default.
	Ref string `json:"ref,omitempty"`
	// Dir is the build context inside the source ("examples/hello").
	Dir string `json:"dir,omitempty"`
	// File is the Railfile/Dockerfile path relative to Dir.
	File string `json:"file,omitempty"`
	// Spec is the service to deploy; Image is filled in by the build.
	Spec engine.ServiceSpec `json:"spec"`
}

// Build is one build and its outcome.
type Build struct {
	ID         string    `json:"id"`
	Service    string    `json:"service"`
	Source     string    `json:"source"`
	Ref        string    `json:"ref,omitempty"`
	Dir        string    `json:"dir,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Image      string    `json:"image,omitempty"`
	Deployment string    `json:"deployment,omitempty"`
	Steps      int       `json:"steps,omitempty"`
	Cached     int       `json:"cached,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	log *logBuffer
}

// Builds runs builds and hands the results to the engine.
type Builds struct {
	Builder *builder.Builder
	Engine  *engine.Engine
	// WorkDir holds checkouts.
	WorkDir string

	mu     sync.Mutex
	builds []*Build
	slots  chan struct{} // bounds concurrent builds
}

// NewBuilds returns a build service allowing two builds at a time.
func NewBuilds(b *builder.Builder, e *engine.Engine, workDir string) *Builds {
	return &Builds{Builder: b, Engine: e, WorkDir: workDir, slots: make(chan struct{}, 2)}
}

var gitURLRE = regexp.MustCompile(`^(https://|git@|ssh://)[^\s]+$`)

// Start validates a request and begins the build in the background.
func (s *Builds) Start(req BuildRequest) (*Build, error) {
	spec := req.Spec
	spec.Image = "placeholder" // validated below with the real tag
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	src := strings.TrimSpace(req.Source)
	if src == "" {
		return nil, errors.New("source is required: a git URL or an absolute directory")
	}
	if !gitURLRE.MatchString(src) {
		if !filepath.IsAbs(src) {
			return nil, errors.New("source must be a git URL (https://…) or an absolute directory path")
		}
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("source directory %s does not exist on the daemon's machine", src)
		}
	}
	if strings.Contains(req.Dir, "..") || strings.Contains(req.File, "..") {
		return nil, errors.New("dir and file must stay inside the source")
	}
	if req.Ref != "" && strings.HasPrefix(req.Ref, "-") {
		return nil, errors.New("invalid ref")
	}
	b := &Build{
		ID: newID(), Service: spec.Name, Source: src, Ref: req.Ref, Dir: req.Dir,
		Status: BuildQueued, CreatedAt: time.Now().UTC(), log: newLogBuffer(),
	}
	s.mu.Lock()
	s.builds = append(s.builds, b)
	if len(s.builds) > 50 {
		s.builds = s.builds[len(s.builds)-50:]
	}
	s.mu.Unlock()
	go s.run(b, req)
	return b, nil
}

func (s *Builds) set(b *Build, fn func(*Build)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(b)
}

func (s *Builds) run(b *Build, req BuildRequest) {
	fail := func(err error) {
		b.log.Printf("error: %v", err)
		s.set(b, func(b *Build) { b.Status, b.Error, b.FinishedAt = BuildFailed, err.Error(), time.Now().UTC() })
		b.log.Close()
	}
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	root := b.Source
	if gitURLRE.MatchString(b.Source) {
		s.set(b, func(b *Build) { b.Status = BuildCloning })
		dir := filepath.Join(s.WorkDir, b.ID)
		defer os.RemoveAll(dir)
		args := []string{"clone", "--depth", "1"}
		if b.Ref != "" {
			args = append(args, "--branch", b.Ref)
		}
		args = append(args, "--", b.Source, dir)
		b.log.Printf("$ git %s", strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // never hang on a password prompt
		cmd.Stdout, cmd.Stderr = b.log, b.log
		if err := cmd.Run(); err != nil {
			fail(fmt.Errorf("git clone failed (is the repository public, and the URL right?): %w", err))
			return
		}
		if out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output(); err == nil {
			commit := strings.TrimSpace(string(out))
			s.set(b, func(b *Build) { b.Commit = commit })
			b.log.Printf("checked out %s", commit)
		}
		root = dir
	}
	ctxDir := filepath.Join(root, filepath.FromSlash(b.Dir))
	if fi, err := os.Stat(ctxDir); err != nil || !fi.IsDir() {
		fail(fmt.Errorf("directory %q not found in the source", b.Dir))
		return
	}
	file := ""
	if req.File != "" {
		file = filepath.Join(ctxDir, filepath.FromSlash(req.File))
	} else if !exists(filepath.Join(ctxDir, "Railfile")) && !exists(filepath.Join(ctxDir, "Dockerfile")) {
		fail(errors.New("no Railfile or Dockerfile found; add one, or set the file path"))
		return
	}

	tag := fmt.Sprintf("localhost/%s:%s", b.Service, b.ID[:8])
	s.set(b, func(b *Build) { b.Status, b.Image = BuildBuilding, tag })
	res, err := s.Builder.Build(ctx, builder.Options{ContextDir: ctxDir, File: file, Tag: tag, Out: b.log})
	if err != nil {
		fail(err)
		return
	}
	s.set(b, func(b *Build) { b.Steps, b.Cached = res.Steps, res.Cached })
	b.log.Printf("built %s in %s (%d steps, %d cached)", tag, res.Duration.Round(time.Millisecond), res.Steps, res.Cached)

	s.set(b, func(b *Build) { b.Status = BuildDeploying })
	spec := req.Spec
	spec.Image = tag
	d, _, err := s.Engine.Apply(spec)
	if err != nil {
		fail(fmt.Errorf("deploy: %w", err))
		return
	}
	b.log.Printf("deployment rev %d queued; follow it in the service's Deployments tab", d.Revision)
	s.set(b, func(b *Build) {
		b.Status, b.Deployment, b.FinishedAt = BuildDone, d.ID, time.Now().UTC()
	})
	b.log.Close()
}

// List returns builds, newest first, optionally for one service.
func (s *Builds) List(service string) []Build {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Build
	for _, b := range s.builds {
		if service == "" || b.Service == service {
			cp := *b
			cp.log = nil
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Builds) get(id string) *Build {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.builds {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// Routes registers the build API on mux.
//
//	POST /v1/builds                 start (BuildRequest) → 202 Build
//	GET  /v1/builds?service=S       list
//	GET  /v1/builds/{id}            one build
//	GET  /v1/builds/{id}/logs       ?follow=1, plain text lines
func (s *Builds) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/builds", func(w http.ResponseWriter, r *http.Request) {
		var req BuildRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid build request: %w", err))
			return
		}
		b, err := s.Start(req)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeJSON(w, http.StatusAccepted, b)
	})
	mux.HandleFunc("GET /v1/builds", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.List(r.URL.Query().Get("service")))
	})
	mux.HandleFunc("GET /v1/builds/{id}", func(w http.ResponseWriter, r *http.Request) {
		b := s.get(r.PathValue("id"))
		if b == nil {
			writeErr(w, http.StatusNotFound, errors.New("no such build"))
			return
		}
		s.mu.Lock()
		cp := *b
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, cp)
	})
	mux.HandleFunc("GET /v1/builds/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		b := s.get(r.PathValue("id"))
		if b == nil {
			writeErr(w, http.StatusNotFound, errors.New("no such build"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		b.log.Stream(r.Context(), w, r.URL.Query().Get("follow") == "1")
	})
}

// ---------------------------------------------------------------------------

// logBuffer collects build output and lets readers follow it.
type logBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	lines  []string
	part   []byte
	closed bool
}

func newLogBuffer() *logBuffer {
	l := &logBuffer{}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.part = append(l.part, p...)
	for {
		i := strings.IndexByte(string(l.part), '\n')
		if i < 0 {
			break
		}
		l.lines = append(l.lines, string(l.part[:i]))
		l.part = l.part[i+1:]
	}
	if len(l.lines) > 20000 {
		l.lines = l.lines[len(l.lines)-20000:]
	}
	l.cond.Broadcast()
	return len(p), nil
}

func (l *logBuffer) Printf(format string, a ...any) {
	fmt.Fprintf(l, "=> "+format+"\n", a...)
}

func (l *logBuffer) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.part) > 0 {
		l.lines = append(l.lines, string(l.part))
		l.part = nil
	}
	l.closed = true
	l.cond.Broadcast()
}

// Stream writes lines to w, then keeps following until the build ends or
// ctx is cancelled.
func (l *logBuffer) Stream(ctx context.Context, w io.Writer, follow bool) {
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriter(w)
	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
	})
	defer stop()
	next := 0
	for {
		l.mu.Lock()
		for follow && next >= len(l.lines) && !l.closed && ctx.Err() == nil {
			l.cond.Wait()
		}
		batch := append([]string(nil), l.lines[min(next, len(l.lines)):]...)
		next = len(l.lines)
		done := l.closed || !follow || ctx.Err() != nil
		l.mu.Unlock()
		for _, line := range batch {
			bw.WriteString(line)
			bw.WriteByte('\n')
		}
		bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
		if done {
			return
		}
	}
}

// ---------------------------------------------------------------------------

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
