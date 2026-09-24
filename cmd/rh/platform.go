package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aggarwalpulkit596/roundhouse/internal/api"
	"github.com/aggarwalpulkit596/roundhouse/internal/builder"
	"github.com/aggarwalpulkit596/roundhouse/internal/container"
	"github.com/aggarwalpulkit596/roundhouse/internal/engine"
	"github.com/aggarwalpulkit596/roundhouse/internal/image"
	"github.com/aggarwalpulkit596/roundhouse/internal/web"
)

func init() {
	register("daemon", "Platform", "Run the provisioning engine (API, reconciler, edge, DNS)", cmdDaemon)
	register("deploy", "Platform", "Declare a service and roll it out", cmdDeploy)
	register("svc", "Platform", "Manage services: ls, status, logs, redeploy, rollback, rm", cmdSvc)
	register("events", "Platform", "Show the engine's activity feed", cmdEvents)
	register("usage", "Platform", "Show metered CPU/memory usage and cost", cmdUsage)
	register("dashboard", "Platform", "Print the dashboard URL (with a login link if a token is set)", cmdDashboard)
}

// ---------------------------------------------------------------------------
// daemon

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", api.DefaultSocket, "unix socket for the API")
	httpAddr := fs.String("http", "127.0.0.1:7070", "serve the dashboard and API on this address (\"\" to disable; 0.0.0.0:7070 to reach it from other machines, token required)")
	tokenFlag := fs.String("token", "", "dashboard token (default: generated and saved when --http is not loopback)")
	nodeName := fs.String("node", "", "node name (default: hostname)")
	workers := fs.Int("workers", 4, "services reconciled in parallel")
	noDNS := fs.Bool("no-dns", false, "do not run the private DNS server on the bridge gateway")
	priceCPU := fs.Float64("price-vcpu", 0, "USD per vCPU-minute (default: illustrative $20/vCPU-month)")
	priceMem := fs.Float64("price-mem", 0, "USD per GB-minute (default: illustrative $10/GB-month)")
	meter := fs.Duration("meter", 10*time.Second, "usage sampling interval")
	_ = fs.Parse(args)

	m, err := manager()
	if err != nil {
		return err
	}
	if *nodeName == "" {
		*nodeName, _ = os.Hostname()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := m.Net.Setup(); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	cfg := engine.Config{
		StateDir:      m.Root + "/engine",
		Runtime:       engine.NodeRuntime{M: m},
		Workers:       *workers,
		VolumeDir:     m.Root + "/volumes",
		MeterInterval: *meter,
	}
	if *priceCPU > 0 || *priceMem > 0 {
		cfg.Prices = engine.Prices{VCPUPerMinute: *priceCPU, MemoryGBPerMinute: *priceMem}
	}
	var dnsSrv *engine.DNSServer
	gw := m.Net.Gateway().String()
	if !*noDNS {
		cfg.DNS = []string{gw}
	}
	eng, err := engine.New(cfg)
	if err != nil {
		return err
	}
	if !*noDNS {
		var ups []string
		for _, ns := range container.HostNameservers() {
			ups = append(ups, net.JoinHostPort(ns, "53"))
		}
		dnsSrv = &engine.DNSServer{Engine: eng, Upstream: ups}
		go func() {
			if err := dnsSrv.ListenAndServe(ctx, net.JoinHostPort(gw, "53")); err != nil {
				fmt.Fprintf(os.Stderr, "rh daemon: private DNS disabled: %v\n", err)
			}
		}()
	}

	builds := web.NewBuilds(&builder.Builder{Store: m.Images, Dir: filepath.Join(m.Root, "build")}, eng, filepath.Join(m.Root, "build", "checkouts"))
	engineAPI := eng.Handler(*nodeName, memTotalMB())
	handler := web.APIHandler(engineAPI, builds)
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(*socket, 0o660)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- engine.Serve(ctx, srv, func() error { return srv.Serve(ln) }) }()
	if *httpAddr != "" {
		loopback := web.IsLoopbackAddr(*httpAddr)
		token := *tokenFlag
		if token == "" && !loopback {
			if token, err = dashboardToken(m.Root); err != nil {
				return err
			}
		}
		ui := web.Handler(web.Config{API: engineAPI, Builds: builds, Token: token, LoopbackOnly: loopback})
		tsrv := &http.Server{Addr: *httpAddr, Handler: ui, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- engine.Serve(ctx, tsrv, tsrv.ListenAndServe) }()
		fmt.Println(dashboardURL(*httpAddr, token))
	}
	fmt.Printf("roundhouse daemon on %s (node %s, state %s, private DNS %s)\n", *socket, *nodeName, m.Root, dnsState(*noDNS, gw))

	go eng.Run(ctx)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			stop()
			return err
		}
	}
	fmt.Println("shutting down; containers keep running under their shims")
	time.Sleep(200 * time.Millisecond)
	return nil
}

