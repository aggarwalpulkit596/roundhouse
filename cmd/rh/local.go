package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aggarwalpulkit596/roundhouse/internal/container"
	"github.com/aggarwalpulkit596/roundhouse/internal/image"
	"github.com/aggarwalpulkit596/roundhouse/internal/spec"
)

func init() {
	register("pull", "Images", "Download an image from a registry", cmdPull)
	register("images", "Images", "List local images", cmdImages)
	register("rmi", "Images", "Remove an image tag", cmdRmi)
	register("run", "Containers", "Create and start a container", cmdRun)
	register("create", "Containers", "Create a container without starting it", cmdCreate)
	register("start", "Containers", "Start a created or exited container", cmdStart)
	register("ps", "Containers", "List containers", cmdPs)
	register("exec", "Containers", "Run a command in a running container", cmdExec)
	register("logs", "Containers", "Print a container's output", cmdLogs)
	register("stop", "Containers", "Gracefully stop a container", cmdStop)
	register("kill", "Containers", "Send a signal to a container", cmdKill)
	register("rm", "Containers", "Remove containers", cmdRm)
	register("stats", "Containers", "Show cgroup resource usage", cmdStats)
	register("inspect", "Containers", "Show a container's record and state as JSON", cmdInspect)
}

func cmdPull(args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: rh pull IMAGE"); fs.PrintDefaults() }
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return exitError(2)
	}
	m, err := manager()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	start := time.Now()
	im, err := m.Images.Pull(ctx, fs.Arg(0), func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s  %d layers  %s  (%s)\n", im.Name, im.Config.Short(), len(im.Layers), image.HumanBytes(im.Size), time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdImages(args []string) error {
	m, err := manager()
	if err != nil {
		return err
	}
	list, err := m.Images.List()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE ID\tLAYERS\tSIZE\tPULLED")
	for _, im := range list {
		ref, _ := image.ParseReference(im.Name)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", ref.Familiar(), im.Config.Short(), len(im.Layers), image.HumanBytes(im.Size), ago(im.Created))
	}
	return tw.Flush()
}

func cmdRmi(args []string) error {
	m, err := manager()
	if err != nil {
		return err
	}
	for _, a := range args {
		im, err := m.Images.Get(a)
		if err != nil {
			return err
		}
		if err := m.Images.Untag(im.Name); err != nil {
			return err
		}
		fmt.Println("untagged", im.Name)
	}
	return nil
}

// runFlags are shared by run and create.
type runFlags struct {
	name, workdir, user, hostname, network, memory, cpus, entrypoint string
	pids                                                             int64
	env, ports, volumes, dns, hosts                                  stringList
	detach, rm, init, readonly, pull                                 bool
}

func (f *runFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.name, "name", "", "container name")
	fs.StringVar(&f.workdir, "w", "", "working directory inside the container")
	fs.StringVar(&f.user, "u", "", "user[:group] to run as")
	fs.StringVar(&f.hostname, "hostname", "", "container hostname")
	fs.StringVar(&f.network, "net", "bridge", "network mode: bridge, host or none")
	fs.StringVar(&f.memory, "m", "", "memory limit (e.g. 256m, 1g)")
	fs.StringVar(&f.cpus, "cpus", "", "CPU limit in cores (e.g. 0.5)")
	fs.StringVar(&f.entrypoint, "entrypoint", "", "override the image entrypoint")
	fs.Int64Var(&f.pids, "pids", 0, "maximum number of processes (fork-bomb guard)")
	fs.Var(&f.env, "e", "set an environment variable KEY=VALUE (repeatable)")
	fs.Var(&f.ports, "p", "publish HOSTPORT:CONTAINERPORT (repeatable)")
	fs.Var(&f.volumes, "v", "bind mount HOSTPATH:CONTAINERPATH[:ro] (repeatable)")
	fs.Var(&f.dns, "dns", "nameserver for /etc/resolv.conf (repeatable)")
	fs.Var(&f.hosts, "add-host", "extra /etc/hosts entry NAME:IP (repeatable)")
	fs.BoolVar(&f.detach, "d", false, "run in the background under a shim")
	fs.BoolVar(&f.rm, "rm", false, "remove the container when it exits")
	fs.BoolVar(&f.init, "init", false, "run a tiny init as PID 1 (signal forwarding, zombie reaping)")
	fs.BoolVar(&f.readonly, "read-only", false, "mount the root filesystem read-only")
	fs.BoolVar(&f.pull, "pull", true, "pull the image if it is not present")
}

