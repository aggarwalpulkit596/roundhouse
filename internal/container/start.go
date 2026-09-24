package container

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aggarwalpulkit596/roundhouse/internal/cgroups"
	"github.com/aggarwalpulkit596/roundhouse/internal/network"
	"github.com/aggarwalpulkit596/roundhouse/internal/rootfs"
	"github.com/aggarwalpulkit596/roundhouse/internal/runtime"
)

// ShimArg is argv[1] of the per-container supervisor process.
const ShimArg = "__shim"

// launch is shared by foreground runs and the shim: make sure the rootfs is
// mounted, wire networking, and start the runtime.
func (m *Manager) launch(rec *Record, opt runtime.Options) (*runtime.Container, error) {
	lay := m.layout(rec.ID)
	if !rootfs.IsMounted(lay.Merged) {
		// e.g. after a host reboot: the upper dir survived, the mount did not.
		if err := rootfs.Mount(rec.Layers, lay); err != nil {
			return nil, err
		}
	}
	// A previous run may have left its cgroup behind.
	_ = cgroups.New(rec.Spec.CgroupParent, rec.ID).Destroy()

	if rec.Network == NetBridge {
		if err := m.Net.Setup(); err != nil {
			return nil, fmt.Errorf("network setup: %w", err)
		}
		ip := net.ParseIP(rec.IP)
		opt.BeforeRelease = func(pid int) error { return m.Net.Attach(rec.ID, pid, ip) }
	} else if rec.Network == NetNone {
		opt.BeforeRelease = func(pid int) error { return network.LoopbackUp(pid) }
	}
	spec := rec.Spec
	return runtime.Start(&spec, opt)
}

// startProxies publishes ports with the userland proxy.
func startProxies(ctx context.Context, rec *Record) ([]*network.Proxy, error) {
	var out []*network.Proxy
	for _, p := range rec.Ports {
		hostIP := p.HostIP
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		px, err := network.Listen(net.JoinHostPort(hostIP, strconv.Itoa(p.HostPort)))
		if err != nil {
			for _, o := range out {
				o.Close()
			}
			return nil, fmt.Errorf("publish %d: %w", p.HostPort, err)
		}
		px.SetBackends([]string{net.JoinHostPort(rec.IP, strconv.Itoa(p.ContainerPort))})
		go px.Serve(ctx)
		out = append(out, px)
	}
	return out, nil
}

// finish records the exit and releases per-run resources.
func (m *Manager) finish(rec *Record, st State, code int, waitErr error) State {
	cg := cgroups.New(rec.Spec.CgroupParent, rec.ID)
	if s, err := cg.Stats(); err == nil && s.OOMKills > 0 {
		st.OOMKilled = true
	}
	// Anything the init left behind (daemonized grandchildren) dies with it.
	if pids, _ := cg.Procs(); len(pids) > 0 {
		_ = cg.Kill()
	}
	_ = cg.Destroy()
	st.Status = StatusExited
	st.ExitCode = code
	st.FinishedAt = time.Now().UTC()
	if waitErr != nil && code < 0 {
		st.Error = waitErr.Error()
	}
	_ = m.writeState(rec.ID, st)
	return st
}

