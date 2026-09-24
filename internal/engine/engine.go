package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/image"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/network"
)

// Labels the engine puts on every container it owns. Observed state is
// rebuilt from these, so a lost or corrupted state.json never orphans a
// running container.
const (
	LabelService    = "rh.service"
	LabelDeployment = "rh.deployment"
	LabelReplica    = "rh.replica"
)

// Config wires an engine.
type Config struct {
	StateDir string
	Runtime  Runtime
	// Workers is how many services can be reconciled in parallel.
	Workers int
	// Resync is the period of the level-triggered safety-net sync.
	Resync time.Duration
	// Prober checks instance health; nil uses real HTTP/TCP probes.
	Prober Prober
	// Domain is the private DNS suffix: <service>.<Domain>.
	Domain string
	// DNS servers written into containers' resolv.conf.
	DNS []string
	// VolumeDir holds named volumes.
	VolumeDir string
	// ListenProxy opens an edge listener; nil uses network.Listen.
	ListenProxy func(port int) (*network.Proxy, error)
	// BackoffBase is the first restart delay (default 1s).
	BackoffBase time.Duration
	// Meter settings.
	MeterInterval time.Duration
	Prices        Prices
	Logf          func(format string, args ...any)
	Now           func() time.Time
}

// Engine is the provisioning engine for one node.
type Engine struct {
	cfg Config
	rt  Runtime

	mu       sync.Mutex
	services map[string]*Service
	health   map[string]*healthState // container id
	proxies  map[string]*serviceProxy
	stopping map[string]bool // container ids being retired
	dnsIPs   map[string][]string

	queue   *workQueue
	events  *eventBus
	usage   *usageLedger
	metrics *metrics
}

type serviceProxy struct {
	port   int
	proxy  *network.Proxy
	cancel context.CancelFunc
}

type persisted struct {
	Services []*Service `json:"services"`
}

// New loads state and returns an engine ready to Run.
func New(cfg Config) (*Engine, error) {
	if cfg.Runtime == nil {
		return nil, errors.New("engine: runtime is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Resync <= 0 {
		cfg.Resync = time.Second
	}
	if cfg.Domain == "" {
		cfg.Domain = "rh.internal"
	}
	if cfg.Prober == nil {
		cfg.Prober = NetProber{}
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ListenProxy == nil {
		cfg.ListenProxy = func(port int) (*network.Proxy, error) {
			return network.Listen(fmt.Sprintf("0.0.0.0:%d", port))
		}
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = time.Second
	}
	if cfg.MeterInterval <= 0 {
		cfg.MeterInterval = 10 * time.Second
	}
	if cfg.Prices == (Prices{}) {
		cfg.Prices = DefaultPrices
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:      cfg,
		rt:       cfg.Runtime,
		services: map[string]*Service{},
		health:   map[string]*healthState{},
		proxies:  map[string]*serviceProxy{},
		stopping: map[string]bool{},
		dnsIPs:   map[string][]string{},
		queue:    newWorkQueue(),
		events:   newEventBus(2000),
		metrics:  newMetrics(),
	}
	var p persisted
	b, err := os.ReadFile(e.statePath())
	if err == nil {
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("corrupt %s: %w", e.statePath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, s := range p.Services {
		e.services[s.Name] = s
	}
	if e.usage, err = loadUsage(filepath.Join(cfg.StateDir, "usage.json")); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) statePath() string { return filepath.Join(e.cfg.StateDir, "state.json") }

// save persists desired state. Callers hold e.mu.
func (e *Engine) save() {
	var p persisted
	names := make([]string, 0, len(e.services))
	for n := range e.services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p.Services = append(p.Services, e.services[n])
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := image.WriteFileAtomic(e.statePath(), b, 0o644); err != nil {
		e.cfg.Logf("engine: save state: %v", err)
	}
}

// Run starts workers, the resync ticker, the prober and the meter, and
// blocks until ctx is cancelled. Containers keep running after Run returns:
// they belong to their shims, not to the engine process.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	n := len(e.services)
	e.mu.Unlock()
	e.publish(Event{Type: "engine", Message: fmt.Sprintf("engine started with %d services", n)})
	var wg sync.WaitGroup
	for i := 0; i < e.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				key, ok := e.queue.Get()
				if !ok {
					return
				}
				start := time.Now()
				after := e.sync(ctx, key)
				e.metrics.observeSync(time.Since(start))
				e.queue.Done(key)
				if after > 0 {
					e.queue.AddAfter(key, after)
				}
			}
		}()
	}
	wg.Add(3)
	go func() { defer wg.Done(); e.resyncLoop(ctx) }()
	go func() { defer wg.Done(); e.probeLoop(ctx) }()
	go func() { defer wg.Done(); e.meterLoop(ctx) }()

	<-ctx.Done()
	e.queue.Shutdown()
	e.mu.Lock()
	for _, p := range e.proxies {
		p.cancel()
	}
	e.mu.Unlock()
	wg.Wait()
	if err := e.usage.save(); err != nil {
		e.cfg.Logf("engine: save usage: %v", err)
	}
}

