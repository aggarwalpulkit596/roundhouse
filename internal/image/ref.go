package image

import (
	"fmt"
	"os"
	"strings"
)

// Reference is a parsed image name such as
// "ghcr.io/org/app:v1" or "alpine@sha256:...".
type Reference struct {
	Domain     string // "docker.io", "ghcr.io", "localhost:5000"
	Repository string // "library/alpine"
	Tag        string // "latest" when neither tag nor digest is given
	Digest     Digest // pinned digest, if any
}

// DefaultDomain is used when a name has no registry host.
const DefaultDomain = "docker.io"

// ParseReference applies the same normalization rules as the Docker CLI:
// "alpine" means "docker.io/library/alpine:latest".
func ParseReference(s string) (Reference, error) {
	var r Reference
	if s == "" {
		return r, fmt.Errorf("empty image reference")
	}
	name := s
	if i := strings.Index(name, "@"); i >= 0 {
		r.Digest = Digest(name[i+1:])
		if err := r.Digest.Validate(); err != nil {
			return r, err
		}
		name = name[:i]
	}
	// A tag is a ":" after the last "/" (a ":" before it is a port).
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		r.Tag = name[i+1:]
		name = name[:i]
	}
	first, rest, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Domain, r.Repository = first, rest
	} else {
		r.Domain, r.Repository = DefaultDomain, name
	}
	if r.Domain == DefaultDomain && !strings.Contains(r.Repository, "/") {
		r.Repository = "library/" + r.Repository
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	if r.Repository == "" || strings.ToLower(r.Repository) != r.Repository {
		return r, fmt.Errorf("invalid repository name %q (must be lowercase)", s)
	}
	for _, part := range strings.Split(r.Repository, "/") {
		if part == "" || part == "." || part == ".." {
			return r, fmt.Errorf("invalid repository name %q", s)
		}
	}
	return r, nil
}

// String is the fully qualified name.
func (r Reference) String() string {
	s := r.Domain + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + string(r.Digest)
	}
	return s
}

// Familiar is the short form Docker prints ("alpine:latest").
func (r Reference) Familiar() string {
	s := r.String()
	s = strings.TrimPrefix(s, DefaultDomain+"/library/")
	return strings.TrimPrefix(s, DefaultDomain+"/")
}

// Identifier is the tag or digest used in /v2/<name>/manifests/<id>.
func (r Reference) Identifier() string {
	if r.Digest != "" {
		return string(r.Digest)
	}
	return r.Tag
}

// Host is the registry API host. Docker Hub's API lives on a different host
// from its image-name domain.
func (r Reference) Host() string {
	if r.Domain == DefaultDomain {
		return "registry-1.docker.io"
	}
	return r.Domain
}

// Scheme is http for local registries and anything listed in
// RH_INSECURE_REGISTRIES (comma separated), https otherwise.
func (r Reference) Scheme() string {
	host := r.Domain
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		return "http"
	}
	for _, h := range strings.Split(os.Getenv("RH_INSECURE_REGISTRIES"), ",") {
		if h != "" && h == host {
			return "http"
		}
	}
	return "https"
}
