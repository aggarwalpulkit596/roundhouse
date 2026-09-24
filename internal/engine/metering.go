package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/image"
)

// Prices are per-minute rates, the way usage-based platforms bill persistent
// containers: you pay for the CPU time and memory you actually consume,
// sampled from cgroups, not for the size of the box you reserved.
type Prices struct {
	VCPUPerMinute     float64 `json:"vcpuPerMinute"`
	MemoryGBPerMinute float64 `json:"memoryGbPerMinute"`
}

// DefaultPrices are illustrative ($20 per vCPU-month and $10 per GB-month
// of 30 days). Override them with `rh daemon --price-vcpu/--price-mem`.
var DefaultPrices = Prices{
	VCPUPerMinute:     20.0 / (30 * 24 * 60),
	MemoryGBPerMinute: 10.0 / (30 * 24 * 60),
}

// Usage is accumulated consumption for one service.
type Usage struct {
	Service         string    `json:"service"`
	CPUSeconds      float64   `json:"cpuSeconds"`
	MemoryGBSeconds float64   `json:"memoryGbSeconds"`
	Samples         int64     `json:"samples"`
	Since           time.Time `json:"since"`
	LastSample      time.Time `json:"lastSample"`
	// Cost is filled in on read from the current prices.
	CostUSD float64 `json:"costUsd"`
}

type usageLedger struct {
	mu   sync.Mutex
	path string
	// By service.
	Services map[string]*Usage `json:"services"`
	// prevCPU remembers the last cumulative cgroup CPU reading per container
	// so each sample adds only the delta. Not persisted: after a restart the
	// first sample only re-primes it, so no usage is ever double counted.
	prevCPU map[string]uint64
}

func loadUsage(path string) (*usageLedger, error) {
	l := &usageLedger{path: path, Services: map[string]*Usage{}, prevCPU: map[string]uint64{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, err
	}
	if l.Services == nil {
		l.Services = map[string]*Usage{}
	}
	return l, nil
}

func (l *usageLedger) save() error {
	l.mu.Lock()
	b, err := json.MarshalIndent(l, "", "  ")
	l.mu.Unlock()
	if err != nil {
		return err
	}
	return image.WriteFileAtomic(l.path, b, 0o644)
}

// sample adds one observation for a container. cpuNanos is the cgroup's
// cumulative counter; memBytes the current charge; dt the sampling period.
func (l *usageLedger) sample(service, id string, cpuNanos, memBytes uint64, dt time.Duration, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.Services[service]
	if u == nil {
		u = &Usage{Service: service, Since: now.UTC()}
		l.Services[service] = u
	}
	if prev, ok := l.prevCPU[id]; ok && cpuNanos >= prev {
		u.CPUSeconds += float64(cpuNanos-prev) / 1e9
	}
	l.prevCPU[id] = cpuNanos
	u.MemoryGBSeconds += float64(memBytes) / (1 << 30) * dt.Seconds()
	u.Samples++
	u.LastSample = now.UTC()
}

func (l *usageLedger) forget(live map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.prevCPU {
		if !live[id] {
			delete(l.prevCPU, id)
		}
	}
}

// Usage returns per-service usage with cost at current prices.
func (e *Engine) Usage() []Usage {
	e.usage.mu.Lock()
	defer e.usage.mu.Unlock()
	out := make([]Usage, 0, len(e.usage.Services))
	for _, u := range e.usage.Services {
		cp := *u
		cp.CostUSD = cp.CPUSeconds/60*e.cfg.Prices.VCPUPerMinute + cp.MemoryGBSeconds/60*e.cfg.Prices.MemoryGBPerMinute
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Prices returns the configured rates.
func (e *Engine) Prices() Prices { return e.cfg.Prices }

func (e *Engine) meterLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.MeterInterval)
	defer t.Stop()
	e.meterOnce(e.cfg.MeterInterval)
	saves := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.meterOnce(e.cfg.MeterInterval)
		if saves++; saves%6 == 0 {
			if err := e.usage.save(); err != nil {
				e.cfg.Logf("engine: save usage: %v", err)
			}
		}
	}
}

func (e *Engine) meterOnce(dt time.Duration) {
	list, err := e.rt.List()
	if err != nil {
		return
	}
	now := e.cfg.Now()
	live := map[string]bool{}
	for _, c := range list {
		svc := c.Labels[LabelService]
		if svc == "" || c.State.Status != container.StatusRunning {
			continue
		}
		s, err := e.rt.Stats(c.ID)
		if err != nil {
			continue
		}
		live[c.ID] = true
		e.usage.sample(svc, c.ID, s.CPUUsageNanos, s.MemoryBytes, dt, now)
		e.metrics.setContainer(svc, c.Name, s.CPUUsageNanos, s.MemoryBytes)
	}
	e.usage.forget(live)
	e.metrics.pruneContainers(live, list)
}
