package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/cgroups"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
)

// fakeRuntime is an in-memory node. Tests drive container exits and health
// directly, which makes rollout edge cases deterministic.
type fakeRuntime struct {
	mu      sync.Mutex
	images  map[string]bool
	pullErr error
	ctrs    map[string]*container.Info
	nextIP  int
	creates int
	// exitOnStart makes every container of an image exit immediately.
	exitOnStart map[string]int
	// maxRunning records the peak number of running containers per service.
	maxRunning map[string]int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{images: map[string]bool{}, ctrs: map[string]*container.Info{}, nextIP: 2, exitOnStart: map[string]int{}, maxRunning: map[string]int{}}
}

func (f *fakeRuntime) HasImage(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[ref]
}

func (f *fakeRuntime) PullImage(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullErr != nil {
		return f.pullErr
	}
	f.images[ref] = true
	return nil
}

func (f *fakeRuntime) Create(o container.CreateOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.ctrs {
		if c.Name == o.Name {
			return "", fmt.Errorf("name %s in use", o.Name)
		}
	}
	f.creates++
	id := fmt.Sprintf("%064x", f.creates)
	c := &container.Info{Record: container.Record{
		ID: id, Name: o.Name, Image: o.Image, Created: time.Now(), Labels: o.Labels,
		IP: fmt.Sprintf("10.88.0.%d", f.nextIP), Network: container.NetBridge,
	}, State: container.State{Status: container.StatusCreated}}
	c.Spec.Process.Env = o.Env
	f.nextIP++
	f.ctrs[id] = c
	return id, nil
}

func (f *fakeRuntime) start(c *container.Info) {
	c.State.Status = container.StatusRunning
	c.State.StartedAt = time.Now().Add(-2 * time.Second) // "running for a while"
	if code, ok := f.exitOnStart[c.Image]; ok {
		c.State.Status = container.StatusExited
		c.State.ExitCode = code
		c.State.FinishedAt = time.Now()
	}
	svc := c.Labels[LabelService]
	n := 0
	for _, o := range f.ctrs {
		if o.Labels[LabelService] == svc && o.State.Status == container.StatusRunning {
			n++
		}
	}
	if n > f.maxRunning[svc] {
		f.maxRunning[svc] = n
	}
}

func (f *fakeRuntime) Start(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.ctrs[id]
	if !ok {
		return errors.New("no such container")
	}
	f.start(c)
	return nil
}

func (f *fakeRuntime) Restart(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.ctrs[id]
	if !ok {
		return errors.New("no such container")
	}
	c.State.Restarts++
	f.start(c)
	return nil
}

func (f *fakeRuntime) Stop(id string, grace time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.ctrs[id]; ok && c.State.Status == container.StatusRunning {
		c.State.Status = container.StatusExited
		c.State.ExitCode = 143
		c.State.FinishedAt = time.Now()
	}
	return nil
}

func (f *fakeRuntime) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ctrs, id)
	return nil
}

func (f *fakeRuntime) List() ([]container.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]container.Info, 0, len(f.ctrs))
	for _, c := range f.ctrs {
		out = append(out, *c)
	}
	return out, nil
}

func (f *fakeRuntime) Stats(id string) (cgroups.Stats, error) {
	return cgroups.Stats{CPUUsageNanos: uint64(time.Now().UnixNano() % 1e9), MemoryBytes: 64 << 20}, nil
}

func (f *fakeRuntime) Logs(ctx context.Context, id string, follow bool, tail int, fn func(container.LogLine) error) error {
	return fn(container.LogLine{Line: "boom: missing DATABASE_URL"})
}

// crash makes a running container exit.
func (f *fakeRuntime) crash(pred func(*container.Info) bool, code int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.ctrs {
		if pred(c) && c.State.Status == container.StatusRunning {
			c.State.Status = container.StatusExited
			c.State.ExitCode = code
			c.State.FinishedAt = time.Now()
			n++
		}
	}
	return n
}

