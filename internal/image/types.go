// Package image implements the OCI image side of a container engine: parsing
// references, talking to registries (distribution spec), a content-addressed
// blob store, and unpacking layers into overlayfs-ready directories.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

// Media types. Docker's v2 schema 2 types predate OCI and are still what
// Docker Hub serves for many images, so a client must accept both.
const (
	MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIConfig      = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayer       = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeOCILayerGzip   = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCILayerZstd   = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerConfig   = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerLayer    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// Descriptor points at a blob by digest (OCI image-spec descriptor.md).
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      Digest            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform selects an entry in a multi-arch index.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// Manifest lists one image's config and layers.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Index is a multi-platform manifest list.
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

// Config is the image configuration blob. Its digest is the image ID.
type Config struct {
	Created      *time.Time      `json:"created,omitempty"`
	Architecture string          `json:"architecture"`
	OS           string          `json:"os"`
	Config       ContainerConfig `json:"config"`
	RootFS       RootFS          `json:"rootfs"`
	History      []History       `json:"history,omitempty"`
}

// ContainerConfig holds the defaults a container inherits from its image.
type ContainerConfig struct {
	User         string              `json:"User,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
}

// RootFS lists the uncompressed layer digests ("diff IDs") in order.
type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []Digest `json:"diff_ids"`
}

// History records how each layer was produced.
type History struct {
	Created    *time.Time `json:"created,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	EmptyLayer bool       `json:"empty_layer,omitempty"`
	Comment    string     `json:"comment,omitempty"`
}

// Digest is "sha256:<64 hex>". Validating it is a security boundary: digests
// become file paths in the blob store and URLs in the registry.
type Digest string

var digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Validate rejects anything that is not a well-formed sha256 digest.
func (d Digest) Validate() error {
	if !digestRE.MatchString(string(d)) {
		return fmt.Errorf("invalid digest %q", string(d))
	}
	return nil
}

// Hex is the digest without the algorithm prefix.
func (d Digest) Hex() string {
	if len(d) > 7 {
		return string(d[7:])
	}
	return ""
}

// Short is the first 12 hex characters, as Docker prints image IDs.
func (d Digest) Short() string {
	h := d.Hex()
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// FromBytes digests b.
func FromBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// FromHash formats a finished sha256 hash.
func FromHash(sum []byte) Digest {
	return Digest("sha256:" + hex.EncodeToString(sum))
}