func dnsState(off bool, gw string) string {
	if off {
		return "off"
	}
	return "*.rh.internal on " + gw + ":53"
}

func memTotalMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 && fs[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(fs[1])
			return kb / 1024
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// deploy

func cmdDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	file := fs.String("f", "", "service spec JSON file (flags override its fields)")
	img := fs.String("i", "", "image")
	port := fs.Int("port", 0, "port the app listens on (injected as $PORT)")
	public := fs.Int("public", 0, "expose on this host port through the edge proxy")
	replicas := fs.Int("replicas", 0, "number of replicas")
	cpu := fs.Float64("cpu", 0, "CPU limit in cores")
	memory := fs.Int("memory", 0, "memory limit in MB")
	health := fs.String("health", "", "healthcheck: an HTTP path like /health, or 'tcp'")
	restart := fs.String("restart", "", "restart policy: always, on-failure, never")
	volume := fs.String("volume", "", "persistent volume NAME:/mount/path")
	drain := fs.Int("drain", 0, "seconds replaced instances keep draining")
	timeout := fs.Int("timeout", 0, "seconds a deployment may take to become healthy")
	cmdStr := fs.String("cmd", "", "command override (whitespace separated)")
	wait := fs.Bool("wait", true, "stream events until the deployment is ACTIVE or FAILED")
	var env stringList
	fs.Var(&env, "e", "environment variable KEY=VALUE (repeatable; ${{svc.VAR}} references allowed)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rh deploy [flags] NAME\n\nExamples:\n  rh deploy -i mirror.gcr.io/library/nginx:alpine --port 80 --public 8080 --replicas 2 --health / web\n  rh deploy -f examples/whoami.json")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	var sp engine.ServiceSpec
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sp); err != nil {
			return fmt.Errorf("%s: %w", *file, err)
		}
	}
	if fs.NArg() > 0 {
		sp.Name = fs.Arg(0)
	}
	if sp.Name == "" {
		fs.Usage()
		return exitError(2)
	}
	if *img != "" {
		sp.Image = *img
	}
	setIf := func(dst *int, v int) {
		if v != 0 {
			*dst = v
		}
	}
	setIf(&sp.Port, *port)
	setIf(&sp.PublicPort, *public)
	setIf(&sp.Replicas, *replicas)
	setIf(&sp.MemoryMB, *memory)
	setIf(&sp.DrainSeconds, *drain)
	setIf(&sp.DeployTimeoutSeconds, *timeout)
	if *cpu > 0 {
		sp.CPU = *cpu
	}
	if *restart != "" {
		sp.Restart = *restart
	}
	if *cmdStr != "" {
		sp.Cmd = strings.Fields(*cmdStr)
	}
	if *health != "" {
		if *health == "tcp" {
			sp.Healthcheck = &engine.Healthcheck{Type: "tcp"}
		} else {
			sp.Healthcheck = &engine.Healthcheck{Type: "http", Path: *health}
		}
	}
	if *volume != "" {
		n, p, ok := strings.Cut(*volume, ":")
		if !ok {
			return fmt.Errorf("--volume wants NAME:/path")
		}
		sp.Volume = &engine.Volume{Name: n, MountPath: p}
	}
	if len(env) > 0 {
		kv, err := engine.ParseEnvList(env)
		if err != nil {
			return err
		}
		if sp.Env == nil {
			sp.Env = map[string]string{}
		}
		for k, v := range kv {
			sp.Env[k] = v
		}
	}

	c := api.FromEnv()
	ctx, cancel := api.Timeout()
	defer cancel()
	var d engine.Deployment
	if err := c.Do(ctx, http.MethodPut, "/v1/services/"+sp.Name, sp, &d); err != nil {
		return err
	}
	fmt.Printf("service %s rev %d (%s) %s\n", sp.Name, d.Revision, d.ID, d.Status)
	if !*wait || d.Status == engine.StatusActive {
		return nil
	}
	return waitDeployment(c, sp.Name, d)
}