func (f *fakeRuntime) running(service string) []container.Info {
	list, _ := f.List()
	var out []container.Info
	for _, c := range list {
		if c.Labels[LabelService] == service && c.State.Status == container.StatusRunning {
			out = append(out, c)
		}
	}
	return out
}

// fakeProber reports unhealthy for images listed in bad.
type fakeProber struct {
	rt  *fakeRuntime
	mu  sync.Mutex
	bad map[string]bool
}

func (p *fakeProber) Probe(ctx context.Context, ip string, port int, hc Healthcheck) error {
	p.rt.mu.Lock()
	var img string
	for _, c := range p.rt.ctrs {
		if c.IP == ip {
			img = c.Image
		}
	}
	p.rt.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bad[img] {
		return errors.New("connection refused")
	}
	return nil
}

type harness struct {
	t      *testing.T
	e      *Engine
	rt     *fakeRuntime
	prober *fakeProber
	cancel context.CancelFunc
	done   chan struct{}
	dir    string
}

func newHarness(t *testing.T, rt *fakeRuntime, dir string) *harness {
	t.Helper()
	if rt == nil {
		rt = newFakeRuntime()
	}
	if dir == "" {
		dir = t.TempDir()
	}
	pr := &fakeProber{rt: rt, bad: map[string]bool{}}
	e, err := New(Config{
		StateDir: dir, Runtime: rt, Prober: pr, Resync: 20 * time.Millisecond,
		BackoffBase: 10 * time.Millisecond, MeterInterval: 50 * time.Millisecond,
		VolumeDir: t.TempDir(), Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, e: e, rt: rt, prober: pr, cancel: cancel, done: make(chan struct{}), dir: dir}
	go func() { e.Run(ctx); close(h.done) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var evs []string
	for _, ev := range h.e.Events(0, "") {
		evs = append(evs, ev.Service+": "+ev.Message)
	}
	h.t.Fatalf("timed out waiting for %s\nevents:\n  %s", what, strings.Join(evs, "\n  "))
}

func (h *harness) status(service string, rev int) string {
	v, err := h.e.Service(service, false)
	if err != nil {
		return ""
	}
	for _, d := range v.Deployments {
		if d.Revision == rev {
			return d.Status
		}
	}
	return ""
}

func webSpec(image string) ServiceSpec {
	return ServiceSpec{
		Name: "web", Image: image, Port: 8080, Replicas: 2,
		Healthcheck:  &Healthcheck{Path: "/health", IntervalSeconds: 1},
		DrainSeconds: 1, DeployTimeoutSeconds: 2,
	}
}

func TestDeployBecomesActive(t *testing.T) {
	h := newHarness(t, nil, "")
	d, created, err := h.e.Apply(webSpec("app:v1"))
	if err != nil || !created || d.Revision != 1 {
		t.Fatalf("apply: %v created=%v rev=%v", err, created, d)
	}
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })
	if n := len(h.rt.running("web")); n != 2 {
		t.Fatalf("want 2 running replicas, got %d", n)
	}
	// The image was pulled because it was missing.
	if !h.rt.HasImage("app:v1") {
		t.Fatal("image not pulled")
	}
	h.eventually("DNS answers", func() bool { return len(h.e.Resolve("web.rh.internal.")) == 2 })
	// PORT and private domain are injected.
	env := strings.Join(h.rt.running("web")[0].Spec.Process.Env, " ")
	for _, want := range []string{"PORT=8080", "RH_PRIVATE_DOMAIN=web.rh.internal", "RH_SERVICE_NAME=web"} {
		if !strings.Contains(env, want) {
			t.Errorf("env %q missing %s", env, want)
		}
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	h := newHarness(t, nil, "")
	if _, _, err := h.e.Apply(webSpec("app:v1")); err != nil {
		t.Fatal(err)
	}
	d, created, err := h.e.Apply(webSpec("app:v1"))
	if err != nil || created || d.Revision != 1 {
		t.Fatalf("second identical apply should be a no-op, got rev %d created=%v err=%v", d.Revision, created, err)
	}
}

func TestRollingUpdateDrainsOldDeployment(t *testing.T) {
	h := newHarness(t, nil, "")
	h.e.Apply(webSpec("app:v1"))
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })

	h.e.Apply(webSpec("app:v2"))
	h.eventually("rev 2 ACTIVE", func() bool { return h.status("web", 2) == StatusActive })
	if s := h.status("web", 1); s != StatusRemoved {
		t.Fatalf("rev 1 should be REMOVED, is %s", s)
	}
	// Old replicas linger for the drain window, then go away.
	h.eventually("rev 1 containers retired", func() bool {
		for _, c := range h.rt.running("web") {
			if c.Image == "app:v1" {
				return false
			}
		}
		return len(h.rt.running("web")) == 2
	})
}

