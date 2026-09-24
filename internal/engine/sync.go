package engine

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/spec"
)

// sync reconciles one service and returns when it wants to be looked at
// again (0 = only on the next event/resync). It is the only code that
// changes containers, and the work queue guarantees it never runs twice
// concurrently for the same service.
func (e *Engine) sync(ctx context.Context, name string) time.Duration {
	all, err := e.rt.List()
	if err != nil {
		e.cfg.Logf("engine: list containers: %v", err)
		return 2 * time.Second
	}
	var obs []container.Info
	for _, c := range all {
		if c.Labels[LabelService] == name {
			obs = append(obs, c)
		}
	}

	e.mu.Lock()
	svc := e.services[name]
	deleted := svc == nil || svc.Deleted
	e.mu.Unlock()

	if deleted {
		return e.teardown(name, svc, obs)
	}

	e.skipSuperseded(name)

	byDep := map[string][]container.Info{}
	for _, c := range obs {
		byDep[c.Labels[LabelDeployment]] = append(byDep[c.Labels[LabelDeployment]], c)
	}

	next := time.Duration(0)
	soonest := func(d time.Duration) {
		if d > 0 && (next == 0 || d < next) {
			next = d
		}
	}

	if target, active := e.targetAndActive(name); target != nil {
		if target.Spec.Volume != nil {
			// A volume is a single-writer resource: two database processes on
			// one data directory corrupt it (Postgres' own lock file cannot
			// tell, because PIDs from another namespace look stale). So a
			// service with a volume is *recreated*: every older instance is
			// stopped and removed before the new one starts. The cost is a
			// short downtime; the alternative is lost data. If the new
			// deployment fails, maintain() brings the old one back.
			var old []container.Info
			for depID, insts := range byDep {
				if depID != target.ID {
					old = append(old, insts...)
				}
			}
			if len(old) > 0 {
				fresh := 0
				for _, c := range old {
					if !e.isStopping(c.ID) {
						fresh++
					}
					e.retire(name, c)
				}
				if fresh > 0 {
					e.publish(Event{Service: name, Deployment: target.ID, Type: "deployment",
						Message: fmt.Sprintf("rev %d uses volume %s: stopping %d older instance(s) before starting (recreate, not rolling)", target.Revision, target.Spec.Volume.Name, fresh)})
				}
				e.route(name, nil)
				return 200 * time.Millisecond
			}
		}
		soonest(e.rollout(ctx, target, active, byDep[target.ID]))
	}
	target, active := e.targetAndActive(name)
	// During a recreate rollout the old deployment is deliberately down;
	// maintaining it would "repair" it straight back onto the volume.
	recreating := target != nil && target.Spec.Volume != nil
	if active != nil && !recreating {
		soonest(e.maintain(active, byDep[active.ID]))
	}

	// Anything that is neither being rolled out nor serving is retired,
	// after its drain window if it was just replaced.
	now := e.cfg.Now()
	for depID, insts := range byDep {
		if (target != nil && depID == target.ID) || (active != nil && depID == active.ID) {
			continue
		}
		e.mu.Lock()
		var drainUntil time.Time
		keep := false
		if d := e.services[name].Find(depID); d != nil {
			drainUntil = d.DrainUntil
			// Keep a crashed deployment's exited containers (and their logs)
			// for inspection until something replaces it.
			keep = d.Status == StatusCrashed && active == nil && target == nil
		}
		e.mu.Unlock()
		if keep {
			continue
		}
		if drainUntil.After(now) {
			soonest(drainUntil.Sub(now))
			continue
		}
		for _, c := range insts {
			e.retire(name, c)
		}
	}

	e.route(name, obs)
	return next
}

// targetAndActive snapshots the two deployments that matter. The returned
// values are copies: sync must not read shared state without the lock.
func (e *Engine) targetAndActive(name string) (target, active *Deployment) {
	e.mu.Lock()
	defer e.mu.Unlock()
	svc := e.services[name]
	if svc == nil {
		return nil, nil
	}
	if t := svc.Target(); t != nil {
		cp := *t
		target = &cp
	}
	if a := svc.Active(); a != nil {
		cp := *a
		active = &cp
	}
	return target, active
}

// update mutates a deployment under the lock and persists.
func (e *Engine) update(service, id string, fn func(*Deployment)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	svc := e.services[service]
	if svc == nil {
		return
	}
	if d := svc.Find(id); d != nil {
		fn(d)
		d.UpdatedAt = e.cfg.Now().UTC()
		e.save()
	}
}

