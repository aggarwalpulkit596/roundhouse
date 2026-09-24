// Package runtime is the low-level container runtime: the part of Roundhouse
// that plays the role of runc.
//
// Starting a container is a two-process dance, the same one runc performs:
//
//	parent (rh / shim)                      child (/proc/self/exe __init)
//	──────────────────                      ──────────────────────────────
//	clone(NEWPID|NEWNS|NEWUTS|NEWIPC|NEWNET)  ─►  born in new namespaces,
//	  + CLONE_INTO_CGROUP (v2)                   blocks reading the sync pipe
//	add pid to cgroup (v1)
//	BeforeRelease hook (veth → netns)
//	write spec JSON to sync pipe          ─►  sethostname, mount /proc /sys /dev,
//	                                          pivot_root, drop caps, setuid,
//	wait for error pipe EOF               ◄─  execve(user program)  (pipe is
//	                                          O_CLOEXEC, so EOF == exec worked)
//
// Re-executing our own binary ("/proc/self/exe") is how Go programs get a
// fresh, single-purpose process inside the namespaces: Go cannot safely
// fork() without exec because the runtime is multi-threaded.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/aggarwalpulkit596/roundhouse/internal/cgroups"
	"github.com/aggarwalpulkit596/roundhouse/internal/spec"
)

// InitArg is argv[1] when the binary is re-executed as a container's init.
const InitArg = "__init"

// Options control how the init process is attached to its parent.
type Options struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Setsid puts the container in its own session. Detached containers want
	// this; a foreground container attached to a terminal must not use it.
	Setsid bool

	// Pdeathsig delivers SIGKILL to the container init if the parent thread
	// dies. Callers using it should runtime.LockOSThread first, because the
	// kernel tracks the parent *thread*, not the process.
	Pdeathsig bool

	// BeforeRelease runs once the init process exists inside its new
	// namespaces and cgroup but before it touches its root filesystem or
	// execs. Networking is wired here: the veth peer is moved into the
	// network namespace of pid.
	BeforeRelease func(pid int) error
}

// Container is a started container init process.
type Container struct {
	Spec   *spec.Spec
	Pid    int
	Cgroup cgroups.Manager
	cmd    *exec.Cmd
}

// Start creates the container and returns once the user program has been
// exec'd (or the init has failed and reported why).
func Start(s *spec.Spec, opt Options) (*Container, error) {
	if s.RootFS == "" || len(s.Process.Args) == 0 {
		return nil, errors.New("runtime: rootfs and process args are required")
	}
	cg := cgroups.New(s.CgroupParent, s.ID)
	if err := cg.Create(s.Resources); err != nil {
		return nil, fmt.Errorf("cgroup: %w", err)
	}

	// cfgR/cfgW carries the spec to the child. errR/errW carries a failure
	// message back; the child marks errW close-on-exec, so a successful
	// execve closes it and the parent reads EOF.
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		return nil, err
	}

	cmd := exec.Command("/proc/self/exe", InitArg)
	cmd.Args[0] = "rh-init"
	cmd.Env = []string{} // the child receives its env through the spec
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opt.Stdin, opt.Stdout, opt.Stderr
	cmd.ExtraFiles = []*os.File{cfgR, errW} // fd 3, fd 4 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cloneFlags(s.Namespaces),
		Setsid:     opt.Setsid,
	}
	if opt.Pdeathsig {
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	}

	// On cgroup v2, clone3(CLONE_INTO_CGROUP) makes the child start life in
	// its cgroup, so there is no window where it runs unconstrained.
	var cgFD *os.File
	if cg.Version() == 2 {
		if f, err := os.Open(cg.Path()); err == nil {
			cgFD = f
			cmd.SysProcAttr.UseCgroupFD = true
			cmd.SysProcAttr.CgroupFD = int(f.Fd())
		}
	}

	startErr := cmd.Start()
	if cgFD != nil {
		cgFD.Close()
	}
	cfgR.Close()
	errW.Close()
	if startErr != nil {
		cfgW.Close()
		errR.Close()
		_ = cg.Destroy()
		return nil, fmt.Errorf("start init: %w", startErr)
	}
	c := &Container{Spec: s, Pid: cmd.Process.Pid, Cgroup: cg, cmd: cmd}

	fail := func(err error) (*Container, error) {
		cfgW.Close()
		errR.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = cg.Destroy()
		return nil, err
	}

	if cg.Version() == 1 || cgFD == nil {
		if err := cg.Add(c.Pid); err != nil {
			return fail(fmt.Errorf("join cgroup: %w", err))
		}
	}
	if opt.BeforeRelease != nil {
		if err := opt.BeforeRelease(c.Pid); err != nil {
			return fail(err)
		}
	}
	if err := json.NewEncoder(cfgW).Encode(s); err != nil {
		return fail(fmt.Errorf("send spec: %w", err))
	}
	cfgW.Close()

	msg, _ := io.ReadAll(errR)
	errR.Close()
	if len(msg) > 0 {
		return fail(fmt.Errorf("container init: %s", strings.TrimSpace(string(msg))))
	}
	return c, nil
}

// Wait blocks until the init process exits and returns its exit status in
// shell convention: the exit code, or 128+signal when killed by a signal.
func (c *Container) Wait() (int, error) {
	err := c.cmd.Wait()
	return ExitCode(c.cmd.ProcessState, err)
}

// Signal sends sig to the container's init process.
func (c *Container) Signal(sig syscall.Signal) error {
	return unix.Kill(c.Pid, sig)
}

// ExitCode converts a finished process into a shell-style status.
func ExitCode(ps *os.ProcessState, waitErr error) (int, error) {
	if ps == nil {
		return -1, waitErr
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ws.ExitStatus(), nil
	}
	return ps.ExitCode(), nil
}

func cloneFlags(n spec.Namespaces) uintptr {
	var f uintptr
	if n.PID {
		f |= unix.CLONE_NEWPID
	}
	if n.UTS {
		f |= unix.CLONE_NEWUTS
	}
	if n.IPC {
		f |= unix.CLONE_NEWIPC
	}
	if n.Mount {
		f |= unix.CLONE_NEWNS
	}
	if n.Net {
		f |= unix.CLONE_NEWNET
	}
	// The cgroup namespace is unshared by the child itself, after it is
	// already inside its cgroup, so that its view is rooted there.
	return f
}
