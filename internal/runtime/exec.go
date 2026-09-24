package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aggarwalpulkit596/roundhouse/internal/cgroups"
	"github.com/aggarwalpulkit596/roundhouse/internal/spec"
)

// ExecArg is argv[1] for the helper that runs a new process inside an
// existing container (`rh exec`).
const ExecArg = "__exec"

// ExecConfig is sent to the exec helper on fd 3.
type ExecConfig struct {
	Process      spec.Process `json:"process"`
	CgroupParent string       `json:"cgroupParent"`
	ContainerID  string       `json:"containerId"`
}

// Exec runs a process inside the container whose init has pid initPid. The
// helper is our own binary; the nsenter constructor (cgo) joins the
// namespaces before Go starts, then ExecMain forks the target so it lands in
// the container's PID namespace.
func Exec(initPid int, cfg ExecConfig, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	cmd := exec.Command("/proc/self/exe", ExecArg)
	cmd.Args[0] = "rh-exec"
	cmd.Env = []string{fmt.Sprintf("_RH_NSENTER_PID=%d", initPid)}
	if cfg.ContainerID != "" {
		// Joined by the C constructor before it enters the mount namespace,
		// because inside the container /sys/fs/cgroup is not the host's.
		files := cgroups.New(cfg.CgroupParent, cfg.ContainerID).ProcsFiles()
		cmd.Env = append(cmd.Env, "_RH_NSENTER_CGROUPS="+strings.Join(files, ":"))
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.ExtraFiles = []*os.File{r}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return -1, err
	}
	r.Close()
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		w.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return -1, err
	}
	w.Close()

	// Forward interrupts so Ctrl-C reaches the process in the container.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	return ExitCode(cmd.ProcessState, err)
}

// ExecMain is the body of the exec helper. It is already inside the
// container's namespaces (see package nsenter).
func ExecMain() {
	var cfg ExecConfig
	f := os.NewFile(3, "exec-cfg")
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, "exec: read config:", err)
		os.Exit(125)
	}
	f.Close()

	if err := applyCredentials(cfg.Process); err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
		os.Exit(125)
	}
	path, err := lookPath(cfg.Process.Args[0], cfg.Process.Env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
		os.Exit(127)
	}
	cwd := cfg.Process.Cwd
	if cwd == "" {
		cwd = "/"
	}
	// setns(CLONE_NEWPID) only affects children, so we must fork.
	cmd := exec.Command(path)
	cmd.Args = cfg.Process.Args
	cmd.Env = cfg.Process.Env
	cmd.Dir = cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
		os.Exit(126)
	}
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	werr := cmd.Wait()
	code, _ := ExitCode(cmd.ProcessState, werr)
	os.Exit(code)
}