// skipSuperseded marks queued/deploying deployments older than the newest
// one as SKIPPED: only the latest intent matters.
func (e *Engine) skipSuperseded(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	svc := e.services[name]
	t := svc.Target()
	changed := false
	for _, d := range svc.Deployments {
		if d != t && (d.Status == StatusQueued || d.Status == StatusDeploying) {
			d.Status, d.Reason, d.UpdatedAt = StatusSkipped, fmt.Sprintf("superseded by rev %d", t.Revision), e.cfg.Now().UTC()
			changed = true
			e.publish(Event{Service: name, Deployment: d.ID, Type: "deployment", Message: fmt.Sprintf("rev %d skipped: %s", d.Revision, d.Reason)})
		}
	}
	if changed {
		e.save()
	}
}

// rollout drives a deployment from QUEUED to ACTIVE or FAILED.
func (e *Engine) rollout(ctx context.Context, t, active *Deployment, insts []container.Info) time.Duration {
	now := e.cfg.Now()
	if t.Status == StatusQueued {
		e.update(t.Service, t.ID, func(d *Deployment) { d.Status, d.StartedAt = StatusDeploying, now.UTC() })
		t.Status, t.StartedAt = StatusDeploying, now.UTC()
		e.publish(Event{Service: t.Service, Deployment: t.ID, Type: "deployment", Message: fmt.Sprintf("rev %d deploying", t.Revision)})
	}
	sp := t.Spec

	if !e.rt.HasImage(sp.Image) {
		e.publish(Event{Service: t.Service, Deployment: t.ID, Type: "image", Message: "pulling " + sp.Image})
		pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := e.rt.PullImage(pctx, sp.Image)
		cancel()
		if err != nil {
			e.fail(t, active, "image pull failed: "+err.Error(), insts)
			return 0
		}
		e.publish(Event{Service: t.Service, Deployment: t.ID, Type: "image", Message: "pulled " + sp.Image})
	}

	if now.Sub(t.StartedAt) > time.Duration(sp.DeployTimeoutSeconds)*time.Second {
		e.fail(t, active, fmt.Sprintf("not healthy within %ds", sp.DeployTimeoutSeconds), insts)
		return 0
	}

	byReplica := newestByReplica(insts)
	failures := 0
	healthy := 0
	next := 500 * time.Millisecond
	for r := 0; r < sp.Replicas; r++ {
		c, ok := byReplica[r]
		if !ok {
			if err := e.createInstance(t, r); err != nil {
				e.fail(t, active, fmt.Sprintf("replica %d: %v", r, err), insts)
				return 0
			}
			continue
		}
		failures += c.State.Restarts
		switch c.State.Status {
		case container.StatusRunning:
			e.mu.Lock()
			ok := e.healthyLocked(c, sp)
			e.mu.Unlock()
			if ok {
				healthy++
			}
		case container.StatusExited:
			failures++
			if failures > 3 {
				e.fail(t, active, fmt.Sprintf("replica %d crashed %d times during deploy (last exit %s)", r, failures, exitText(c)), insts)
				return 0
			}
			due := c.State.FinishedAt.Add(e.backoff(c.State.Restarts))
			if now.Before(due) {
				next = minDur(next, due.Sub(now))
				continue
			}
			e.publish(Event{Service: t.Service, Deployment: t.ID, Instance: c.Name, Type: "instance",
				Message: fmt.Sprintf("replica %d exited during deploy (%s), restarting", r, exitText(c))})
			if err := e.rt.Restart(c.ID); err != nil {
				e.fail(t, active, fmt.Sprintf("replica %d: restart: %v", r, err), insts)
				return 0
			}
		case container.StatusCreated:
			if err := e.rt.Start(c.ID); err != nil {
				e.fail(t, active, fmt.Sprintf("replica %d: start: %v", r, err), insts)
				return 0
			}
		}
	}
	if healthy < sp.Replicas {
		return next
	}

	// Every replica is healthy: promote. Order matters for zero downtime:
	// the new deployment becomes ACTIVE (so routing points at it) before the
	// old one's instances are retired, and those are kept for a drain window
	// so in-flight requests finish.
	drain := time.Duration(sp.DrainSeconds) * time.Second
	e.mu.Lock()
	svc := e.services[t.Service]
	if old := svc.Active(); old != nil && old.ID != t.ID {
		old.Status, old.Reason, old.UpdatedAt = StatusRemoved, fmt.Sprintf("replaced by rev %d", t.Revision), now.UTC()
		old.DrainUntil = now.Add(drain).UTC()
	}
	if d := svc.Find(t.ID); d != nil {
		d.Status, d.ActiveAt, d.Reason, d.UpdatedAt = StatusActive, now.UTC(), "", now.UTC()
	}
	e.save()
	e.mu.Unlock()
	e.metrics.promotions.Add(1)
	msg := fmt.Sprintf("rev %d is live (%d/%d healthy, took %s)", t.Revision, healthy, sp.Replicas, now.Sub(t.StartedAt).Round(time.Millisecond))
	if active != nil && sp.Volume == nil {
		msg += fmt.Sprintf("; draining rev %d for %s", active.Revision, drain)
	}
	e.publish(Event{Service: t.Service, Deployment: t.ID, Type: "deployment", Message: msg})
	return 10 * time.Millisecond
}

