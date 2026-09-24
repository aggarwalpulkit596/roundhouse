//go:build integration

// Package integration exercises Roundhouse against a real kernel: every
// test here creates namespaces, cgroups, overlay mounts and veth pairs. Run
// as root:
//
//	sudo env "PATH=$PATH" go test -tags integration -v ./integration/
//
// RH_TEST_IMAGE overrides the base image (default: busybox from Google's
// Docker Hub mirror, which avoids Docker Hub's anonymous rate limit).
package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	rhBin   string
	rhRoot  string
	baseImg = "mirror.gcr.io/library/busybox:latest"
)

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Println("integration tests need root; skipping")
		os.Exit(0)
	}
	if v := os.Getenv("RH_TEST_IMAGE"); v != "" {
		baseImg = v
	}
	dir, err := os.MkdirTemp("", "rh-it-")
	if err != nil {
		panic(err)
	}
	rhBin = filepath.Join(dir, "rh")
	rhRoot = filepath.Join(dir, "root")
	build := exec.Command("go", "build", "-o", rhBin, "../cmd/rh")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	if out, err := rh(nil, "pull", baseImg); err != nil {
		fmt.Println(out)
		panic(err)
	}
	code := m.Run()
	// Remove everything we created so repeated runs start clean.
	if ids, err := rh(nil, "ps", "-a", "-q"); err == nil {
		for _, id := range strings.Fields(ids) {
			_, _ = rh(nil, "rm", "-f", id)
		}
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// rh runs the CLI and returns combined output.
func rh(env []string, args ...string) (string, error) {
	cmd := exec.Command(rhBin, args...)
	cmd.Env = append(append(os.Environ(), "RH_ROOT="+rhRoot, "RH_HOST=unix://"+filepath.Join(rhRoot, "api.sock")), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustRh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := rh(nil, args...)
	if err != nil {
		t.Fatalf("rh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

func TestIsolation(t *testing.T) {
	out := mustRh(t, "run", "--rm", "--hostname", "box", baseImg, "sh", "-c",
		`echo "pid=$$ host=$(hostname)"; ps -o pid= | wc -l; grep CapEff /proc/self/status; cat /proc/self/cgroup | tail -1`)
	for _, want := range []string{"pid=1 host=box", "CapEff:\t00000000a80425fb", "0::/"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Only sh, ps and wc exist in this PID namespace.
	lines := strings.Split(out, "\n")
	if n := strings.TrimSpace(lines[1]); n > "4" || len(n) > 1 {
		t.Errorf("the container should see only its own processes, saw %s", n)
	}
	// The host's filesystem is not reachable after pivot_root.
	if out, _ := rh(nil, "run", "--rm", baseImg, "ls", rhRoot); !strings.Contains(out, "No such file") {
		t.Errorf("host path visible inside the container: %s", out)
	}
}

func TestNonRootUserHasNoCapabilities(t *testing.T) {
	out := mustRh(t, "run", "--rm", "-u", "65534:65534", baseImg, "sh", "-c", "id -u; grep CapEff /proc/self/status")
	if !strings.Contains(out, "65534") || !strings.Contains(out, "CapEff:\t0000000000000000") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestReadOnlyRootfs(t *testing.T) {
	out, err := rh(nil, "run", "--rm", "--read-only", baseImg, "touch", "/x")
	if err == nil || !strings.Contains(out, "Read-only file system") {
		t.Fatalf("write succeeded on a read-only rootfs: %v %s", err, out)
	}
}

func TestMemoryLimitOOMKills(t *testing.T) {
	_, err := rh(nil, "run", "--name", "oom", "-m", "24m", baseImg, "sh", "-c", "x=a; while true; do x=$x$x; done")
	if exitCode(err) != 137 {
		t.Fatalf("want exit 137 (SIGKILL by the OOM killer), got %v", err)
	}
	ps := mustRh(t, "ps", "-a")
	if !strings.Contains(ps, "OOMKilled") {
		t.Fatalf("ps should report OOMKilled:\n%s", ps)
	}
	mustRh(t, "rm", "oom")
}

func TestPidsLimitStopsForkBomb(t *testing.T) {
	out, _ := rh(nil, "run", "--rm", "--pids", "16", baseImg, "sh", "-c", "for i in $(seq 1 40); do sleep 5 & done; wait")
	if !strings.Contains(out, "Resource temporarily unavailable") && !strings.Contains(out, "can't fork") {
		t.Fatalf("fork bomb was not contained:\n%s", out)
	}
}

func TestInitReapsAndForwardsSignals(t *testing.T) {
	// Without --init, sh as PID 1 ignores SIGTERM; with it, the child exits.
	id := mustRh(t, "run", "-d", "--init", baseImg, "sleep", "300")
	start := time.Now()
	mustRh(t, "stop", "-t", "5s", id)
	if time.Since(start) > 3*time.Second {
		t.Fatal("SIGTERM was not forwarded by the init")
	}
	mustRh(t, "rm", id)
}

func TestBridgeNetworkingAndPublishedPort(t *testing.T) {
	web := mustRh(t, "run", "-d", "--name", "it-web", "-p", "18181:8080", baseImg, "sh", "-c",
		"mkdir -p /w && echo roundhouse > /w/index.html && exec httpd -f -p 8080 -h /w")
	defer rh(nil, "rm", "-f", web)
	var ip string
	for _, l := range strings.Split(mustRh(t, "inspect", "it-web"), "\n") {
		if strings.Contains(l, `"ip":`) {
			ip = strings.Trim(strings.TrimSpace(strings.SplitN(l, ":", 2)[1]), `",`)
		}
	}
	if ip == "" {
		t.Fatal("no IP")
	}
	waitHTTP(t, "http://127.0.0.1:18181/", "roundhouse")
	out := mustRh(t, "run", "--rm", baseImg, "wget", "-qO-", "http://"+ip+":8080/")
	if out != "roundhouse" {
		t.Fatalf("container-to-container over the bridge: %q", out)
	}
}

func TestExecJoinsNamespacesAndCgroup(t *testing.T) {
	id := mustRh(t, "run", "-d", "--hostname", "exec-target", baseImg, "sleep", "300")
	defer rh(nil, "rm", "-f", id)
	out := mustRh(t, "exec", id, "sh", "-c", "hostname; ps -o pid,comm | grep -c sleep")
	if !strings.Contains(out, "exec-target") || !strings.Contains(out, "1") {
		t.Fatalf("exec did not land in the container:\n%s", out)
	}
	_, err := rh(nil, "exec", id, "sh", "-c", "exit 42")
	if exitCode(err) != 42 {
		t.Fatalf("exit status not propagated: %v", err)
	}
}

func TestDetachedLogsAndRestart(t *testing.T) {
	id := mustRh(t, "run", "-d", baseImg, "sh", "-c", "echo out; echo err >&2; sleep 300")
	defer rh(nil, "rm", "-f", id)
	time.Sleep(300 * time.Millisecond)
	if out := mustRh(t, "logs", id); !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Fatalf("logs:\n%s", out)
	}
	mustRh(t, "stop", "-t", "1s", id)
	mustRh(t, "start", id)
	if ps := mustRh(t, "ps"); !strings.Contains(ps, id[:12]) {
		t.Fatalf("restarted container not running:\n%s", ps)
	}
}

func TestBuildCacheAndReproducibility(t *testing.T) {
	ctx := t.TempDir()
	os.WriteFile(filepath.Join(ctx, "app.sh"), []byte("#!/bin/sh\necho built-by-roundhouse\n"), 0o755)
	os.WriteFile(filepath.Join(ctx, "Railfile"), []byte(fmt.Sprintf(`
FROM %s AS gen
RUN echo generated > /gen.txt
FROM %s
COPY app.sh /usr/local/bin/app
COPY --from=gen /gen.txt /gen.txt
RUN chmod +x /usr/local/bin/app && mkdir -p /data
CMD ["app"]
`, baseImg, baseImg)), 0o644)
	env := []string{"SOURCE_DATE_EPOCH=1700000000"}
	out1, err := rh(env, "build", "-t", "it/app:1", ctx)
	if err != nil {
		t.Fatalf("%v\n%s", err, out1)
	}
	out2, err := rh(env, "build", "-t", "it/app:2", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "4 cached") {
		t.Fatalf("second build should be fully cached:\n%s", out2)
	}
	id := func(s string) string {
		i := strings.Index(s, "(")
		return s[i+1 : i+13]
	}
	if id(out1[strings.LastIndex(out1, "built"):]) != id(out2[strings.LastIndex(out2, "built"):]) {
		t.Fatalf("SOURCE_DATE_EPOCH builds should produce identical image IDs:\n%s\n%s", out1, out2)
	}
	if out := mustRh(t, "run", "--rm", "it/app:1"); out != "built-by-roundhouse" {
		t.Fatalf("run built image: %q", out)
	}
	// A changed input invalidates only the steps after it.
	os.WriteFile(filepath.Join(ctx, "app.sh"), []byte("#!/bin/sh\necho changed\n"), 0o755)
	out3 := mustRh(t, "build", "-t", "it/app:3", ctx)
	if !strings.Contains(out3, "1 cached") {
		t.Fatalf("only steps before the changed COPY should be cached:\n%s", out3)
	}
}

func TestRegistryPushPull(t *testing.T) {
	reg := exec.Command(rhBin, "registry", "-q", "--listen", "127.0.0.1:15000", "--root", t.TempDir())
	if err := reg.Start(); err != nil {
		t.Fatal(err)
	}
	defer reg.Process.Kill()
	waitHTTP(t, "http://127.0.0.1:15000/v2/", "{}")
	mustRh(t, "push", baseImg, "127.0.0.1:15000/it/base:1")
	again := mustRh(t, "push", baseImg, "127.0.0.1:15000/it/base:2")
	if !strings.Contains(again, "already exists") {
		t.Fatalf("second push should skip existing blobs:\n%s", again)
	}
	other := t.TempDir()
	cmd := exec.Command(rhBin, "pull", "127.0.0.1:15000/it/base:2")
	cmd.Env = append(os.Environ(), "RH_ROOT="+other)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
}

func TestDaemonZeroDowntimeRolloutAndFailedDeploy(t *testing.T) {
	sock := filepath.Join(rhRoot, "api.sock")
	d := exec.Command(rhBin, "daemon", "--socket", sock, "--meter", "1s", "--http", "127.0.0.1:17070")
	d.Env = append(os.Environ(), "RH_ROOT="+rhRoot)
	logf, _ := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	d.Stdout, d.Stderr = logf, logf
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		rh(nil, "svc", "rm", "it-web")
		time.Sleep(2 * time.Second)
		d.Process.Signal(os.Interrupt)
		d.Wait()
	}()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	spec := `sh -c mkdir${IFS}-p${IFS}/w&&echo${IFS}$VERSION>/w/index.html&&exec${IFS}httpd${IFS}-f${IFS}-p${IFS}$PORT${IFS}-h${IFS}/w`
	deploy := func(version string, extra ...string) (string, error) {
		args := append([]string{"deploy", "-i", baseImg, "--port", "8080", "--public", "18282", "--replicas", "2",
			"--health", "/index.html", "--drain", "1", "--cmd", spec, "-e", "VERSION=" + version}, extra...)
		return rh(nil, append(args, "it-web")...)
	}
	if out, err := deploy("v1"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	waitHTTP(t, "http://127.0.0.1:18282/", "v1")

	// Hammer the edge while rolling to v2; not one request may fail.
	var ok, bad atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
		for ctx.Err() == nil {
			resp, err := c.Get("http://127.0.0.1:18282/")
			if err != nil || resp.StatusCode != 200 {
				bad.Add(1)
			} else {
				ok.Add(1)
			}
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}()
	if out, err := deploy("v2"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	waitHTTP(t, "http://127.0.0.1:18282/", "v2")
	time.Sleep(1500 * time.Millisecond) // through the drain window
	cancel()
	<-done
	if bad.Load() != 0 || ok.Load() < 50 {
		t.Fatalf("rollout dropped requests: ok=%d failed=%d", ok.Load(), bad.Load())
	}

	// A deploy that never becomes healthy fails and v2 keeps serving.
	out, err := deploy("v3", "--health", "/does-not-exist", "--timeout", "5")
	if err == nil || !strings.Contains(out, "FAILED") {
		t.Fatalf("bad deploy should fail:\n%s", out)
	}
	waitHTTP(t, "http://127.0.0.1:18282/", "v2")

	// Private DNS resolves the service to its healthy instances.
	lookup := mustRh(t, "run", "--rm", "--dns", "10.88.0.1", baseImg, "nslookup", "it-web.rh.internal", "10.88.0.1")
	if strings.Count(lookup, "Address") < 3 { // server + 2 answers
		t.Fatalf("private DNS:\n%s", lookup)
	}
	if usage := mustRh(t, "usage"); !strings.Contains(usage, "it-web") {
		t.Fatalf("usage not metered:\n%s", usage)
	}

	// The dashboard is served, and mutations need the CSRF header.
	waitHTTP(t, "http://127.0.0.1:17070/", "Roundhouse")
	req, _ := http.NewRequest(http.MethodDelete, "http://127.0.0.1:17070/v1/services/it-web", nil)
	if resp, err := noProxy.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE without X-Requested-By should be refused: %v %v", err, resp)
	}

	// Build from a folder and deploy, the way the dashboard's "Git repo or
	// folder" form does.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM "+baseImg+"\nRUN mkdir -p /w && echo built-by-api > /w/index.html\nCMD [\"sh\", \"-c\", \"exec httpd -f -p $PORT -h /w\"]\n"), 0o644)
	body := fmt.Sprintf(`{"source":%q,"spec":{"name":"it-built","port":8080,"publicPort":18383,"healthcheck":{"path":"/"}}}`, src)
	req, _ = http.NewRequest(http.MethodPost, "http://127.0.0.1:17070/v1/builds", strings.NewReader(body))
	req.Header.Set("X-Requested-By", "test")
	resp, err := noProxy.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("start build: %v %s", err, b)
	}
	resp.Body.Close()
	waitHTTP(t, "http://127.0.0.1:18383/", "built-by-api")
	rh(nil, "svc", "rm", "it-built")
}

var noProxy = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}

func waitHTTP(t *testing.T, url, want string) {
	t.Helper()
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	var last string
	for i := 0; i < 100; i++ {
		resp, err := c.Get(url)
		if err == nil {
			b, _ := io.ReadAll(bufio.NewReader(resp.Body))
			resp.Body.Close()
			last = string(b)
			if strings.Contains(last, want) {
				return
			}
		} else {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never returned %q (last: %s)", url, want, last)
}