func (f *runFlags) options(m *container.Manager, imageName string, cmd []string) (container.CreateOptions, error) {
	o := container.CreateOptions{
		Name: f.name, Image: imageName, Cmd: cmd, Env: f.env, Workdir: f.workdir, User: f.user,
		Hostname: f.hostname, Network: f.network, Init: f.init, ReadOnly: f.readonly,
		DNS: f.dns, AutoRemove: f.rm,
	}
	if f.entrypoint != "" {
		o.Entrypoint = strings.Fields(f.entrypoint)
	}
	var err error
	if o.Resources.MemoryBytes, err = ParseBytes(f.memory); err != nil {
		return o, err
	}
	if f.cpus != "" {
		c, err := strconv.ParseFloat(f.cpus, 64)
		if err != nil || c <= 0 {
			return o, fmt.Errorf("invalid --cpus %q", f.cpus)
		}
		o.Resources.CPUMillis = int64(c * 1000)
	}
	o.Resources.PidsMax = f.pids
	for _, p := range f.ports {
		pm, err := ParsePort(p)
		if err != nil {
			return o, err
		}
		o.Ports = append(o.Ports, pm)
	}
	for _, v := range f.volumes {
		mt, err := ParseVolume(v, m.Root)
		if err != nil {
			return o, err
		}
		o.Volumes = append(o.Volumes, mt)
	}
	if len(f.hosts) > 0 {
		o.ExtraHosts = map[string]string{}
		for _, h := range f.hosts {
			n, ip, ok := strings.Cut(h, ":")
			if !ok {
				return o, fmt.Errorf("invalid --add-host %q (want NAME:IP)", h)
			}
			o.ExtraHosts[n] = ip
		}
	}
	if f.pull {
		if _, err := m.Images.Get(imageName); err != nil {
			fmt.Fprintf(os.Stderr, "Unable to find image %s locally, pulling\n", imageName)
			if _, err := m.Images.Pull(context.Background(), imageName, func(fm string, a ...any) {
				fmt.Fprintf(os.Stderr, fm+"\n", a...)
			}); err != nil {
				return o, err
			}
		}
	}
	return o, nil
}

func createFromArgs(name string, args []string) (*container.Manager, *container.Info, *runFlags, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var f runFlags
	f.register(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: rh %s [flags] IMAGE [COMMAND [ARG...]]\n", name)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fs.Usage()
		return nil, nil, nil, exitError(2)
	}
	m, err := manager()
	if err != nil {
		return nil, nil, nil, err
	}
	o, err := f.options(m, fs.Arg(0), fs.Args()[1:])
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := m.Create(o)
	if err != nil {
		return nil, nil, nil, err
	}
	return m, c, &f, nil
}

func cmdRun(args []string) error {
	m, c, f, err := createFromArgs("run", args)
	if err != nil {
		return err
	}
	if f.detach {
		c, err := m.StartDetached(c.ID)
		if err != nil {
			return err
		}
		fmt.Println(c.ID)
		return nil
	}
	// Pdeathsig tracks the creating thread, so pin this goroutine to it.
	goruntime.LockOSThread()
	code, err := m.RunForeground(c.ID, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError(code)
	}
	return nil
}

func cmdCreate(args []string) error {
	_, c, _, err := createFromArgs("create", args)
	if err != nil {
		return err
	}
	fmt.Println(c.ID)
	return nil
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	attach := fs.Bool("a", false, "attach stdio and wait (foreground)")
	_ = fs.Parse(args)
	m, err := manager()
	if err != nil {
		return err
	}
	for _, ref := range fs.Args() {
		if *attach {
			code, err := m.RunForeground(ref, os.Stdin, os.Stdout, os.Stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError(code)
			}
			continue
		}
		c, err := m.StartDetached(ref)
		if err != nil {
			return err
		}
		fmt.Println(c.Name)
	}
	return nil
}