func TestFailedDeployKeepsPreviousServing(t *testing.T) {
	h := newHarness(t, nil, "")
	h.e.Apply(webSpec("app:v1"))
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })

	h.prober.mu.Lock()
	h.prober.bad["app:broken"] = true
	h.prober.mu.Unlock()
	h.e.Apply(webSpec("app:broken"))
	h.eventually("rev 2 FAILED", func() bool { return h.status("web", 2) == StatusFailed })

	if s := h.status("web", 1); s != StatusActive {
		t.Fatalf("rev 1 must keep serving, is %s", s)
	}
	h.eventually("broken replicas retired", func() bool {
		for _, c := range h.rt.running("web") {
			if c.Image == "app:broken" {
				return false
			}
		}
		return len(h.rt.running("web")) == 2
	})
	v, _ := h.e.Service("web", false)
	if !strings.Contains(v.Deployments[1].Reason, "not healthy") {
		t.Fatalf("reason = %q", v.Deployments[1].Reason)
	}
}

func TestCrashingDeployFailsWithLogs(t *testing.T) {
	rt := newFakeRuntime()
	rt.exitOnStart["app:crash"] = 1
	h := newHarness(t, rt, "")
	sp := webSpec("app:crash")
	sp.Healthcheck = nil
	sp.DeployTimeoutSeconds = 30
	h.e.Apply(sp)
	h.eventually("rev 1 FAILED", func() bool { return h.status("web", 1) == StatusFailed })
	v, _ := h.e.Service("web", false)
	d := v.Deployments[0]
	if !strings.Contains(d.Reason, "crashed") || len(d.Logs) == 0 || !strings.Contains(d.Logs[0], "DATABASE_URL") {
		t.Fatalf("failed deployment should explain itself: reason=%q logs=%q", d.Reason, d.Logs)
	}
}

func TestActiveRestartsThenCrashes(t *testing.T) {
	h := newHarness(t, nil, "")
	sp := webSpec("app:v1")
	sp.Replicas = 1
	sp.MaxRestarts = 2
	h.e.Apply(sp)
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })

	// One crash: the policy restarts it.
	h.rt.crash(func(c *container.Info) bool { return true }, 1)
	h.eventually("restarted", func() bool {
		r := h.rt.running("web")
		return len(r) == 1 && r[0].State.Restarts == 1
	})
	// Keep crashing until the budget is exhausted.
	h.eventually("rev 1 CRASHED", func() bool {
		h.rt.crash(func(c *container.Info) bool { return true }, 1)
		return h.status("web", 1) == StatusCrashed
	})
	// A crashed deployment keeps its exited container for inspection.
	list, _ := h.rt.List()
	if len(list) != 1 {
		t.Fatalf("want the crashed container kept, have %d", len(list))
	}
	// A new deployment recovers the service.
	h.e.Apply(webSpec("app:v2"))
	h.eventually("rev 2 ACTIVE", func() bool { return h.status("web", 2) == StatusActive })
}

