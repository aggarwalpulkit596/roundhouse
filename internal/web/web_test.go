package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aggarwalpulkit596/roundhouse/internal/engine"
)

// okAPI stands in for the engine: it answers 200 to anything.
var okAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
})

func do(h http.Handler, method, target, host string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader("{}"))
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDashboardServesUIWithoutAuth(t *testing.T) {
	h := Handler(Config{API: okAPI, Token: "secret-token-123456"})
	rec := do(h, http.MethodGet, "/", "10.0.0.5:7070", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Roundhouse") {
		t.Fatalf("index: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("static pages should carry a CSP")
	}
}

func TestTokenRequiredForAPI(t *testing.T) {
	const tok = "secret-token-123456"
	h := Handler(Config{API: okAPI, Token: tok})
	if rec := do(h, http.MethodGet, "/v1/services", "10.0.0.5:7070", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/v1/services", "", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/v1/services", "", map[string]string{"Authorization": "Bearer " + tok}); rec.Code != http.StatusOK {
		t.Fatalf("bearer token: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/v1/services", "", map[string]string{"Cookie": cookieName + "=" + tok}); rec.Code != http.StatusOK {
		t.Fatalf("cookie token: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/metrics", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics must need the token too: %d", rec.Code)
	}
}

func TestLoginSetsHttpOnlyCookie(t *testing.T) {
	const tok = "secret-token-123456"
	h := Handler(Config{API: okAPI, Token: tok})
	rec := do(h, http.MethodGet, "/login?token="+tok, "", nil)
	c := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || len(c) != 1 || c[0].Value != tok || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("login: %d %+v", rec.Code, c)
	}
	if rec := do(h, http.MethodGet, "/login?token=nope", "", nil); len(rec.Result().Cookies()) != 0 {
		t.Fatal("a wrong token must not set a cookie")
	}
}

func TestMutationsNeedCSRFHeader(t *testing.T) {
	h := Handler(Config{API: okAPI, LoopbackOnly: true})
	if rec := do(h, http.MethodPut, "/v1/services/web", "localhost:7070", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without %s: %d", csrfHeader, rec.Code)
	}
	if rec := do(h, http.MethodPut, "/v1/services/web", "localhost:7070", map[string]string{csrfHeader: "x"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT with header: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/v1/services", "127.0.0.1:7070", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET needs no header: %d", rec.Code)
	}
}

func TestLoopbackModeRejectsForeignHost(t *testing.T) {
	// DNS rebinding: evil.example resolves to 127.0.0.1, but the browser
	// still sends Host: evil.example.
	h := Handler(Config{API: okAPI, LoopbackOnly: true})
	if rec := do(h, http.MethodGet, "/v1/services", "evil.example:7070", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign Host: %d", rec.Code)
	}
	for _, host := range []string{"localhost:7070", "127.0.0.1:7070", "[::1]:7070"} {
		if rec := do(h, http.MethodGet, "/v1/services", host, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", host, rec.Code)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7070": true, "localhost:7070": true, "[::1]:7070": true,
		"0.0.0.0:7070": false, ":7070": false, "192.168.64.2:7070": false,
	} {
		if got := IsLoopbackAddr(addr); got != want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestBuildRequestValidation(t *testing.T) {
	s := NewBuilds(nil, nil, t.TempDir())
	good := engine.ServiceSpec{Name: "app", Port: 8080}
	cases := map[string]BuildRequest{
		"no source":        {Spec: good},
		"relative path":    {Source: "app", Spec: good},
		"missing dir":      {Source: "/definitely/not/here", Spec: good},
		"escape via dir":   {Source: t.TempDir(), Dir: "../..", Spec: good},
		"bad service name": {Source: t.TempDir(), Spec: engine.ServiceSpec{Name: "Bad_Name"}},
		"option injection": {Source: "https://example.com/r.git", Ref: "--upload-pack=touch /tmp/x", Spec: good},
	}
	for name, req := range cases {
		if _, err := s.Start(req); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestLogBufferFollow(t *testing.T) {
	l := newLogBuffer()
	l.Write([]byte("one\ntw"))
	l.Write([]byte("o\n"))
	done := make(chan string)
	go func() {
		var b strings.Builder
		l.Stream(t.Context(), &b, true)
		done <- b.String()
	}()
	l.Printf("three")
	l.Close()
	if got := <-done; got != "one\ntwo\n=> three\n" {
		t.Fatalf("stream = %q", got)
	}
}