// RunForeground starts a container attached to the caller's stdio and
// blocks until it exits, like `docker run` without -d. Signals are forwarded:
// the first Ctrl-C sends SIGTERM, the second kills the cgroup.
func (m *Manager) RunForeground(ref string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c, err := m.Get(ref)
	if err != nil {
		return -1, err
	}
	if c.State.Status == StatusRunning {
		return -1, fmt.Errorf("container %s is already running", c.Name)
	}
	rec := &c.Record
	ctr, err := m.launch(rec, runtime.Options{Stdin: stdin, Stdout: stdout, Stderr: stderr})
	if err != nil {
		st := State{Status: StatusExited, ExitCode: -1, Error: err.Error(), FinishedAt: time.Now().UTC()}
		_ = m.writeState(rec.ID, st)
		if rec.AutoRm {
			_ = m.Remove(rec.ID, true)
		}
		return -1, err
	}
	ps, _ := ProcStartTime(ctr.Pid)
	st := State{Status: StatusRunning, Pid: ctr.Pid, PidStart: ps, ShimPid: os.Getpid(), ShimStart: selfStart(), StartedAt: time.Now().UTC(), Restarts: c.State.Restarts}
	_ = m.writeState(rec.ID, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxies, err := startProxies(ctx, rec)
	if err != nil {
		_ = ctr.Cgroup.Kill()
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		n := 0
		for s := range sigs {
			n++
			if n == 1 {
				_ = ctr.Signal(s.(syscall.Signal))
				if s == syscall.SIGINT {
					_ = ctr.Signal(syscall.SIGTERM)
				}
				continue
			}
			_ = ctr.Cgroup.Kill()
		}
	}()

	code, werr := ctr.Wait()
	cancel()
	for _, p := range proxies {
		p.Close()
	}
	m.finish(rec, st, code, werr)
	if rec.AutoRm {
		_ = m.Remove(rec.ID, true)
	}
	return code, err
}

// StartDetached starts a container under a shim and returns once it is
// running. The shim is a separate, session-leader process: the CLI or the
// daemon can exit or crash and the container keeps running, supervised.
// This is exactly why containerd has containerd-shim.
func (m *Manager) StartDetached(ref string) (*Info, error) {
	c, err := m.Get(ref)
	if err != nil {
		return nil, err
	}
	if c.State.Status == StatusRunning {
		return c, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	shimLog, err := os.OpenFile(filepath.Join(m.Dir(c.ID), "shim.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.Close()
		return nil, err
	}
	defer shimLog.Close()
	cmd := exec.Command("/proc/self/exe", ShimArg, m.Root, c.ID)
	cmd.Args[0] = "rh-shim"
	cmd.Stdout, cmd.Stderr = shimLog, shimLog
	cmd.ExtraFiles = []*os.File{w} // fd 3: readiness
	cmd.Env = append(os.Environ(), "RH_SHIM=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, err
	}
	w.Close()
	// Reap the shim if this process outlives it; otherwise init adopts it.
	go func() { _ = cmd.Wait() }()

	msg, _ := io.ReadAll(r)
	if s := strings.TrimSpace(string(msg)); s != "ok" {
		if s == "" {
			s = "shim exited before reporting (see " + filepath.Join(m.Dir(c.ID), "shim.log") + ")"
		}
		return nil, errors.New(s)
	}
	return m.Get(c.ID)
}

// ShimMain supervises one container for its whole life. It holds the
// container's stdio pipes, writes logs, publishes ports, reaps the init
// process, and records its exit status.
func ShimMain(root, id string) {
	ready := os.NewFile(3, "ready")
	fail := func(err error) {
		fmt.Fprintln(ready, err)
		ready.Close()
		fmt.Fprintln(os.Stderr, "shim:", err)
		os.Exit(1)
	}
	// The shim ignores terminal and termination signals: stopping a
	// container is done by signalling the container, not its supervisor.
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGPIPE)
	// Orphaned descendants of the container get re-parented to us, not to
	// host init, so we can reap them.
	_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)

	m, err := NewManager(root)
	if err != nil {
		fail(err)
	}
	var rec Record
	if err := readJSON(filepath.Join(m.Dir(id), "container.json"), &rec); err != nil {
		fail(err)
	}
	prev, _ := m.ReadState(id)

	logw, err := newLogWriter(m.LogPath(id))
	if err != nil {
		fail(err)
	}
	defer logw.Close()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	devnull, _ := os.Open(os.DevNull)

	ctr, err := m.launch(&rec, runtime.Options{Stdin: devnull, Stdout: outW, Stderr: errW, Setsid: true})
	outW.Close()
	errW.Close()
	if err != nil {
		st := State{Status: StatusExited, ExitCode: -1, Error: err.Error(), FinishedAt: time.Now().UTC(), Restarts: prev.Restarts}
		_ = m.writeState(id, st)
		fail(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); logw.Copy("stdout", outR) }()
	go func() { defer wg.Done(); logw.Copy("stderr", errR) }()

	ps, _ := ProcStartTime(ctr.Pid)
	st := State{Status: StatusRunning, Pid: ctr.Pid, PidStart: ps, ShimPid: os.Getpid(), ShimStart: selfStart(), StartedAt: time.Now().UTC(), Restarts: prev.Restarts}
	if err := m.writeState(id, st); err != nil {
		_ = ctr.Cgroup.Kill()
		fail(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxies, err := startProxies(ctx, &rec)
	if err != nil {
		_ = ctr.Cgroup.Kill()
		_, _ = ctr.Wait()
		m.finish(&rec, st, -1, err)
		cancel()
		fail(err)
	}
	fmt.Fprintln(ready, "ok")
	ready.Close()

	code, werr := ctr.Wait()
	// Reap any other children that were re-parented to us.
	for {
		var ws unix.WaitStatus
		if pid, _ := unix.Wait4(-1, &ws, unix.WNOHANG, nil); pid <= 0 {
			break
		}
	}
	cancel()
	for _, p := range proxies {
		p.Close()
	}
	wg.Wait()
	m.finish(&rec, st, code, werr)
	if rec.AutoRm {
		_ = m.Remove(id, true)
	}
}

// ---------------------------------------------------------------------------
// logs

// LogLine is one line of container output.
type LogLine struct {
	Time   time.Time `json:"t"`
	Stream string    `json:"s"`
	Line   string    `json:"l"`
}

// maxLogBytes triggers rotation to container.log.1.
const maxLogBytes = 10 << 20

type logWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func newLogWriter(path string) (*logWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fi, _ := f.Stat()
	return &logWriter{path: path, f: f, size: fi.Size()}, nil
}

func (l *logWriter) Copy(stream string, r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			l.write(LogLine{Time: time.Now().UTC(), Stream: stream, Line: strings.TrimSuffix(line, "\n")})
		}
		if err != nil {
			return
		}
	}
}

func (l *logWriter) write(ll LogLine) {
	b, _ := json.Marshal(ll)
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(b)) > maxLogBytes {
		l.f.Close()
		_ = os.Rename(l.path, l.path+".1")
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return
		}
		l.f, l.size = f, 0
	}
	n, _ := l.f.Write(b)
	l.size += int64(n)
}