func TestCleanExitNotRestartedOnFailurePolicy(t *testing.T) {
	h := newHarness(t, nil, "")
	sp := webSpec("app:v1")
	sp.Replicas = 1
	h.e.Apply(sp)
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })
	h.rt.crash(func(c *container.Info) bool { return true }, 0)
	time.Sleep(200 * time.Millisecond)
	if n := len(h.rt.running("web")); n != 0 {
		t.Fatalf("exit 0 under on-failure must not restart, running=%d", n)
	}
}

func TestReplicaRemovedOutOfBandIsReplaced(t *testing.T) {
	h := newHarness(t, nil, "")
	h.e.Apply(webSpec("app:v1"))
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })
	victim := h.rt.running("web")[0]
	h.rt.Remove(victim.ID) // someone ran `rh rm -f`
	h.eventually("replaced", func() bool { return len(h.rt.running("web")) == 2 })
}

func TestEngineRestartAdoptsRunningContainers(t *testing.T) {
	rt := newFakeRuntime()
	dir := t.TempDir()
	h := newHarness(t, rt, dir)
	h.e.Apply(webSpec("app:v1"))
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })
	h.stop()
	creates := rt.creates

	// A new engine process with the same state dir and the same node.
	h2 := newHarness(t, rt, dir)
	time.Sleep(200 * time.Millisecond)
	if rt.creates != creates {
		t.Fatalf("restarted engine created %d new containers; it should adopt the running ones", rt.creates-creates)
	}
	if s := h2.status("web", 1); s != StatusActive {
		t.Fatalf("status after restart = %s", s)
	}
}

func TestNewestQueuedDeploymentWins(t *testing.T) {
	rt := newFakeRuntime()
	h := newHarness(t, rt, "")
	h.e.Apply(webSpec("app:v1"))
	h.e.Apply(webSpec("app:v2"))
	h.e.Apply(webSpec("app:v3"))
	h.eventually("rev 3 ACTIVE", func() bool { return h.status("web", 3) == StatusActive })
	for _, rev := range []int{1, 2} {
		if s := h.status("web", rev); s != StatusSkipped && s != StatusFailed && s != StatusRemoved {
			t.Errorf("rev %d status %s", rev, s)
		}
	}
}

func TestPullFailureFailsDeployment(t *testing.T) {
	rt := newFakeRuntime()
	rt.pullErr = errors.New("manifest unknown")
	h := newHarness(t, rt, "")
	h.e.Apply(webSpec("app:nope"))
	h.eventually("rev 1 FAILED", func() bool { return h.status("web", 1) == StatusFailed })
}

func TestRollback(t *testing.T) {
	h := newHarness(t, nil, "")
	h.e.Apply(webSpec("app:v1"))
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })
	h.e.Apply(webSpec("app:v2"))
	h.eventually("rev 2 ACTIVE", func() bool { return h.status("web", 2) == StatusActive })
	d, err := h.e.Rollback("web", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Spec.Image != "app:v1" {
		t.Fatalf("rollback picked %s", d.Spec.Image)
	}
	h.eventually("rev 3 ACTIVE", func() bool { return h.status("web", 3) == StatusActive })
}

func TestDeleteTearsDown(t *testing.T) {
	h := newHarness(t, nil, "")
	h.e.Apply(webSpec("app:v1"))
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("web", 1) == StatusActive })
	if err := h.e.Delete("web"); err != nil {
		t.Fatal(err)
	}
	h.eventually("service gone", func() bool {
		_, err := h.e.Service("web", false)
		list, _ := h.rt.List()
		return err != nil && len(list) == 0
	})
}

func TestVariableReferences(t *testing.T) {
	h := newHarness(t, nil, "")
	h.e.Apply(ServiceSpec{Name: "db", Image: "postgres:17", Port: 5432, Env: map[string]string{"POSTGRES_USER": "lab"}})
	h.e.Apply(ServiceSpec{Name: "api", Image: "api:v1", Env: map[string]string{
		"DATABASE_URL": "postgres://${{db.POSTGRES_USER}}@${{db.RH_PRIVATE_DOMAIN}}:${{db.PORT}}/app",
	}})
	h.eventually("api running", func() bool { return len(h.rt.running("api")) == 1 })
	env := strings.Join(h.rt.running("api")[0].Spec.Process.Env, " ")
	if !strings.Contains(env, "DATABASE_URL=postgres://lab@db.rh.internal:5432/app") {
		t.Fatalf("references not resolved: %s", env)
	}
}