func (e *Engine) resyncLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.Resync)
	defer t.Stop()
	e.enqueueAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.enqueueAll()
		}
	}
}

func (e *Engine) enqueueAll() {
	e.mu.Lock()
	names := make([]string, 0, len(e.services))
	for n := range e.services {
		names = append(names, n)
	}
	e.mu.Unlock()
	for _, n := range names {
		e.queue.Add(n)
	}
	// Containers labelled for services we no longer know about.
	if list, err := e.rt.List(); err == nil {
		for _, c := range list {
			if s := c.Labels[LabelService]; s != "" {
				e.mu.Lock()
				_, known := e.services[s]
				e.mu.Unlock()
				if !known {
					e.queue.Add(s)
				}
			}
		}
	}
}

func (e *Engine) publish(ev Event) {
	e.events.Publish(ev)
	msg := ev.Message
	if ev.Service != "" {
		msg = ev.Service + ": " + msg
	}
	e.cfg.Logf("[%s] %s", ev.Type, msg)
}

// ---------------------------------------------------------------------------
// API-facing operations

// Apply declares a service spec. An identical spec is a no-op (PUT is
// idempotent, so clients can retry freely); anything else creates a new
// deployment.
func (e *Engine) Apply(spec ServiceSpec) (*Deployment, bool, error) {
	if err := spec.Normalize(); err != nil {
		return nil, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkPortConflict(spec); err != nil {
		return nil, false, err
	}
	svc := e.services[spec.Name]
	if svc != nil && svc.Deleted {
		return nil, false, fmt.Errorf("service %s is being deleted; retry when it is gone", spec.Name)
	}
	if svc != nil {
		if l := svc.Latest(); l != nil && reflect.DeepEqual(l.Spec, spec) &&
			(l.Status == StatusActive || l.Status == StatusDeploying || l.Status == StatusQueued) {
			return l, false, nil
		}
	}
	if svc == nil {
		svc = &Service{Name: spec.Name, NextRev: 1}
		e.services[spec.Name] = svc
	}
	d := e.newDeployment(svc, spec, "")
	return d, true, nil
}

// newDeployment appends a QUEUED deployment. Caller holds e.mu.
func (e *Engine) newDeployment(svc *Service, spec ServiceSpec, reason string) *Deployment {
	now := e.cfg.Now().UTC()
	d := &Deployment{
		ID:        newID(),
		Service:   svc.Name,
		Revision:  svc.NextRev,
		Spec:      spec,
		Status:    StatusQueued,
		Reason:    reason,
		CreatedAt: now,
		UpdatedAt: now,
	}
	svc.NextRev++
	svc.Deployments = append(svc.Deployments, d)
	// Keep bounded history, never dropping live deployments.
	for len(svc.Deployments) > 20 {
		old := svc.Deployments[0]
		if old.Status == StatusActive || old.Status == StatusDeploying || old.Status == StatusQueued {
			break
		}
		svc.Deployments = svc.Deployments[1:]
	}
	e.save()
	e.metrics.deployments.Add(1)
	e.publish(Event{Service: svc.Name, Deployment: d.ID, Type: "deployment", Message: fmt.Sprintf("rev %d queued (%s)", d.Revision, spec.Image)})
	e.queue.Add(svc.Name)
	return d
}

func (e *Engine) checkPortConflict(spec ServiceSpec) error {
	if spec.PublicPort == 0 {
		return nil
	}
	for name, s := range e.services {
		if name == spec.Name || s.Deleted {
			continue
		}
		for _, d := range []*Deployment{s.Active(), s.Target()} {
			if d != nil && d.Spec.PublicPort == spec.PublicPort {
				return fmt.Errorf("public port %d is already used by service %s", spec.PublicPort, name)
			}
		}
	}
	return nil
}

// Redeploy creates a new deployment from the latest spec (e.g. to pick up a
// moved :latest tag).
func (e *Engine) Redeploy(name string) (*Deployment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	svc := e.services[name]
	if svc == nil || svc.Deleted || svc.Latest() == nil {
		return nil, fmt.Errorf("no such service %s", name)
	}
	return e.newDeployment(svc, svc.Latest().Spec, "redeploy"), nil
}

// Rollback redeploys the spec of an earlier deployment that was once live.
// With an empty id it picks the most recent one other than the current.
func (e *Engine) Rollback(name, id string) (*Deployment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	svc := e.services[name]
	if svc == nil || svc.Deleted {
		return nil, fmt.Errorf("no such service %s", name)
	}
	active := svc.Active()
	var pick *Deployment
	if id != "" {
		pick = svc.Find(id)
		if pick == nil {
			return nil, fmt.Errorf("service %s has no deployment %s", name, id)
		}
	} else {
		for i := len(svc.Deployments) - 1; i >= 0; i-- {
			d := svc.Deployments[i]
			if !d.ActiveAt.IsZero() && d != active {
				pick = d
				break
			}
		}
		if pick == nil {
			return nil, fmt.Errorf("service %s has no earlier successful deployment", name)
		}
	}
	return e.newDeployment(svc, pick.Spec, fmt.Sprintf("rollback to rev %d", pick.Revision)), nil
}

// Delete marks a service for removal; the reconciler tears it down.
func (e *Engine) Delete(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	svc := e.services[name]
	if svc == nil {
		return fmt.Errorf("no such service %s", name)
	}
	svc.Deleted = true
	now := e.cfg.Now().UTC()
	for _, d := range svc.Deployments {
		switch d.Status {
		case StatusActive, StatusDeploying, StatusQueued, StatusCrashed:
			d.Status, d.Reason, d.UpdatedAt = StatusRemoved, "service deleted", now
		}
	}
	e.save()
	e.publish(Event{Service: name, Type: "service", Message: "deleting"})
	e.queue.Add(name)
	return nil
}

// ---------------------------------------------------------------------------
// read models

// InstanceView is one observed container of a service.
type InstanceView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Deployment string    `json:"deployment"`
	Replica    string    `json:"replica"`
	Status     string    `json:"status"`
	Healthy    bool      `json:"healthy"`
	Health     string    `json:"health,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Restarts   int       `json:"restarts"`
	ExitCode   int       `json:"exitCode"`
	OOMKilled  bool      `json:"oomKilled,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	CPUNanos   uint64    `json:"cpuNanos,omitempty"`
	MemBytes   uint64    `json:"memoryBytes,omitempty"`
}

