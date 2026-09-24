// Package registry is a small OCI distribution-spec registry: enough of the
// API for `rh push`, `rh pull`, `docker push/pull` and `crane` to work.
//
// Storage is content-addressed and shared across repositories, so a base
// layer pushed by a hundred apps is stored once:
//
//	<root>/blobs/sha256/<hex>
//	<root>/repositories/<name>/_manifests/tags/<tag>   -> manifest digest
//	<root>/uploads/<uuid>                              in-progress uploads
//
// Real registries (distribution/distribution, Zot, Harbor) also keep
// per-repository blob links so that knowing a digest is not enough to read
// a blob from a repository you cannot access. This one serves any blob from
// any repository; do not expose it beyond a trusted network.
package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aggarwalpulkit596/roundhouse/internal/image"
)

// Server implements http.Handler.
type Server struct {
	Root string
	Logf func(string, ...any)
}

// New prepares the storage layout.
func New(root string) (*Server, error) {
	for _, d := range []string{"blobs/sha256", "repositories", "uploads"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Server{Root: root, Logf: log.Printf}, nil
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	tagRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	uuidRE = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// errorBody is the spec's error envelope.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

// ServeHTTP routes /v2/<name>/{blobs,manifests,tags}/... . Names contain
// slashes, so the route is found by its last keyword.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := r.URL.Path
	if p == "/v2/" || p == "/v2" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
		return
	}
	if !strings.HasPrefix(p, "/v2/") {
		http.NotFound(w, r)
		return
	}
	rest := p[len("/v2/"):]
	for _, kw := range []string{"/blobs/uploads/", "/blobs/uploads", "/blobs/", "/manifests/", "/tags/list"} {
		i := strings.LastIndex(rest, kw)
		if i < 0 {
			continue
		}
		name, arg := rest[:i], rest[i+len(kw):]
		if !nameRE.MatchString(name) {
			writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")
			return
		}
		s.logf("%s %s", r.Method, p)
		switch kw {
		case "/blobs/uploads/", "/blobs/uploads":
			s.uploads(w, r, name, arg)
		case "/blobs/":
			s.blob(w, r, name, arg)
		case "/manifests/":
			s.manifest(w, r, name, arg)
		case "/tags/list":
			s.tags(w, r, name)
		}
		return
	}
	http.NotFound(w, r)
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

func (s *Server) blobPath(d image.Digest) string {
	return filepath.Join(s.Root, "blobs", "sha256", d.Hex())
}

// ---------------------------------------------------------------------------
// blobs

func (s *Server) blob(w http.ResponseWriter, r *http.Request, name, ref string) {
	d := image.Digest(ref)
	if d.Validate() != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		f, err := os.Open(s.blobPath(d))
		if err != nil {
			writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", string(d))
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		// Blobs are immutable, so clients and proxies may cache forever.
		w.Header().Set("Cache-Control", "max-age=31536000, immutable")
		if r.Method == http.MethodHead {
			return
		}
		http.ServeContent(w, r, "", fi.ModTime(), f) // supports Range for resumable pulls
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// uploads implements the blob upload state machine:
//
//	POST   /blobs/uploads/                 → 202, Location: /blobs/uploads/<id>
//	POST   /blobs/uploads/?digest=D        → monolithic upload, 201
//	POST   /blobs/uploads/?mount=D&from=R  → cross-repo mount, 201 if we have D
//	PATCH  /blobs/uploads/<id>             → append a chunk, 202 + Range
//	PUT    /blobs/uploads/<id>?digest=D    → final chunk + verify, 201
func (s *Server) uploads(w http.ResponseWriter, r *http.Request, name, id string) {
	id = strings.Trim(id, "/")
	switch {
	case r.Method == http.MethodPost && id == "":
		q := r.URL.Query()
		if m := image.Digest(q.Get("mount")); m != "" {
			if m.Validate() == nil {
				if _, err := os.Stat(s.blobPath(m)); err == nil {
					w.Header().Set("Location", "/v2/"+name+"/blobs/"+string(m))
					w.Header().Set("Docker-Content-Digest", string(m))
					w.WriteHeader(http.StatusCreated)
					return
				}
			}
			// Fall through to a normal upload, as the spec requires.
		}
		uid := newUUID()
		f, err := os.Create(s.uploadPath(uid))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
			return
		}
		f.Close()
		if d := image.Digest(q.Get("digest")); d != "" {
			s.finish(w, r, name, uid, d)
			return
		}
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+uid)
		w.Header().Set("Range", "0-0")
		w.Header().Set("Docker-Upload-UUID", uid)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPatch && uuidRE.MatchString(id):
		f, err := os.OpenFile(s.uploadPath(id), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
			return
		}
		_, err = io.Copy(f, r.Body)
		fi, _ := f.Stat()
		f.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
		w.Header().Set("Range", fmt.Sprintf("0-%d", max64(fi.Size()-1, 0)))
		w.Header().Set("Docker-Upload-UUID", id)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && uuidRE.MatchString(id):
		s.finish(w, r, name, id, image.Digest(r.URL.Query().Get("digest")))
	case r.Method == http.MethodDelete && uuidRE.MatchString(id):
		_ = os.Remove(s.uploadPath(id))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) uploadPath(id string) string { return filepath.Join(s.Root, "uploads", id) }

// finish appends the request body and moves the upload into the blob store
// if, and only if, it hashes to the digest the client claimed.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, name, id string, d image.Digest) {
	if d.Validate() != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest parameter required")
		return
	}
	path := s.uploadPath(id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
		return
	}
	_, err = io.Copy(f, r.Body)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
		return
	}
	got, err := hashFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	if got != d {
		_ = os.Remove(path)
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", fmt.Sprintf("content hashes to %s, not %s", got, d))
		return
	}
	if err := os.Rename(path, s.blobPath(d)); err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	w.Header().Set("Location", "/v2/"+name+"/blobs/"+string(d))
	w.Header().Set("Docker-Content-Digest", string(d))
	w.WriteHeader(http.StatusCreated)
}

