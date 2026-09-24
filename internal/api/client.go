// Package api is the HTTP client the CLI uses to talk to `rh daemon`,
// over a unix socket by default (like the Docker CLI and dockerd).
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultSocket is where the daemon listens.
const DefaultSocket = "/run/roundhouse.sock"

// Client calls the engine API.
type Client struct {
	base string
	http *http.Client
}

// FromEnv builds a client from RH_HOST ("unix:///path" or "http://host:port").
func FromEnv() *Client {
	host := os.Getenv("RH_HOST")
	if host == "" {
		host = "unix://" + DefaultSocket
	}
	return New(host)
}

// New returns a client for host.
func New(host string) *Client {
	if path, ok := strings.CutPrefix(host, "unix://"); ok {
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		}
		return &Client{base: "http://roundhouse", http: &http.Client{Transport: tr}}
	}
	return &Client{base: strings.TrimSuffix(host, "/"), http: &http.Client{Transport: &http.Transport{Proxy: nil}}}
}

// Error is a non-2xx API response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Do sends a request and decodes a JSON response into out (if non-nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the roundhouse daemon (is `rh daemon` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream reads an NDJSON endpoint, calling fn per line until EOF, ctx end or
// fn returns an error.
func (c *Client) Stream(ctx context.Context, path string, fn func(json.RawMessage) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("cannot reach the roundhouse daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if err := fn(json.RawMessage(sc.Bytes())); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// Query builds a query string.
func Query(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// Timeout is a convenience context for one-shot calls.
func Timeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