func cmdPs(args []string) error {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	all := fs.Bool("a", false, "show all containers (default shows running)")
	quiet := fs.Bool("q", false, "only print IDs")
	_ = fs.Parse(args)
	m, err := manager()
	if err != nil {
		return err
	}
	list, err := m.List()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if !*quiet {
		fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tCOMMAND\tSTATUS\tIP\tPORTS")
	}
	for _, c := range list {
		if !*all && c.State.Status != container.StatusRunning {
			continue
		}
		if *quiet {
			fmt.Fprintln(tw, c.ID[:12])
			continue
		}
		ref, _ := image.ParseReference(c.Image)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID[:12], c.Name, ref.Familiar(),
			truncate(strings.Join(c.Spec.Process.Args, " "), 28), statusText(c), c.IP, portsText(c.Ports))
	}
	return tw.Flush()
}

func statusText(c container.Info) string {
	switch c.State.Status {
	case container.StatusRunning:
		return "Up " + ago(c.State.StartedAt)
	case container.StatusExited:
		s := fmt.Sprintf("Exited (%d) %s ago", c.State.ExitCode, strings.TrimSuffix(ago(c.State.FinishedAt), " ago"))
		if c.State.OOMKilled {
			s += " OOMKilled"
		}
		return s
	}
	return "Created"
}

func portsText(ps []container.PortMapping) string {
	var s []string
	for _, p := range ps {
		s = append(s, fmt.Sprintf("%d->%d", p.HostPort, p.ContainerPort))
	}
	return strings.Join(s, ",")
}

func cmdExec(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	var env stringList
	fs.Var(&env, "e", "set an environment variable (repeatable)")
	user := fs.String("u", "", "user[:group]")
	wd := fs.String("w", "", "working directory")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rh exec [flags] CONTAINER COMMAND [ARG...]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		fs.Usage()
		return exitError(2)
	}
	m, err := manager()
	if err != nil {
		return err
	}
	code, err := m.Exec(fs.Arg(0), container.ExecOptions{Args: fs.Args()[1:], Env: env, User: *user, Cwd: *wd}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError(code)
	}
	return nil
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow output")
	tail := fs.Int("n", 0, "only the last N lines")
	ts := fs.Bool("t", false, "show timestamps")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: rh logs [-f] [-n N] CONTAINER")
	}
	m, err := manager()
	if err != nil {
		return err
	}
	c, err := m.Get(fs.Arg(0))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return m.ReadLogs(ctx, c.ID, *follow, *tail, func(l container.LogLine) error {
		out := os.Stdout
		if l.Stream == "stderr" {
			out = os.Stderr
		}
		if *ts {
			fmt.Fprintf(out, "%s %s\n", l.Time.Format(time.RFC3339Nano), l.Line)
		} else {
			fmt.Fprintln(out, l.Line)
		}
		return nil
	})
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	t := fs.Duration("t", 10*time.Second, "grace period before SIGKILL")
	_ = fs.Parse(args)
	m, err := manager()
	if err != nil {
		return err
	}
	for _, ref := range fs.Args() {
		if err := m.Stop(ref, *t); err != nil {
			return err
		}
		fmt.Println(ref)
	}
	return nil
}

func cmdKill(args []string) error {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	sigName := fs.String("s", "KILL", "signal name or number")
	_ = fs.Parse(args)
	sig, err := ParseSignal(*sigName)
	if err != nil {
		return err
	}
	m, err := manager()
	if err != nil {
		return err
	}
	for _, ref := range fs.Args() {
		if err := m.Signal(ref, sig); err != nil {
			return err
		}
	}
	return nil
}