// waitDeployment streams the service's events until the deployment settles.
func waitDeployment(c *api.Client, service string, d engine.Deployment) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			var v engine.ServiceView
			if err := c.Do(ctx, http.MethodGet, "/v1/services/"+service, nil, &v); err != nil {
				continue
			}
			for _, x := range v.Deployments {
				if x.ID != d.ID {
					continue
				}
				switch x.Status {
				case engine.StatusActive:
					done <- nil
					return
				case engine.StatusFailed, engine.StatusCrashed, engine.StatusSkipped, engine.StatusRemoved:
					msg := fmt.Sprintf("rev %d %s: %s", x.Revision, x.Status, x.Reason)
					if len(x.Logs) > 0 {
						msg += "\nlast output:\n  " + strings.Join(x.Logs, "\n  ")
					}
					done <- errors.New(msg)
					return
				}
			}
		}
	}()
	go func() {
		_ = c.Stream(ctx, "/v1/events"+api.Query("service", service, "follow", "1"), func(raw json.RawMessage) error {
			var ev engine.Event
			if json.Unmarshal(raw, &ev) == nil && (ev.Deployment == d.ID || ev.Type == "routing") && ev.Time.After(d.CreatedAt.Add(-time.Second)) {
				fmt.Printf("  %s  %-10s %s\n", ev.Time.Local().Format("15:04:05.000"), ev.Type, ev.Message)
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		time.Sleep(100 * time.Millisecond) // let the last events print
		return err
	case <-ctx.Done():
		return nil
	}
}

// ---------------------------------------------------------------------------
// svc

func cmdSvc(args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	c := api.FromEnv()
	sub, rest := args[0], args[1:]
	need := func(n int, usage string) error {
		if len(rest) < n {
			return fmt.Errorf("usage: rh svc %s", usage)
		}
		return nil
	}
	ctx, cancel := api.Timeout()
	defer cancel()
	switch sub {
	case "ls", "list":
		var list []engine.ServiceView
		if err := c.Do(ctx, http.MethodGet, "/v1/services", nil, &list); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SERVICE\tSTATUS\tREV\tIMAGE\tREPLICAS\tURL")
		for _, s := range list {
			rev, ready := "-", 0
			if s.Active != nil {
				rev = strconv.Itoa(s.Active.Revision)
				for _, i := range s.Instances {
					if i.Deployment == s.Active.ID && i.Healthy {
						ready++
					}
				}
			}
			url := s.PublicURL
			if url == "" {
				url = s.PrivateURL
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\n", s.Name, s.Status, rev, familiar(s.Spec.Image), ready, s.Spec.Replicas, url)
		}
		return tw.Flush()
	case "status", "inspect":
		if err := need(1, "status NAME"); err != nil {
			return err
		}
		var s engine.ServiceView
		if err := c.Do(ctx, http.MethodGet, "/v1/services/"+rest[0]+"?stats=1", nil, &s); err != nil {
			return err
		}
		printService(s)
		return nil
	case "logs":
		fs := flag.NewFlagSet("svc logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow")
		tail := fs.Int("n", 100, "last N lines per instance")
		_ = fs.Parse(rest)
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: rh svc logs [-f] [-n N] NAME")
		}
		sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		f := ""
		if *follow {
			f = "1"
		}
		return c.Stream(sctx, "/v1/services/"+fs.Arg(0)+"/logs"+api.Query("follow", f, "tail", strconv.Itoa(*tail)), func(raw json.RawMessage) error {
			var l struct {
				container.LogLine
				Instance string `json:"instance"`
			}
			if json.Unmarshal(raw, &l) == nil {
				fmt.Printf("%s  %s\n", dim(l.Instance), l.Line)
			}
			return nil
		})
	case "redeploy":
		if err := need(1, "redeploy NAME"); err != nil {
			return err
		}
		var d engine.Deployment
		if err := c.Do(ctx, http.MethodPost, "/v1/services/"+rest[0]+"/redeploy", nil, &d); err != nil {
			return err
		}
		fmt.Printf("service %s rev %d queued\n", rest[0], d.Revision)
		return waitDeployment(c, rest[0], d)
	case "rollback":
		if err := need(1, "rollback NAME [DEPLOYMENT]"); err != nil {
			return err
		}
		body := map[string]string{}
		if len(rest) > 1 {
			body["deployment"] = rest[1]
		}
		var d engine.Deployment
		if err := c.Do(ctx, http.MethodPost, "/v1/services/"+rest[0]+"/rollback", body, &d); err != nil {
			return err
		}
		fmt.Printf("service %s rev %d queued (%s)\n", rest[0], d.Revision, d.Reason)
		return waitDeployment(c, rest[0], d)
	case "rm", "delete":
		if err := need(1, "rm NAME"); err != nil {
			return err
		}
		for _, n := range rest {
			if err := c.Do(ctx, http.MethodDelete, "/v1/services/"+n, nil, nil); err != nil {
				return err
			}
			fmt.Println("deleting", n)
		}
		return nil
	}
	return fmt.Errorf("unknown svc command %q (ls, status, logs, redeploy, rollback, rm)", sub)
}

func printService(s engine.ServiceView) {
	fmt.Printf("%s  %s\n", s.Name, s.Status)
	fmt.Printf("  image     %s\n", s.Spec.Image)
	if s.PrivateURL != "" {
		fmt.Printf("  private   %s\n", s.PrivateURL)
	}
	if s.PublicURL != "" {
		fmt.Printf("  public    %s\n", s.PublicURL)
	}
	fmt.Println("\nDEPLOYMENTS")
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  REV\tID\tSTATUS\tIMAGE\tCREATED\tREASON")
	for i := len(s.Deployments) - 1; i >= 0; i-- {
		d := s.Deployments[i]
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%s\n", d.Revision, d.ID[:8], d.Status, familiar(d.Spec.Image), ago(d.CreatedAt), truncate(d.Reason, 70))
	}
	tw.Flush()
	for _, d := range s.Deployments {
		if d.Status == engine.StatusFailed && len(d.Logs) > 0 {
			fmt.Printf("\n  last output of failed rev %d:\n", d.Revision)
			for _, l := range d.Logs {
				fmt.Printf("    %s\n", l)
			}
		}
	}
	fmt.Println("\nINSTANCES")
	tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tSTATUS\tHEALTH\tIP\tRESTARTS\tCPU\tMEMORY")
	for _, i := range s.Instances {
		health := "-"
		if i.Status == container.StatusRunning {
			health = "starting"
			if i.Healthy {
				health = "healthy"
			} else if i.Health != "" {
				health = "unhealthy"
			}
		}
		status := i.Status
		if i.Status == container.StatusExited {
			status = fmt.Sprintf("exited(%d)", i.ExitCode)
			if i.OOMKilled {
				status += " OOM"
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\t%s\n", i.Name, status, health, i.IP, i.Restarts,
			time.Duration(i.CPUNanos).Round(time.Millisecond), image.HumanBytes(int64(i.MemBytes)))
	}
	tw.Flush()
}

// ---------------------------------------------------------------------------
// events, usage

func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow")
	service := fs.String("service", "", "only this service")
	_ = fs.Parse(args)
	c := api.FromEnv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	f := ""
	if *follow {
		f = "1"
	}
	return c.Stream(ctx, "/v1/events"+api.Query("service", *service, "follow", f), func(raw json.RawMessage) error {
		var ev engine.Event
		if json.Unmarshal(raw, &ev) == nil {
			svc := ev.Service
			if svc == "" {
				svc = "-"
			}
			fmt.Printf("%s  %-12s %-10s %s\n", ev.Time.Local().Format("15:04:05.000"), svc, ev.Type, ev.Message)
		}
		return nil
	})
}

func cmdUsage(args []string) error {
	c := api.FromEnv()
	ctx, cancel := api.Timeout()
	defer cancel()
	var out struct {
		Prices   engine.Prices  `json:"prices"`
		Services []engine.Usage `json:"services"`
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/usage", nil, &out); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tvCPU-SECONDS\tGB-SECONDS\tSINCE\tCOST (USD)")
	total := 0.0
	for _, u := range out.Services {
		fmt.Fprintf(tw, "%s\t%.3f\t%.3f\t%s\t$%.6f\n", u.Service, u.CPUSeconds, u.MemoryGBSeconds, ago(u.Since), u.CostUSD)
		total += u.CostUSD
	}
	fmt.Fprintf(tw, "TOTAL\t\t\t\t$%.6f\n", total)
	tw.Flush()
	fmt.Printf("\nrates: $%.6f per vCPU-minute, $%.6f per GB-minute (billed on measured usage, per second)\n", out.Prices.VCPUPerMinute, out.Prices.MemoryGBPerMinute)
	return nil
}

func familiar(ref string) string {
	r, err := image.ParseReference(ref)
	if err != nil {
		return ref
	}
	return r.Familiar()
}

func dim(s string) string {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return "\033[2m" + s + "\033[0m"
	}
	return s
}

// ---------------------------------------------------------------------------
// dashboard

// dashboardToken loads or creates the dashboard token (RH_ROOT/ui-token,
// readable by root only).
func dashboardToken(root string) (string, error) {
	p := filepath.Join(root, "ui-token")
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b)), nil
	}
	t := web.NewToken()
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// dashboardURL is the address to open, with a one-click login when a token
// is required. For 0.0.0.0 it lists the machine's addresses.
func dashboardURL(addr, token string) string {
	host, port, _ := net.SplitHostPort(addr)
	hosts := []string{host}
	if host == "" || host == "0.0.0.0" || host == "::" {
		hosts = nil
		if ifaces, err := net.InterfaceAddrs(); err == nil {
			for _, a := range ifaces {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !strings.HasPrefix(ipn.IP.String(), "10.88.") {
					hosts = append(hosts, ipn.IP.String())
				}
			}
		}
		if len(hosts) == 0 {
			hosts = []string{"<this-machine's-ip>"}
		}
	}
	var b strings.Builder
	b.WriteString("dashboard:")
	for _, h := range hosts {
		if token != "" {
			fmt.Fprintf(&b, "\n  http://%s/login?token=%s", net.JoinHostPort(h, port), token)
		} else {
			fmt.Fprintf(&b, "\n  http://%s", net.JoinHostPort(h, port))
		}
	}
	if token != "" {
		b.WriteString("\n  (the token is in RH_ROOT/ui-token; anyone with it can run containers here)")
	}
	return b.String()
}

func cmdDashboard(args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	addr := fs.String("http", "", "the address the daemon serves the dashboard on (default: read from the running service, else 127.0.0.1:7070)")
	_ = fs.Parse(args)
	if *addr == "" {
		*addr = "127.0.0.1:7070"
		if b, err := os.ReadFile("/etc/default/roundhouse"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "RH_HTTP="); ok && v != "" {
					*addr = strings.Trim(v, `"`)
				}
			}
		}
	}
	token := ""
	if !web.IsLoopbackAddr(*addr) {
		if os.Geteuid() != 0 {
			return fmt.Errorf("reading the dashboard token needs root: sudo rh dashboard")
		}
		var err error
		if token, err = dashboardToken(stateRoot()); err != nil {
			return err
		}
	}
	fmt.Println(dashboardURL(*addr, token))
	return nil
}