// ---------------------------------------------------------------------------
// manifests

func (s *Server) tagPath(name, tag string) string {
	return filepath.Join(s.Root, "repositories", filepath.FromSlash(name), "_manifests", "tags", tag)
}

func (s *Server) resolve(name, ref string) (image.Digest, bool) {
	if strings.HasPrefix(ref, "sha256:") {
		d := image.Digest(ref)
		return d, d.Validate() == nil
	}
	if !tagRE.MatchString(ref) {
		return "", false
	}
	b, err := os.ReadFile(s.tagPath(name, ref))
	if err != nil {
		return "", false
	}
	d := image.Digest(strings.TrimSpace(string(b)))
	return d, d.Validate() == nil
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		d, ok := s.resolve(name, ref)
		if !ok {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		b, err := os.ReadFile(s.blobPath(d))
		if err != nil {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(b, &probe)
		if probe.MediaType == "" {
			probe.MediaType = image.MediaTypeOCIManifest
		}
		w.Header().Set("Content-Type", probe.MediaType)
		w.Header().Set("Docker-Content-Digest", string(d))
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodPut:
		b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		var m image.Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		// Every blob a manifest references must already be here; otherwise
		// a pull would find a tag that points at nothing.
		refs := append([]image.Descriptor{m.Config}, m.Layers...)
		if m.Config.Digest == "" {
			refs = m.Layers // an index has no config
		}
		for _, d := range refs {
			if d.Digest.Validate() != nil {
				writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "invalid digest in manifest")
				return
			}
			if _, err := os.Stat(s.blobPath(d.Digest)); err != nil {
				writeError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "blob "+string(d.Digest)+" unknown")
				return
			}
		}
		d := image.FromBytes(b)
		if strings.HasPrefix(ref, "sha256:") && image.Digest(ref) != d {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "manifest does not match digest")
			return
		}
		if err := image.WriteFileAtomic(s.blobPath(d), b, 0o644); err != nil {
			writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
			return
		}
		if !strings.HasPrefix(ref, "sha256:") {
			if !tagRE.MatchString(ref) {
				writeError(w, http.StatusBadRequest, "TAG_INVALID", "invalid tag")
				return
			}
			tp := s.tagPath(name, ref)
			if err := os.MkdirAll(filepath.Dir(tp), 0o755); err != nil {
				writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
				return
			}
			if err := image.WriteFileAtomic(tp, []byte(d), 0o644); err != nil {
				writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
				return
			}
		}
		w.Header().Set("Location", "/v2/"+name+"/manifests/"+string(d))
		w.Header().Set("Docker-Content-Digest", string(d))
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request, name string) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "repositories", filepath.FromSlash(name), "_manifests", "tags"))
	if err != nil {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
		return
	}
	tags := []string{}
	for _, e := range ents {
		tags = append(tags, e.Name())
	}
	sort.Strings(tags)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": tags})
}

func hashFile(p string) (image.Digest, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return image.FromHash(h.Sum(nil)), nil
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("no entropy"))
	}
	return hex.EncodeToString(b)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
