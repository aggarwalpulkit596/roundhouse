package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/spec"
)

// Namespaces, capabilities and prctl flags are per *thread* in Linux. Pin the
// main goroutine to the main OS thread so every step of init, and the final
// execve, happens on one thread. This must run in an init() so it happens
// before main starts scheduling goroutines elsewhere.
func init() {
	if len(os.Args) > 1 && (os.Args[1] == InitArg || os.Args[1] == ExecArg) {
		goruntime.LockOSThread()
	}
}

// Init is the body of the container's first process. It never returns on
// success: it either execs the user program or, in init mode, exits with the
// user program's status.
func Init() {
	cfg := os.NewFile(3, "sync-cfg")
	errPipe := os.NewFile(4, "sync-err")
	// A successful execve will close the error pipe; that EOF is the parent's
	// signal that the container is running.
	unix.CloseOnExec(4)

	report := func(err error) {
		fmt.Fprintf(errPipe, "%v", err)
		errPipe.Close()
		os.Exit(1)
	}

	var s spec.Spec
	if err := json.NewDecoder(cfg).Decode(&s); err != nil {
		report(fmt.Errorf("read spec: %w", err))
	}
	cfg.Close()

	if err := prepare(&s); err != nil {
		report(err)
	}

	path, err := lookPath(s.Process.Args[0], s.Process.Env)
	if err != nil {
		report(err)
	}

	if s.Process.Init {
		runInit(&s, path, errPipe)
		return
	}
	// execve replaces this process image. From here on the user program is
	// PID 1 of the container.
	if err := unix.Exec(path, s.Process.Args, s.Process.Env); err != nil {
		report(fmt.Errorf("exec %s: %w", s.Process.Args[0], err))
	}
}

// prepare does everything between "born in new namespaces" and exec.
func prepare(s *spec.Spec) error {
	if s.Namespaces.Cgroup {
		// We are already inside our cgroup, so the new cgroup namespace is
		// rooted there and /proc/self/cgroup reads "0::/".
		if err := unix.Unshare(unix.CLONE_NEWCGROUP); err != nil {
			return fmt.Errorf("unshare cgroupns: %w", err)
		}
	}
	if s.Namespaces.UTS && s.Hostname != "" {
		if err := unix.Sethostname([]byte(s.Hostname)); err != nil {
			return fmt.Errorf("sethostname: %w", err)
		}
	}
	if s.Namespaces.Mount {
		if err := setupRootfs(s); err != nil {
			return err
		}
	} else if err := unix.Chroot(s.RootFS); err != nil {
		// Without a mount namespace we can only chroot, which is escapable
		// by a root process (see docs/02-filesystems.md).
		return fmt.Errorf("chroot: %w", err)
	}
	if err := setRlimits(s.Process.Rlimits); err != nil {
		return err
	}
	if err := applyCredentials(s.Process); err != nil {
		return err
	}
	cwd := s.Process.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("mkdir cwd: %w", err)
	}
	if err := unix.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cwd, err)
	}
	return nil
}

// applyCredentials drops privileges in the order that keeps it correct:
//  1. shrink the bounding set (limits what any future exec can gain),
//  2. keep capabilities across the uid change,
//  3. setgroups/setgid/setuid,
//  4. set effective/permitted/inheritable to exactly the kept set,
//  5. no_new_privs so setuid binaries cannot climb back up.
func applyCredentials(p spec.Process) error {
	keep := p.Capabilities
	if keep == nil {
		keep = spec.DefaultCapabilities
	}
	mask, err := capMask(keep)
	if err != nil {
		return err
	}
	if err := dropBounding(mask); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("keepcaps: %w", err)
	}
	groups := make([]int, len(p.AdditionalGIDs))
	for i, g := range p.AdditionalGIDs {
		groups[i] = int(g)
	}
	if err := unix.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := unix.Setresgid(int(p.GID), int(p.GID), int(p.GID)); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := unix.Setresuid(int(p.UID), int(p.UID), int(p.UID)); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("keepcaps: %w", err)
	}
	if err := setCaps(mask); err != nil {
		return err
	}
	if p.NoNewPrivileges {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("no_new_privs: %w", err)
		}
	}
	return nil
}

var rlimitNames = map[string]int{
	"RLIMIT_NOFILE": unix.RLIMIT_NOFILE,
	"RLIMIT_NPROC":  unix.RLIMIT_NPROC,
	"RLIMIT_CORE":   unix.RLIMIT_CORE,
	"RLIMIT_STACK":  unix.RLIMIT_STACK,
	"RLIMIT_AS":     unix.RLIMIT_AS,
	"RLIMIT_FSIZE":  unix.RLIMIT_FSIZE,
}

func setRlimits(rl []spec.Rlimit) error {
	for _, r := range rl {
		t, ok := rlimitNames[r.Type]
		if !ok {
			return fmt.Errorf("unknown rlimit %q", r.Type)
		}
		if err := unix.Setrlimit(t, &unix.Rlimit{Cur: r.Soft, Max: r.Hard}); err != nil {
			return fmt.Errorf("setrlimit %s: %w", r.Type, err)
		}
	}
	return nil
}

// lookPath resolves a command against the container's PATH (not ours: this
// process has an empty environment and is already inside the new root).
func lookPath(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			path = e[5:]
		}
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, file)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", file)
}

// runInit is a ~40 line tini. PID 1 is special in Linux: the kernel does not
// apply default signal dispositions to it (SIGTERM does nothing unless PID 1
// installs a handler), and orphaned processes are re-parented to it, so if it
// never calls wait() they pile up as zombies. Most application binaries do
// neither, which is why `docker run --init` exists.
func runInit(s *spec.Spec, path string, errPipe *os.File) {
	// Show up as "rh-init" in ps instead of "exe".
	_ = unix.Prctl(unix.PR_SET_NAME, uintptr(unsafe.Pointer(&[]byte("rh-init\x00")[0])), 0, 0, 0)
	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs)

	child, err := os.StartProcess(path, s.Process.Args, &os.ProcAttr{
		Env:   s.Process.Env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		fmt.Fprintf(errPipe, "exec %s: %v", s.Process.Args[0], err)
		errPipe.Close()
		os.Exit(1)
	}
	errPipe.Close()

	for sig := range sigs {
		if sig == syscall.SIGCHLD {
			// Reap every exited descendant, not just our direct child.
			for {
				var ws unix.WaitStatus
				pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				if pid == child.Pid {
					if ws.Signaled() {
						os.Exit(128 + int(ws.Signal()))
					}
					os.Exit(ws.ExitStatus())
				}
			}
			continue
		}
		if sig == syscall.SIGURG { // Go runtime preemption noise
			continue
		}
		// Forward everything else to the application.
		_ = unix.Kill(child.Pid, sig.(syscall.Signal))
	}
}
