package engine

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aggarwalpulkit596/roundhouse/internal/container"
)

// metrics exposes Prometheus text format by hand: the format is simple
// enough that a dependency would hide more than it helps.
type metrics struct {
	deployments, promotions, failures, restarts atomic.Int64
	probesOK, probesFail                        atomic.Int64
	syncCount                                   atomic.Int64
	syncNanos                                   atomic.Int64

	mu         sync.Mutex
	proxyConns map[string][2]int64 // service -> ok, failed
	containers map[string]containerMetric
}

type containerMetric struct {
	service  string
	cpuNanos uint64
	memBytes uint64
}

func newMetrics() *metrics {
	return &metrics{proxyConns: map[string][2]int64{}, containers: map[string]containerMetric{}}
}

func (m *metrics) observeSync(d time.Duration) {
	m.syncCount.Add(1)
	m.syncNanos.Add(int64(d))
}

func (m *metrics) probe(ok bool) {
	if ok {
		m.probesOK.Add(1)
	} else {
		m.probesFail.Add(1)
	}
}

func (m *metrics) proxied(service string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.proxyConns[service]
	if err == nil {
		v[0]++
	} else {
		v[1]++
	}
	m.proxyConns[service] = v
}

func (m *metrics) setContainer(service, name string, cpu, mem uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers[name] = containerMetric{service, cpu, mem}
}

func (m *metrics) pruneContainers(live map[string]bool, list []container.Info) {
	names := map[string]bool{}
	for _, c := range list {
		if live[c.ID] {
			names[c.Name] = true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for n := range m.containers {
		if !names[n] {
			delete(m.containers, n)
		}
	}
}

// WriteMetrics renders /metrics.
func (e *Engine) WriteMetrics(w io.Writer) {
	m := e.metrics
	counter := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	counter("rh_deployments_created_total", "Deployments created.", m.deployments.Load())
	counter("rh_deployments_promoted_total", "Deployments that became ACTIVE.", m.promotions.Load())
	counter("rh_deployments_failed_total", "Deployments that FAILED or CRASHED.", m.failures.Load())
	counter("rh_instance_restarts_total", "Instance restarts by the restart policy.", m.restarts.Load())
	fmt.Fprintf(w, "# HELP rh_health_probes_total Health probes by result.\n# TYPE rh_health_probes_total counter\n")
	fmt.Fprintf(w, "rh_health_probes_total{result=\"success\"} %d\nrh_health_probes_total{result=\"failure\"} %d\n", m.probesOK.Load(), m.probesFail.Load())
	fmt.Fprintf(w, "# HELP rh_reconcile_seconds Time spent in service syncs.\n# TYPE rh_reconcile_seconds summary\n")
	fmt.Fprintf(w, "rh_reconcile_seconds_sum %f\nrh_reconcile_seconds_count %d\n", float64(m.syncNanos.Load())/1e9, m.syncCount.Load())
	fmt.Fprintf(w, "# HELP rh_workqueue_depth Services waiting to be reconciled.\n# TYPE rh_workqueue_depth gauge\nrh_workqueue_depth %d\n", e.queue.Len())

	m.mu.Lock()
	defer m.mu.Unlock()
	svcs := make([]string, 0, len(m.proxyConns))
	for s := range m.proxyConns {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	fmt.Fprintf(w, "# HELP rh_edge_connections_total Connections through the edge proxy.\n# TYPE rh_edge_connections_total counter\n")
	for _, s := range svcs {
		v := m.proxyConns[s]
		fmt.Fprintf(w, "rh_edge_connections_total{service=%q,result=\"ok\"} %d\n", s, v[0])
		fmt.Fprintf(w, "rh_edge_connections_total{service=%q,result=\"no_backend\"} %d\n", s, v[1])
	}
	names := make([]string, 0, len(m.containers))
	for n := range m.containers {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "# HELP rh_container_cpu_seconds_total Cumulative CPU from cgroups.\n# TYPE rh_container_cpu_seconds_total counter\n")
	for _, n := range names {
		c := m.containers[n]
		fmt.Fprintf(w, "rh_container_cpu_seconds_total{service=%q,container=%q} %f\n", c.service, n, float64(c.cpuNanos)/1e9)
	}
	fmt.Fprintf(w, "# HELP rh_container_memory_bytes Current memory charge from cgroups.\n# TYPE rh_container_memory_bytes gauge\n")
	for _, n := range names {
		c := m.containers[n]
		fmt.Fprintf(w, "rh_container_memory_bytes{service=%q,container=%q} %d\n", c.service, n, c.memBytes)
	}
}