func TestUsageIsMetered(t *testing.T) {
	h := newHarness(t, nil, "")
	sp := webSpec("app:v1")
	sp.Replicas = 1
	h.e.Apply(sp)
	h.eventually("usage sampled", func() bool {
		u := h.e.Usage()
		return len(u) == 1 && u[0].MemoryGBSeconds > 0 && u[0].CostUSD > 0
	})
}

func TestSpecValidation(t *testing.T) {
	cases := map[string]ServiceSpec{
		"bad name":        {Name: "Web_1", Image: "x"},
		"no image":        {Name: "web"},
		"volume replicas": {Name: "db", Image: "x", Replicas: 2, Volume: &Volume{Name: "data", MountPath: "/data"}},
		"public no port":  {Name: "web", Image: "x", PublicPort: 80},
		"bad restart":     {Name: "web", Image: "x", Restart: "sometimes"},
		"check no port":   {Name: "web", Image: "x", Healthcheck: &Healthcheck{Path: "/"}},
	}
	for name, sp := range cases {
		if err := sp.Normalize(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	ok := ServiceSpec{Name: "web", Image: "x", Port: 80, Healthcheck: &Healthcheck{Path: "health"}}
	if err := ok.Normalize(); err != nil {
		t.Fatal(err)
	}
	if ok.Replicas != 1 || ok.Restart != RestartOnFailure || ok.Healthcheck.Type != "http" || ok.Healthcheck.Path != "/health" {
		t.Fatalf("defaults not applied: %+v %+v", ok, ok.Healthcheck)
	}
}

func TestPublicPortConflict(t *testing.T) {
	h := newHarness(t, nil, "")
	h.e.cfg.ListenProxy = nil // never reached: Apply rejects first
	a := ServiceSpec{Name: "svc-a", Image: "x", Port: 80, PublicPort: 18123}
	b := ServiceSpec{Name: "svc-b", Image: "x", Port: 80, PublicPort: 18123}
	if _, _, err := h.e.Apply(a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.e.Apply(b); err == nil {
		t.Fatal("second service on the same public port should be rejected")
	}
}

func TestVolumeServiceIsRecreatedNeverOverlapped(t *testing.T) {
	h := newHarness(t, nil, "")
	db := func(image string) ServiceSpec {
		return ServiceSpec{Name: "db", Image: image, Port: 5432, Volume: &Volume{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
			Healthcheck: &Healthcheck{Type: "tcp", IntervalSeconds: 1}, DeployTimeoutSeconds: 2}
	}
	h.e.Apply(db("postgres:16"))
	h.eventually("rev 1 ACTIVE", func() bool { return h.status("db", 1) == StatusActive })
	h.e.Apply(db("postgres:17"))
	h.eventually("rev 2 ACTIVE", func() bool { return h.status("db", 2) == StatusActive })
	h.rt.mu.Lock()
	peak := h.rt.maxRunning["db"]
	h.rt.mu.Unlock()
	if peak != 1 {
		t.Fatalf("two database instances ran on one volume at the same time (peak %d)", peak)
	}

	// A broken new version: the old one must come back on its own.
	h.prober.mu.Lock()
	h.prober.bad["postgres:broken"] = true
	h.prober.mu.Unlock()
	h.e.Apply(db("postgres:broken"))
	h.eventually("rev 3 FAILED", func() bool { return h.status("db", 3) == StatusFailed })
	h.eventually("rev 2 serving again", func() bool {
		r := h.rt.running("db")
		return len(r) == 1 && r[0].Image == "postgres:17"
	})
}