func cmdRm(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	force := fs.Bool("f", false, "stop running containers first")
	_ = fs.Parse(args)
	m, err := manager()
	if err != nil {
		return err
	}
	var errs []error
	for _, ref := range fs.Args() {
		if err := m.Remove(ref, *force); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Println(ref)
	}
	return errors.Join(errs...)
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	stream := fs.Bool("w", false, "refresh every second")
	_ = fs.Parse(args)
	m, err := manager()
	if err != nil {
		return err
	}
	prev := map[string]uint64{}
	prevT := time.Now()
	for {
		list, err := m.List()
		if err != nil {
			return err
		}
		now := time.Now()
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tCPU %\tCPU TIME\tMEM USAGE / LIMIT\tPIDS\tOOM KILLS")
		for _, c := range list {
			if c.State.Status != container.StatusRunning {
				continue
			}
			if fs.NArg() > 0 && !contains(fs.Args(), c.Name) && !contains(fs.Args(), c.ID[:12]) {
				continue
			}
			s, err := m.Stats(c.ID)
			if err != nil {
				continue
			}
			pct := "-"
			if p, ok := prev[c.ID]; ok && s.CPUUsageNanos >= p {
				pct = fmt.Sprintf("%.1f%%", float64(s.CPUUsageNanos-p)/float64(now.Sub(prevT).Nanoseconds())*100)
			}
			prev[c.ID] = s.CPUUsageNanos
			limit := "unlimited"
			if s.MemoryLimit > 0 {
				limit = image.HumanBytes(int64(s.MemoryLimit))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s / %s\t%d\t%d\n", c.Name, pct,
				time.Duration(s.CPUUsageNanos).Round(time.Millisecond), image.HumanBytes(int64(s.MemoryBytes)), limit, s.Pids, s.OOMKills)
		}
		prevT = now
		tw.Flush()
		if !*stream {
			return nil
		}
		time.Sleep(time.Second)
		fmt.Print("\033[H\033[2J")
	}
}

func cmdInspect(args []string) error {
	m, err := manager()
	if err != nil {
		return err
	}
	var out []any
	for _, ref := range args {
		c, err := m.Get(ref)
		if err != nil {
			return err
		}
		out = append(out, c)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ---------------------------------------------------------------------------
// parsing helpers

// ParseBytes parses "512m", "1g", "1048576".
func ParseBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(strings.TrimSuffix(s, "b"), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// ParsePort parses "8080:80" or "127.0.0.1:8080:80".
func ParsePort(s string) (container.PortMapping, error) {
	parts := strings.Split(s, ":")
	var pm container.PortMapping
	var err error
	switch len(parts) {
	case 2:
		pm.HostPort, err = strconv.Atoi(parts[0])
		if err == nil {
			pm.ContainerPort, err = strconv.Atoi(parts[1])
		}
	case 3:
		pm.HostIP = parts[0]
		pm.HostPort, err = strconv.Atoi(parts[1])
		if err == nil {
			pm.ContainerPort, err = strconv.Atoi(parts[2])
		}
	default:
		err = errors.New("want HOSTPORT:CONTAINERPORT")
	}
	if err != nil || pm.HostPort <= 0 || pm.ContainerPort <= 0 || pm.HostPort > 65535 || pm.ContainerPort > 65535 {
		return pm, fmt.Errorf("invalid port mapping %q", s)
	}
	return pm, nil
}

// ParseVolume parses "/host:/ctr[:ro]" or "name:/ctr" (a named volume under
// RH_ROOT/volumes).
func ParseVolume(s, root string) (spec.Mount, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return spec.Mount{}, fmt.Errorf("invalid volume %q (want SRC:DST[:ro])", s)
	}
	mt := spec.Mount{Source: parts[0], Target: parts[1], ReadOnly: len(parts) == 3 && parts[2] == "ro"}
	if !strings.HasPrefix(mt.Target, "/") {
		return mt, fmt.Errorf("volume target %q must be absolute", mt.Target)
	}
	if !strings.HasPrefix(mt.Source, "/") {
		if strings.ContainsAny(mt.Source, "/.") {
			return mt, fmt.Errorf("invalid volume name %q", mt.Source)
		}
		mt.Source = root + "/volumes/" + mt.Source
		if err := os.MkdirAll(mt.Source, 0o755); err != nil {
			return mt, err
		}
	}
	return mt, nil
}

// ParseSignal accepts "TERM", "SIGTERM" or "15".
func ParseSignal(s string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return syscall.Signal(n), nil
	}
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	m := map[string]syscall.Signal{
		"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
		"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "TERM": syscall.SIGTERM, "STOP": syscall.SIGSTOP,
		"CONT": syscall.SIGCONT, "WINCH": syscall.SIGWINCH,
	}
	if sig, ok := m[s]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
