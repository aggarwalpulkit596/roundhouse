// hello is a tiny web service used to exercise Roundhouse: builds, deploys,
// health checks, rolling updates, private networking and crash handling.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	version := os.Getenv("VERSION")
	if version == "" {
		version = "dev"
	}
	host, _ := os.Hostname()
	var ready atomic.Bool
	var requests atomic.Int64

	// Simulate a slow boot so health-gated rollouts are visible.
	if d, err := time.ParseDuration(os.Getenv("BOOT_DELAY")); err == nil {
		log.Printf("booting for %s", d)
		time.Sleep(d)
	}
	if os.Getenv("CRASH_ON_BOOT") != "" {
		log.Fatalf("refusing to start: CRASH_ON_BOOT=%s", os.Getenv("CRASH_ON_BOOT"))
	}
	ready.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		fmt.Fprintf(w, "hello from %s (version %s, request %d)\n", host, version, n)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	// /env shows what the platform injected (PORT, RH_*), minus secrets.
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "RH_") || strings.HasPrefix(kv, "PORT=") || strings.HasPrefix(kv, "VERSION=") || strings.HasPrefix(kv, "UPSTREAM") {
				fmt.Fprintln(w, kv)
			}
		}
	})
	// /upstream calls another service over private networking.
	mux.HandleFunc("/upstream", func(w http.ResponseWriter, r *http.Request) {
		u := os.Getenv("UPSTREAM_URL")
		if u == "" {
			http.Error(w, "UPSTREAM_URL not set", http.StatusNotFound)
			return
		}
		resp, err := http.Get(u)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		fmt.Fprintf(w, "%s -> %s: ", host, u)
		_, _ = fmt.Fprint(w, readAll(resp))
	})
	// /crash exits non-zero, to watch the restart policy work.
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "crashing")
		go func() { time.Sleep(50 * time.Millisecond); os.Exit(3) }()
	})
	// /burn allocates memory, to watch the cgroup OOM killer work.
	mux.HandleFunc("/burn", func(w http.ResponseWriter, r *http.Request) {
		var hog [][]byte
		for i := 0; i < 4096; i++ {
			b := make([]byte, 1<<20)
			for j := range b {
				b[j] = 1
			}
			hog = append(hog, b)
		}
		fmt.Fprintln(w, len(hog))
	})

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		s := <-sig
		log.Printf("received %s, draining", s)
		_ = srv.Close()
	}()
	log.Printf("hello %s listening on :%s", version, port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Printf("served %d requests, bye", requests.Load())
}

func readAll(resp *http.Response) string {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}