// fail marks a rollout FAILED. The previous ACTIVE deployment is untouched
// and keeps serving: a bad deploy never takes the service down.
func (e *Engine) fail(t, active *Deployment, reason string, insts []container.Info) {
	logs := e.tailLogs(insts, 20)
	e.update(t.Service, t.ID, func(d *Deployment) { d.Status, d.Reason, d.Logs = StatusFailed, reason, logs })
	e.metrics.failures.Add(1)
	msg := fmt.Sprintf("rev %d failed: %s", t.Revision, reason)
	if active != nil {
		msg += fmt.Sprintf("; rev %d keeps serving", active.Revision)
	}
	e.publish(Event{Service: t.Service, Deployment: t.ID, Type: "deployment", Message: msg})
	e.queue.Add(t.Service) // retire the failed instances promptly
}

// maintain keeps an ACTIVE deployment at its replica count and applies the
// restart policy with exponential backoff (1s, 2s, 4s … 30s), like
// Kubernetes' CrashLoopBackOff.
func (e *Engine) maintain(a *Deployment, insts []container.Info) time.Duration {
	sp := a.Spec
	now := e.cfg.Now()
	byReplica := newestByReplica(insts)
	next := time.Duration(0)
	for r := 0; r < sp.Replicas; r++ {
		c, ok := byReplica[r]
		if !ok {
			e.publish(Event{Service: a.Service, Deployment: a.ID, Type: "instance", Message: fmt.Sprintf("replica %d missing, replacing", r)})
			if err := e.createInstance(a, r); err != nil {
				e.publish(Event{Service: a.Service, Deployment: a.ID, Type: "instance", Message: fmt.Sprintf("replica %d: %v", r, err)})
				next = minDur0(next, 5*time.Second)
			}
			continue
		}
		switch c.State.Status {
		case container.StatusCreated:
			_ = e.rt.Start(c.ID)
		case container.StatusExited:
			if e.isStopping(c.ID) {
				continue
			}
			if sp.Restart == RestartNever || (sp.Restart == RestartOnFailure && c.State.ExitCode == 0) {
				continue
			}
			ranFor := c.State.FinishedAt.Sub(c.State.StartedAt)
			// A replica that ran for a while before dying is not crash
			// looping; only quick deaths count against the budget.
			if c.State.Restarts >= sp.MaxRestarts && ranFor < time.Minute {
				e.update(a.Service, a.ID, func(d *Deployment) {
					d.Status, d.Reason = StatusCrashed, fmt.Sprintf("replica %d exceeded %d restarts (last exit %s)", r, sp.MaxRestarts, exitText(c))
				})
				e.metrics.failures.Add(1)
				e.publish(Event{Service: a.Service, Deployment: a.ID, Instance: c.Name, Type: "deployment",
					Message: fmt.Sprintf("rev %d crashed: replica %d exceeded %d restarts", a.Revision, r, sp.MaxRestarts)})
				return 0
			}
			wait := e.backoff(c.State.Restarts)
			if ranFor > time.Minute {
				wait = e.cfg.BackoffBase
			}
			due := c.State.FinishedAt.Add(wait)
			if now.Before(due) {
				next = minDur0(next, due.Sub(now))
				continue
			}
			e.metrics.restarts.Add(1)
			e.publish(Event{Service: a.Service, Deployment: a.ID, Instance: c.Name, Type: "instance",
				Message: fmt.Sprintf("replica %d exited (%s); restart %d after %s backoff", r, exitText(c), c.State.Restarts+1, wait)})
			if err := e.rt.Restart(c.ID); err != nil {
				e.publish(Event{Service: a.Service, Deployment: a.ID, Instance: c.Name, Type: "instance", Message: "restart failed: " + err.Error()})
				next = minDur0(next, 2*time.Second)
			}
		}
	}
	// Replicas beyond the spec (from an interrupted rollout) are retired.
	for r, c := range byReplica {
		if r >= sp.Replicas {
			e.retire(a.Service, c)
		}
	}
	return next
}

