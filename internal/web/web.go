// Package web is Roundhouse's dashboard: a Railway-style canvas of
// services, served by `rh daemon` from files embedded in the binary. It is
// plain HTML, CSS and JavaScript talking to the same JSON API the CLI uses,
// so there is no separate frontend build and nothing extra to deploy.
//
// Security model. Whoever can use this API can run containers as root on
// the host, so:
//
//   - Bound to loopback (the default), the dashboard needs no login, but
//     requests must carry a loopback Host header (defeats DNS rebinding).
//   - Bound to any other address, a token is required. The daemon prints a
//     one-time login URL; the browser keeps the token in an HttpOnly cookie.
//   - Every state-changing request must carry the X-Requested-By header,
//     which a cross-site form cannot set (CSRF protection).
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"io/fs"
	"net"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// Config wires the dashboard.
type Config struct {
	// API serves /v1 and /metrics (the engine's handler).
	API http.Handler
	// Builds adds the build endpoints; may be nil.
	Builds *Builds
	// Token, when non-empty, is required for API access.
	Token string
	// LoopbackOnly enforces a loopback Host header.
	LoopbackOnly bool
}

const (
	cookieName = "rh_token"
	csrfHeader = "X-Requested-By"
)

// Handler returns the dashboard plus API.
func Handler(c Config) http.Handler {
	api := http.NewServeMux()
	if c.Builds != nil {
		c.Builds.Routes(api)
	}
	api.Handle("/", c.API)

	files, _ := fs.Sub(static, "static")
	fileServer := http.FileServer(http.FS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.LoopbackOnly && !loopbackHost(r.Host) {
			http.Error(w, "the dashboard is bound to loopback; open it as http://localhost:<port>", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// /login?token=… sets the cookie and redirects to the dashboard.
		if r.URL.Path == "/login" {
			if c.Token != "" && tokenOK(r.URL.Query().Get("token"), c.Token) {
				http.SetCookie(w, &http.Cookie{Name: cookieName, Value: c.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/?login=failed", http.StatusSeeOther)
			return
		}
		if r.URL.Path == "/v1/session" {
			writeJSON(w, http.StatusOK, map[string]bool{"authenticated": authorized(r, c.Token), "tokenRequired": c.Token != ""})
			return
		}
		isAPI := strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/metrics" || r.URL.Path == "/healthz"
		if !isAPI {
			// Static assets hold no data and are always served.
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
			fileServer.ServeHTTP(w, r)
			return
		}
		if !authorized(r, c.Token) {
			writeErr(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
			writeErr(w, http.StatusForbidden, errCSRF)
			return
		}
		api.ServeHTTP(w, r)
	})
}

// APIHandler is the API with builds but without the dashboard or auth, for
// the unix socket (whose file permissions are the access control).
func APIHandler(engineAPI http.Handler, builds *Builds) http.Handler {
	mux := http.NewServeMux()
	if builds != nil {
		builds.Routes(mux)
	}
	mux.Handle("/", engineAPI)
	return mux
}

type webError string

func (e webError) Error() string { return string(e) }

const (
	errUnauthorized = webError("login required: open the login URL printed by `rh daemon`, or run `sudo rh dashboard` for a fresh one")
	errCSRF         = webError("missing " + csrfHeader + " header")
)

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	if c, err := r.Cookie(cookieName); err == nil && tokenOK(c.Value, token) {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && tokenOK(strings.TrimPrefix(h, "Bearer "), token) {
		return true
	}
	return false
}

func tokenOK(got, want string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsLoopbackAddr reports whether a listen address only accepts local
// connections.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return loopbackHost(host)
}

// NewToken returns a random 32-hex-character token.
func NewToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
