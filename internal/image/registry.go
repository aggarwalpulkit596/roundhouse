package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Client speaks the OCI distribution spec
// (https://github.com/opencontainers/distribution-spec) — the HTTP API every
// registry (Docker Hub, GHCR, ECR, Harbor, our own `rh registry`) implements.
type Client struct {
	HTTP *http.Client
	// UserAgent is sent on every request.
	UserAgent string

	mu     sync.Mutex
	tokens map[string]string // "host|scope" -> bearer token
}

// NewClient returns a client using the environment's proxy settings.
func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Minute},
		UserAgent: "roundhouse/0.1",
		tokens:    map[string]string{},
	}
}

// ManifestAccept lists every manifest type we understand, in preference order.
var ManifestAccept = strings.Join([]string{
	MediaTypeOCIIndex, MediaTypeDockerList, MediaTypeOCIManifest, MediaTypeDockerManifest,
}, ", ")

func (c *Client) base(r Reference) string {
	return fmt.Sprintf("%s://%s/v2/%s", r.Scheme(), r.Host(), r.Repository)
}

// do sends req, handling the registry token dance:
//
//  1. request without credentials → 401 + WWW-Authenticate: Bearer realm=...,
//     service=..., scope=repository:library/alpine:pull
//  2. GET realm?service=...&scope=... → {"token": "..."}
//  3. retry with Authorization: Bearer <token>
//
// Tokens are cached per host and scope.
func (c *Client) do(ctx context.Context, r Reference, req *http.Request, scope string) (*http.Response, error) {
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", c.UserAgent)
	key := r.Host() + "|" + scope
	c.mu.Lock()
	tok := c.tokens[key]
	c.mu.Unlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// Bodies must be replayable for the retry.
	var body []byte
	if req.Body != nil && req.GetBody == nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	tok, err = c.fetchToken(ctx, challenge, scope)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tokens[key] = tok
	c.mu.Unlock()

	retry := req.Clone(ctx)
	if body != nil {
		retry.Body = io.NopCloser(bytes.NewReader(body))
	} else if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, err
		}
	}
	retry.Header.Set("Authorization", "Bearer "+tok)
	return c.HTTP.Do(retry)
}

func (c *Client) fetchToken(ctx context.Context, challenge, scope string) (string, error) {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		return "", fmt.Errorf("registry requires unsupported auth %q", challenge)
	}
	u, err := url.Parse(params["realm"])
	if err != nil {
		return "", err
	}
	q := u.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	if s := params["scope"]; s != "" {
		scope = s
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if user := os.Getenv("RH_REGISTRY_USER"); user != "" {
		req.SetBasicAuth(user, os.Getenv("RH_REGISTRY_PASSWORD"))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token endpoint: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, nil
}

// parseChallenge parses `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[strings.TrimSpace(k)] = v[1:]
				break
			}
			params[strings.TrimSpace(k)] = v[1 : end+1]
			rest = v[end+2:]
		} else {
			val, after, _ := strings.Cut(v, ",")
			params[strings.TrimSpace(k)] = val
			rest = after
		}
	}
	return scheme, params
}

func pullScope(r Reference) string { return "repository:" + r.Repository + ":pull" }
func pushScope(r Reference) string { return "repository:" + r.Repository + ":pull,push" }

// GetManifest fetches a manifest or index by tag or digest.
func (c *Client) GetManifest(ctx context.Context, r Reference, id string) ([]byte, string, error) {
	req, _ := http.NewRequest(http.MethodGet, c.base(r)+"/manifests/"+id, nil)
	req.Header.Set("Accept", ManifestAccept)
	resp, err := c.do(ctx, r, req, pullScope(r))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", httpError("get manifest "+r.Repository+":"+id, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	mt := resp.Header.Get("Content-Type")
	if mt == "" || mt == "application/json" {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(b, &probe)
		mt = probe.MediaType
	}
	return b, mt, nil
}

// GetBlob streams a blob. The caller must verify the digest.
func (c *Client) GetBlob(ctx context.Context, r Reference, d Digest) (io.ReadCloser, int64, error) {
	if err := d.Validate(); err != nil {
		return nil, 0, err
	}
	req, _ := http.NewRequest(http.MethodGet, c.base(r)+"/blobs/"+string(d), nil)
	resp, err := c.do(ctx, r, req, pullScope(r))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, httpError("get blob "+d.Short(), resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// BlobExists is the HEAD request that makes pushes incremental: layers the
// registry already has are never re-uploaded.
func (c *Client) BlobExists(ctx context.Context, r Reference, d Digest) (bool, error) {
	req, _ := http.NewRequest(http.MethodHead, c.base(r)+"/blobs/"+string(d), nil)
	resp, err := c.do(ctx, r, req, pushScope(r))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, httpError("head blob", resp)
}

// PutBlob uploads a blob with the two-step monolithic flow:
// POST /blobs/uploads/ → 202 Location, then PUT <location>?digest=<d>.
func (c *Client) PutBlob(ctx context.Context, r Reference, d Digest, size int64, open func() (io.ReadCloser, error)) error {
	req, _ := http.NewRequest(http.MethodPost, c.base(r)+"/blobs/uploads/", nil)
	resp, err := c.do(ctx, r, req, pushScope(r))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		defer resp.Body.Close()
		return httpError("start upload", resp)
	}
	resp.Body.Close()
	loc, err := resolveLocation(c.base(r), resp.Header.Get("Location"))
	if err != nil {
		return err
	}
	q := loc.Query()
	q.Set("digest", string(d))
	loc.RawQuery = q.Encode()

	body, err := open()
	if err != nil {
		return err
	}
	defer body.Close()
	put, _ := http.NewRequest(http.MethodPut, loc.String(), body)
	put.ContentLength = size
	put.Header.Set("Content-Type", "application/octet-stream")
	put.GetBody = open
	resp, err = c.do(ctx, r, put, pushScope(r))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return httpError("finish upload", resp)
	}
	return nil
}

// PutManifest uploads a manifest under a tag or digest.
func (c *Client) PutManifest(ctx context.Context, r Reference, id, mediaType string, b []byte) error {
	req, _ := http.NewRequest(http.MethodPut, c.base(r)+"/manifests/"+id, bytes.NewReader(b))
	req.Header.Set("Content-Type", mediaType)
	resp, err := c.do(ctx, r, req, pushScope(r))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return httpError("put manifest", resp)
	}
	return nil
}

func resolveLocation(base, loc string) (*url.URL, error) {
	b, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	l, err := url.Parse(loc)
	if err != nil {
		return nil, err
	}
	return b.ResolveReference(l), nil
}

func httpError(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("%s: %s: %s", op, resp.Status, strings.TrimSpace(string(b)))
}