// teardown removes every container of a deleted service, then the service.
func (e *Engine) teardown(name string, svc *Service, obs []container.Info) time.Duration {
	for _, c := range obs {
		e.retire(name, c)
	}
	if len(obs) > 0 {
		return 500 * time.Millisecond
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.proxies[name]; p != nil {
		p.cancel()
		delete(e.proxies, name)
	}
	delete(e.dnsIPs, name)
	if svc != nil {
		delete(e.services, name)
		e.save()
		e.publish(Event{Service: name, Type: "service", Message: "deleted"})
	}
	return 0
}

// retire stops and removes a container in the background. Stopping may
// take a full grace period and must not block the worker.
func (e *Engine) retire(service string, c container.Info) {
	e.mu.Lock()
	if e.stopping[c.ID] {
		e.mu.Unlock()
		return
	}
	e.stopping[c.ID] = true
	grace := 10 * time.Second
	if svc := e.services[service]; svc != nil {
		if d := svc.Find(c.Labels[LabelDeployment]); d != nil {
			grace = time.Duration(d.Spec.StopGraceSeconds) * time.Second
		}
	}
	e.mu.Unlock()
	go func() {
		if err := e.rt.Stop(c.ID, grace); err != nil {
			e.cfg.Logf("engine: stop %s: %v", c.Name, err)
		}
		if err := e.rt.Remove(c.ID); err != nil {
			e.cfg.Logf("engine: remove %s: %v", c.Name, err)
		}
		e.mu.Lock()
		delete(e.stopping, c.ID)
		delete(e.health, c.ID)
		e.mu.Unlock()
		e.publish(Event{Service: service, Deployment: c.Labels[LabelDeployment], Instance: c.Name, Type: "instance", Message: "removed " + c.Name})
		e.queue.Add(service)
	}()
}

func (e *Engine) isStopping(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopping[id]
}

// createInstance creates and starts replica r of deployment d.
func (e *Engine) createInstance(d *Deployment, r int) error {
	sp := d.Spec
	e.mu.Lock()
	env := e.resolveEnvLocked(sp)
	e.mu.Unlock()
	env["RH_SERVICE_NAME"] = sp.Name
	env["RH_DEPLOYMENT_ID"] = d.ID
	env["RH_REPLICA_ID"] = strconv.Itoa(r)
	env["RH_PRIVATE_DOMAIN"] = sp.Name + "." + e.cfg.Domain
	if sp.Port > 0 {
		env["PORT"] = strconv.Itoa(sp.Port)
	}
	o := container.CreateOptions{
		Name:     fmt.Sprintf("%s-%s-%d", sp.Name, d.ID[:6], r),
		Image:    sp.Image,
		Cmd:      sp.Cmd,
		Env:      sortedEnv(env),
		Hostname: fmt.Sprintf("%s-%d", sp.Name, r),
		Network:  container.NetBridge,
		Init:     true,
		DNS:      e.cfg.DNS,
		StopWait: time.Duration(sp.StopGraceSeconds) * time.Second,
		Labels: map[string]string{
			LabelService:    sp.Name,
			LabelDeployment: d.ID,
			LabelReplica:    strconv.Itoa(r),
		},
		Resources: spec.Resources{
			MemoryBytes: int64(sp.MemoryMB) << 20,
			CPUMillis:   int64(sp.CPU * 1000),
			PidsMax:     int64(sp.PidsMax),
		},
	}
	if sp.Volume != nil {
		o.Volumes = []spec.Mount{{Source: filepath.Join(e.cfg.VolumeDir, sp.Volume.Name), Target: sp.Volume.MountPath}}
		if err := mkdirAll(o.Volumes[0].Source); err != nil {
			return err
		}
	}
	id, err := e.rt.Create(o)
	if err != nil {
		return err
	}
	if err := e.rt.Start(id); err != nil {
		return err
	}
	e.publish(Event{Service: sp.Name, Deployment: d.ID, Instance: o.Name, Type: "instance", Message: fmt.Sprintf("started replica %d (%s)", r, o.Name)})
	return nil
}

// refRE matches Railway-style variable references: ${{postgres.PGHOST}}.
var refRE = regexp.MustCompile(`\$\{\{\s*([a-z0-9-]+)\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// resolveEnvLocked expands ${{service.VAR}} against other services' specs.
// RH_PRIVATE_DOMAIN and PORT of the referenced service are always
// available, which is how one service finds another over private
// networking without hard-coding addresses.
func (e *Engine) resolveEnvLocked(sp ServiceSpec) map[string]string {
	out := make(map[string]string, len(sp.Env)+6)
	for k, v := range sp.Env {
		out[k] = refRE.ReplaceAllStringFunc(v, func(m string) string {
			sub := refRE.FindStringSubmatch(m)
			other := e.services[sub[1]]
			if other == nil || other.Latest() == nil {
				return ""
			}
			os := other.Latest().Spec
			if a := other.Active(); a != nil {
				os = a.Spec
			}
			switch sub[2] {
			case "RH_PRIVATE_DOMAIN":
				return os.Name + "." + e.cfg.Domain
			case "PORT":
				return strconv.Itoa(os.Port)
			}
			return os.Env[sub[2]]
		})
	}
	return out
}

// route points the edge proxy and private DNS at the healthy instances of
// the ACTIVE deployment.
func (e *Engine) route(name string, obs []container.Info) {
	e.mu.Lock()
	svc := e.services[name]
	if svc == nil {
		e.mu.Unlock()
		return
	}
	active := svc.Active()
	var backends, ips, running []string
	var sp ServiceSpec
	if active != nil {
		sp = active.Spec
		for _, c := range obs {
			if c.Labels[LabelDeployment] != active.ID || c.State.Status != container.StatusRunning || e.stopping[c.ID] {
				continue
			}
			running = append(running, c.IP)
			if e.healthyLocked(c, sp) {
				ips = append(ips, c.IP)
				if sp.Port > 0 {
					backends = append(backends, net.JoinHostPort(c.IP, strconv.Itoa(sp.Port)))
				}
			}
		}
	}
	if len(ips) == 0 {
		ips = running // better a possibly-unready answer than NXDOMAIN
	}
	sort.Strings(ips)
	sort.Strings(backends)
	e.dnsIPs[name] = ips
	p := e.proxies[name]
	if p != nil && (sp.PublicPort == 0 || p.port != sp.PublicPort) {
		p.cancel()
		delete(e.proxies, name)
		p = nil
	}
	if p == nil && sp.PublicPort > 0 {
		px, err := e.cfg.ListenProxy(sp.PublicPort)
		if err != nil {
			e.mu.Unlock()
			e.publish(Event{Service: name, Type: "routing", Message: fmt.Sprintf("cannot listen on public port %d: %v", sp.PublicPort, err)})
			return
		}
		svcName := name
		px.OnConn = func(backend string, err error) { e.metrics.proxied(svcName, err) }
		ctx, cancel := context.WithCancel(context.Background())
		go px.Serve(ctx)
		p = &serviceProxy{port: sp.PublicPort, proxy: px, cancel: cancel}
		e.proxies[name] = p
		e.publish(Event{Service: name, Type: "routing", Message: fmt.Sprintf("listening on :%d", sp.PublicPort)})
	}
	changed := false
	if p != nil && strings.Join(p.proxy.Backends(), ",") != strings.Join(backends, ",") {
		p.proxy.SetBackends(backends)
		changed = true
	}
	e.mu.Unlock()
	if changed {
		e.publish(Event{Service: name, Type: "routing", Message: fmt.Sprintf("edge backends: [%s]", strings.Join(backends, " "))})
	}
}

// newestByReplica keeps the newest container for each replica index.
func newestByReplica(insts []container.Info) map[int]container.Info {
	out := map[int]container.Info{}
	for _, c := range insts {
		r, err := strconv.Atoi(c.Labels[LabelReplica])
		if err != nil {
			continue
		}
		if cur, ok := out[r]; !ok || c.Created.After(cur.Created) {
			out[r] = c
		}
	}
	return out
}

// backoff doubles from BackoffBase per restart, capped at 30 bases.
func (e *Engine) backoff(restarts int) time.Duration {
	base := e.cfg.BackoffBase
	limit := 30 * base
	d := base
	for i := 0; i < restarts && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return d
}

func exitText(c container.Info) string {
	s := fmt.Sprintf("code %d", c.State.ExitCode)
	if c.State.OOMKilled {
		s += ", OOMKilled"
	}
	if c.State.Error != "" {
		s += ", " + c.State.Error
	}
	return s
}

func minDur(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}

func minDur0(a, b time.Duration) time.Duration {
	if a == 0 || b < a {
		return b
	}
	return a
}

// tailLogs captures the last n output lines of the given containers, so a
// failed deployment explains itself after its containers are gone.
func (e *Engine) tailLogs(insts []container.Info, n int) []string {
	var out []string
	for _, c := range insts {
		_ = e.rt.Logs(context.Background(), c.ID, false, n, func(l container.LogLine) error {
			out = append(out, c.Name+" | "+l.Line)
			return nil
		})
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }
