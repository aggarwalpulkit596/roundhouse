package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
)

// Prober performs one health check against an instance.
type Prober interface {
	Probe(ctx context.Context, ip string, port int, hc Healthcheck) error
}

// NetProber does real HTTP/TCP checks over the bridge network.
type NetProber struct{}

var probeClient = &http.Client{
	// Never follow redirects to somewhere else, and never use a proxy for
	// private addresses.
	Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Probe implements Prober.
func (NetProber) Probe(ctx context.Context, ip string, port int, hc Healthcheck) error {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	switch hc.Type {
	case "tcp":
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	case "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+hc.Path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "roundhouse-healthcheck")
		resp, err := probeClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return fmt.Errorf("GET %s: %s", hc.Path, resp.Status)
		}
	}
	return nil
}

// healthState tracks one instance. A single success marks it healthy; three
// consecutive failures mark it unhealthy (hysteresis avoids flapping).
type healthState struct {
	healthy   bool
	fails     int
	lastProbe time.Time
	lastErr   string
	inflight  bool
}

const unhealthyAfter = 3

// healthyLocked decides whether an instance may receive traffic. Without a
// healthcheck, "running for a second" is the best signal available.
func (e *Engine) healthyLocked(c container.Info, sp ServiceSpec) bool {
	if c.State.Status != container.StatusRunning {
		return false
	}
	if sp.Healthcheck == nil || sp.Healthcheck.Type == "none" {
		return e.cfg.Now().Sub(c.State.StartedAt) >= time.Second
	}
	h := e.health[c.ID]
	return h != nil && h.healthy
}

// probeLoop checks every running engine-owned instance at its configured
// interval, in parallel, and wakes the owning service when health changes.
func (e *Engine) probeLoop(ctx context.Context) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := e.rt.List()
		if err != nil {
			continue
		}
		now := e.cfg.Now()
		seen := map[string]bool{}
		for _, c := range list {
			svcName := c.Labels[LabelService]
			if svcName == "" || c.State.Status != container.StatusRunning {
				continue
			}
			seen[c.ID] = true
			e.mu.Lock()
			svc := e.services[svcName]
			var d *Deployment
			if svc != nil {
				d = svc.Find(c.Labels[LabelDeployment])
			}
			if d == nil || d.Spec.Healthcheck == nil || d.Spec.Healthcheck.Type == "none" || e.stopping[c.ID] {
				e.mu.Unlock()
				continue
			}
			hc, port := *d.Spec.Healthcheck, d.Spec.Port
			h := e.health[c.ID]
			if h == nil {
				h = &healthState{}
				e.health[c.ID] = h
			}
			due := h.lastProbe.Add(time.Duration(hc.IntervalSeconds) * time.Second)
			if h.inflight || now.Before(due) {
				e.mu.Unlock()
				continue
			}
			h.inflight, h.lastProbe = true, now
			e.mu.Unlock()

			wg.Add(1)
			go func(c container.Info) {
				defer wg.Done()
				pctx, cancel := context.WithTimeout(ctx, time.Duration(hc.TimeoutSeconds)*time.Second)
				err := e.cfg.Prober.Probe(pctx, c.IP, port, hc)
				cancel()
				e.recordProbe(svcName, c, err)
			}(c)
		}
		// Forget instances that are gone.
		e.mu.Lock()
		for id, h := range e.health {
			if !seen[id] && !h.inflight {
				delete(e.health, id)
			}
		}
		e.mu.Unlock()
	}
}

func (e *Engine) recordProbe(service string, c container.Info, err error) {
	e.mu.Lock()
	h := e.health[c.ID]
	if h == nil {
		e.mu.Unlock()
		return
	}
	h.inflight = false
	was := h.healthy
	if err == nil {
		h.fails, h.lastErr, h.healthy = 0, "", true
	} else {
		h.fails++
		h.lastErr = err.Error()
		if h.fails >= unhealthyAfter {
			h.healthy = false
		}
	}
	now := h.healthy
	e.mu.Unlock()
	e.metrics.probe(err == nil)
	if was != now {
		state := "healthy"
		if !now {
			state = "unhealthy: " + h.lastErr
		}
		e.publish(Event{Service: service, Deployment: c.Labels[LabelDeployment], Instance: c.Name, Type: "health", Message: c.Name + " is " + state})
		e.queue.Add(service)
	}
}