func (l *logWriter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// ReadLogs streams log lines to fn: the last tail lines (all when tail <= 0),
// then, with follow, new lines as they are written until the container
// exits or ctx is done.
func (m *Manager) ReadLogs(ctx context.Context, id string, follow bool, tail int, fn func(LogLine) error) error {
	f, err := os.Open(m.LogPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)

	var backlog []LogLine
	var partial []byte
	next := func() (LogLine, bool) {
		for {
			b, err := br.ReadBytes('\n')
			partial = append(partial, b...)
			if err != nil {
				return LogLine{}, false // incomplete line stays in partial
			}
			var ll LogLine
			ok := json.Unmarshal(partial, &ll) == nil
			partial = partial[:0]
			if ok {
				return ll, true
			}
		}
	}
	for {
		ll, ok := next()
		if !ok {
			break
		}
		backlog = append(backlog, ll)
		if tail > 0 && len(backlog) > tail {
			backlog = backlog[1:]
		}
	}
	for _, ll := range backlog {
		if err := fn(ll); err != nil {
			return err
		}
	}
	for follow {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
		st, serr := m.ReadState(id)
		for {
			ll, ok := next()
			if !ok {
				break
			}
			if err := fn(ll); err != nil {
				return err
			}
		}
		if serr != nil || st.Status != StatusRunning {
			return nil
		}
	}
	return nil
}

func selfStart() uint64 {
	st, _ := ProcStartTime(os.Getpid())
	return st
}