// ServiceView is the API representation of a service.
type ServiceView struct {
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Spec        ServiceSpec    `json:"spec"`
	Active      *Deployment    `json:"active,omitempty"`
	Target      *Deployment    `json:"target,omitempty"`
	Deployments []*Deployment  `json:"deployments"`
	Instances   []InstanceView `json:"instances"`
	PrivateURL  string         `json:"privateUrl,omitempty"`
	PublicURL   string         `json:"publicUrl,omitempty"`
	Deleted     bool           `json:"deleted,omitempty"`
}

// Services returns every service with observed instances.
func (e *Engine) Services(withStats bool) ([]ServiceView, error) {
	list, err := e.rt.List()
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.services))
	for n := range e.services {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ServiceView, 0, len(names))
	for _, n := range names {
		out = append(out, e.viewLocked(e.services[n], list, withStats))
	}
	return out, nil
}

// Service returns one service.
func (e *Engine) Service(name string, withStats bool) (ServiceView, error) {
	list, err := e.rt.List()
	if err != nil {
		return ServiceView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	svc := e.services[name]
	if svc == nil {
		return ServiceView{}, fmt.Errorf("no such service %s", name)
	}
	return e.viewLocked(svc, list, withStats), nil
}

func (e *Engine) viewLocked(svc *Service, list []container.Info, withStats bool) ServiceView {
	// Return copies: the reconciler keeps mutating the originals.
	v := ServiceView{Name: svc.Name, Deleted: svc.Deleted}
	for _, d := range svc.Deployments {
		cp := *d
		v.Deployments = append(v.Deployments, &cp)
		if d == svc.Active() {
			v.Active = &cp
		}
		if d == svc.Target() {
			v.Target = &cp
		}
	}
	if l := svc.Latest(); l != nil {
		v.Spec = l.Spec
		v.Status = l.Status
	}
	if v.Active != nil && v.Target != nil {
		v.Status = StatusActive + " (deploying rev " + fmt.Sprint(v.Target.Revision) + ")"
	}
	if svc.Deleted {
		v.Status = "DELETING"
	}
	spec := v.Spec
	if v.Active != nil {
		spec = v.Active.Spec
	}
	if spec.Port > 0 {
		v.PrivateURL = fmt.Sprintf("http://%s.%s:%d", svc.Name, e.cfg.Domain, spec.Port)
	}
	if spec.PublicPort > 0 {
		v.PublicURL = fmt.Sprintf("http://localhost:%d", spec.PublicPort)
	}
	for _, c := range list {
		if c.Labels[LabelService] != svc.Name {
			continue
		}
		iv := InstanceView{
			ID: c.ID, Name: c.Name, Deployment: c.Labels[LabelDeployment], Replica: c.Labels[LabelReplica],
			Status: c.State.Status, IP: c.IP, Restarts: c.State.Restarts, ExitCode: c.State.ExitCode,
			OOMKilled: c.State.OOMKilled, StartedAt: c.State.StartedAt, FinishedAt: c.State.FinishedAt,
		}
		if h := e.health[c.ID]; h != nil {
			iv.Healthy, iv.Health = h.healthy, h.lastErr
		}
		if d := svc.Find(iv.Deployment); d != nil && c.State.Status == container.StatusRunning && e.healthyLocked(c, d.Spec) {
			iv.Healthy = true
		}
		if e.stopping[c.ID] {
			iv.Status = "stopping"
		}
		if withStats && c.State.Status == container.StatusRunning {
			if s, err := e.rt.Stats(c.ID); err == nil {
				iv.CPUNanos, iv.MemBytes = s.CPUUsageNanos, s.MemoryBytes
			}
		}
		v.Instances = append(v.Instances, iv)
	}
	sort.Slice(v.Instances, func(i, j int) bool {
		if v.Instances[i].Deployment != v.Instances[j].Deployment {
			return v.Instances[i].StartedAt.After(v.Instances[j].StartedAt)
		}
		return v.Instances[i].Replica < v.Instances[j].Replica
	})
	return v
}

// Events returns buffered events after seq.
func (e *Engine) Events(since uint64, service string) []Event { return e.events.Since(since, service) }

// Subscribe streams new events.
func (e *Engine) Subscribe() (chan Event, func()) { return e.events.Subscribe() }

// Resolve answers private DNS: the IPs serving <service>.
func (e *Engine) Resolve(name string) []string {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	name = strings.TrimSuffix(name, "."+e.cfg.Domain)
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.dnsIPs[name]...)
}

// Domain is the private DNS zone.
func (e *Engine) Domain() string { return e.cfg.Domain }

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
