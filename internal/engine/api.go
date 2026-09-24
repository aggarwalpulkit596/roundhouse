package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aggarwalpulkit596/roundhouse/internal/container"
)

// NodeInfo describes this node's capacity for a scheduler.
type NodeInfo struct {
	Name          string  `json:"name"`
	CPUs          int     `json:"cpus"`
	MemoryMB      int     `json:"memoryMb"`
	AllocatedCPU  float64 `json:"allocatedCpu"`
	AllocatedMB   int     `json:"allocatedMb"`
	Services      int     `json:"services"`
	Instances     int     `json:"instances"`
	EngineVersion string  `json:"engineVersion"`
}

// Node returns capacity and what active deployments have reserved.
func (e *Engine) Node(name string, memTotalMB int) NodeInfo {
	n := NodeInfo{Name: name, CPUs: goruntime.NumCPU(), MemoryMB: memTotalMB, EngineVersion: "0.1"}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.services {
		if s.Deleted {
			continue
		}
		n.Services++
		for _, d := range []*Deployment{s.Active(), s.Target()} {
			if d == nil {
				continue
			}
			n.AllocatedCPU += d.Spec.CPU * float64(d.Spec.Replicas)
			n.AllocatedMB += d.Spec.MemoryMB * d.Spec.Replicas
			n.Instances += d.Spec.Replicas
		}
	}
	return n
}

// Handler returns the engine's HTTP API.
//
//	GET    /v1/node                         capacity (for schedulers)
//	GET    /v1/services                     list
//	PUT    /v1/services/{name}              declare spec (idempotent)
//	GET    /v1/services/{name}              detail with instances
//	DELETE /v1/services/{name}              tear down
//	POST   /v1/services/{name}/redeploy
//	POST   /v1/services/{name}/rollback     {"deployment": "<id>"} optional
//	GET    /v1/services/{name}/logs         ?follow=1&tail=N (NDJSON)
//	GET    /v1/events                       ?since=SEQ&service=S&follow=1 (NDJSON)
//	GET    /v1/usage                        metered CPU/memory and cost
//	GET    /metrics                         Prometheus text format
//	GET    /healthz
func (e *Engine) Handler(nodeName string, memTotalMB int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		e.WriteMetrics(w)
	})
	mux.HandleFunc("GET /v1/node", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, e.Node(nodeName, memTotalMB))
	})
	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, r *http.Request) {
		v, err := e.Services(r.URL.Query().Get("stats") == "1")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	})
	mux.HandleFunc("GET /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		v, err := e.Service(r.PathValue("name"), r.URL.Query().Get("stats") == "1")
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	})
	mux.HandleFunc("PUT /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		var spec ServiceSpec
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid spec: %w", err))
			return
		}
		if spec.Name == "" {
			spec.Name = r.PathValue("name")
		}
		if spec.Name != r.PathValue("name") {
			writeErr(w, http.StatusBadRequest, errors.New("spec name does not match URL"))
			return
		}
		d, created, err := e.Apply(spec)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		code := http.StatusOK
		if created {
			code = http.StatusCreated
		}
		writeJSON(w, code, d)
	})
	mux.HandleFunc("DELETE /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := e.Delete(r.PathValue("name")); err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /v1/services/{name}/redeploy", func(w http.ResponseWriter, r *http.Request) {
		d, err := e.Redeploy(r.PathValue("name"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusCreated, d)
	})
	mux.HandleFunc("POST /v1/services/{name}/rollback", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Deployment string `json:"deployment"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		d, err := e.Rollback(r.PathValue("name"), body.Deployment)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeJSON(w, http.StatusCreated, d)
	})
	mux.HandleFunc("GET /v1/services/{name}/logs", e.handleLogs)
	mux.HandleFunc("GET /v1/events", e.handleEvents)
	mux.HandleFunc("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"prices": e.Prices(), "services": e.Usage()})
	})
	return mux
}

// handleLogs merges the logs of every instance of a service into one NDJSON
// stream, the way a platform's log view shows all replicas together.
func (e *Engine) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	q := r.URL.Query()
	follow := q.Get("follow") == "1"
	tail, _ := strconv.Atoi(q.Get("tail"))
	v, err := e.Service(name, false)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	type line struct {
		container.LogLine
		Instance string `json:"instance"`
	}
	emit := func(inst string) func(container.LogLine) error {
		return func(l container.LogLine) error {
			mu.Lock()
			defer mu.Unlock()
			if err := enc.Encode(line{l, inst}); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
	}
	ctx := r.Context()
	var wg sync.WaitGroup
	started := map[string]bool{}
	startTail := func(inst InstanceView) {
		if started[inst.ID] {
			return
		}
		started[inst.ID] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.rt.Logs(ctx, inst.ID, follow, tail, emit(inst.Name))
		}()
	}
	for _, inst := range v.Instances {
		startTail(inst)
	}
	if follow {
		// Pick up instances created later (restarts, new deployments).
		t := time.NewTicker(time.Second)
		defer t.Stop()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-t.C:
				if v, err := e.Service(name, false); err == nil {
					for _, inst := range v.Instances {
						startTail(inst)
					}
				}
			}
		}
	}
	wg.Wait()
}

func (e *Engine) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	service := q.Get("service")
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	ch, unsub := e.Subscribe()
	defer unsub()
	last := since
	for _, ev := range e.Events(since, service) {
		_ = enc.Encode(ev)
		last = ev.Seq
	}
	if flusher != nil {
		flusher.Flush()
	}
	if q.Get("follow") != "1" {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if ev.Seq <= last || (service != "" && ev.Service != service) {
				continue
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// Serve runs the API on a listener until ctx is done.
func Serve(ctx context.Context, srv *http.Server, serve func() error) error {
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ParseEnvList converts KEY=VALUE strings into a map.
func ParseEnvList(list []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid env %q (want KEY=VALUE)", kv)
		}
		out[k] = v
	}
	return out, nil
}
