package image

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

// Store is a content-addressed image store, laid out like a simplified
// containerd content store + overlayfs snapshotter:
//
//	<root>/blobs/sha256/<hex>        compressed layers, configs, manifests
//	<root>/layers/<diffid-hex>/fs    one unpacked directory per layer
//	<root>/images.json               name -> manifest digest
//
// Blobs are immutable and named by their hash, so two images sharing a base
// layer store and unpack it exactly once.
type Store struct {
	Root   string
	Client *Client
	mu     sync.Mutex
}

// Image is an entry in images.json.
type Image struct {
	Name     string    `json:"name"`
	Manifest Digest    `json:"manifest"`
	Config   Digest    `json:"config"`
	Layers   []Digest  `json:"layers"` // diff IDs, bottom to top
	Size     int64     `json:"size"`   // compressed bytes
	Created  time.Time `json:"created"`
}

// NewStore opens (creating if needed) a store under root.
func NewStore(root string) (*Store, error) {
	for _, d := range []string{"blobs/sha256", "layers", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root, Client: NewClient()}, nil
}

// BlobPath is where a blob lives on disk.
func (s *Store) BlobPath(d Digest) string {
	return filepath.Join(s.Root, "blobs", "sha256", d.Hex())
}

// HasBlob reports whether a blob is present.
func (s *Store) HasBlob(d Digest) bool {
	_, err := os.Stat(s.BlobPath(d))
	return err == nil
}

// PutBlob streams r into the store, verifying it hashes to want (if set).
// The write goes to a temp file and is renamed into place, so a crash never
// leaves a truncated blob under a valid name.
func (s *Store) PutBlob(r io.Reader, want Digest) (Digest, int64, error) {
	if want != "" {
		if err := want.Validate(); err != nil {
			return "", 0, err
		}
		if fi, err := os.Stat(s.BlobPath(want)); err == nil {
			return want, fi.Size(), nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Join(s.Root, "tmp"), "blob-")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	got := FromHash(h.Sum(nil))
	if want != "" && got != want {
		return "", 0, fmt.Errorf("digest mismatch: expected %s, got %s", want, got)
	}
	if err := os.Rename(tmp.Name(), s.BlobPath(got)); err != nil {
		return "", 0, err
	}
	return got, n, nil
}

// ReadBlob returns a small blob's contents.
func (s *Store) ReadBlob(d Digest) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return os.ReadFile(s.BlobPath(d))
}

// LayerDir is the unpacked directory for a diff ID.
func (s *Store) LayerDir(diffID Digest) string {
	return filepath.Join(s.Root, "layers", diffID.Hex(), "fs")
}

// HasLayer reports whether a layer has been fully unpacked.
func (s *Store) HasLayer(diffID Digest) bool {
	_, err := os.Stat(filepath.Join(s.Root, "layers", diffID.Hex(), "done"))
	return err == nil
}

// ---------------------------------------------------------------------------
// image index

func (s *Store) indexPath() string { return filepath.Join(s.Root, "images.json") }

func (s *Store) loadIndex() (map[string]Image, error) {
	idx := map[string]Image{}
	b, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	return idx, json.Unmarshal(b, &idx)
}

// updateIndex applies fn under both an in-process mutex and a file lock, so
// the CLI and the daemon can both tag images safely.
func (s *Store) updateIndex(fn func(map[string]Image) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := flock(filepath.Join(s.Root, ".images.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	idx, err := s.loadIndex()
	if err != nil {
		return err
	}
	if err := fn(idx); err != nil {
		return err
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.indexPath(), b, 0o644)
}

// Tag records an image under name.
func (s *Store) Tag(img Image) error {
	return s.updateIndex(func(idx map[string]Image) error {
		idx[img.Name] = img
		return nil
	})
}

// Untag removes a name. Blobs are left for GC.
func (s *Store) Untag(name string) error {
	return s.updateIndex(func(idx map[string]Image) error {
		if _, ok := idx[name]; !ok {
			return fmt.Errorf("no such image %s", name)
		}
		delete(idx, name)
		return nil
	})
}

// List returns images sorted by name.
func (s *Store) List() ([]Image, error) {
	idx, err := s.loadIndex()
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(idx))
	for _, im := range idx {
		out = append(out, im)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get resolves a user-supplied name ("alpine", "docker.io/library/alpine:3",
// or an image ID prefix) to a stored image.
func (s *Store) Get(name string) (Image, error) {
	idx, err := s.loadIndex()
	if err != nil {
		return Image{}, err
	}
	if ref, err := ParseReference(name); err == nil {
		if im, ok := idx[ref.String()]; ok {
			return im, nil
		}
	}
	if im, ok := idx[name]; ok {
		return im, nil
	}
	for _, im := range idx {
		if len(name) >= 6 && strings.HasPrefix(im.Config.Hex(), strings.TrimPrefix(name, "sha256:")) {
			return im, nil
		}
	}
	return Image{}, fmt.Errorf("image %q not found locally (try: rh pull %s)", name, name)
}

// Config reads an image's config blob.
func (s *Store) Config(im Image) (Config, error) {
	var c Config
	b, err := s.ReadBlob(im.Config)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// ---------------------------------------------------------------------------
// pull

// Progress receives human-readable pull events.
type Progress func(format string, args ...any)

// Pull fetches an image and unpacks its layers. Downloads and unpacking run
// concurrently per layer, which the overlay model makes safe: each layer
// unpacks into its own directory, independent of the layers below it.
func (s *Store) Pull(ctx context.Context, name string, progress Progress) (Image, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	ref, err := ParseReference(name)
	if err != nil {
		return Image{}, err
	}
	body, mt, err := s.Client.GetManifest(ctx, ref, ref.Identifier())
	if err != nil {
		return Image{}, err
	}
	if ref.Digest != "" && FromBytes(body) != ref.Digest {
		return Image{}, fmt.Errorf("manifest digest mismatch for %s", ref)
	}
	if mt == MediaTypeOCIIndex || mt == MediaTypeDockerList {
		var idx Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return Image{}, err
		}
		desc, err := SelectPlatform(idx, goruntime.GOOS, goruntime.GOARCH)
		if err != nil {
			return Image{}, fmt.Errorf("%s: %w", ref.Familiar(), err)
		}
		progress("%s: resolved index to %s/%s manifest %s", ref.Familiar(), goruntime.GOOS, goruntime.GOARCH, desc.Digest.Short())
		if body, mt, err = s.Client.GetManifest(ctx, ref, string(desc.Digest)); err != nil {
			return Image{}, err
		}
		if FromBytes(body) != desc.Digest {
			return Image{}, fmt.Errorf("manifest digest mismatch for %s", desc.Digest)
		}
	}
	if mt != MediaTypeOCIManifest && mt != MediaTypeDockerManifest {
		return Image{}, fmt.Errorf("unsupported manifest type %q", mt)
	}
	manDigest, _, err := s.PutBlob(strings.NewReader(string(body)), "")
	if err != nil {
		return Image{}, err
	}
	var man Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return Image{}, err
	}

	if err := s.fetch(ctx, ref, man.Config); err != nil {
		return Image{}, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	cb, err := s.ReadBlob(man.Config.Digest)
	if err != nil {
		return Image{}, err
	}
	if err := json.Unmarshal(cb, &cfg); err != nil {
		return Image{}, err
	}
	if len(cfg.RootFS.DiffIDs) != len(man.Layers) {
		return Image{}, fmt.Errorf("config lists %d diff IDs for %d layers", len(cfg.RootFS.DiffIDs), len(man.Layers))
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	var total int64
	for i, l := range man.Layers {
		i, l := i, l
		total += l.Size
		diffID := cfg.RootFS.DiffIDs[i]
		g.Go(func() error {
			if s.HasLayer(diffID) {
				progress("%s: layer %s already exists", ref.Familiar(), l.Digest.Short())
				return nil
			}
			start := time.Now()
			if err := s.fetch(gctx, ref, l); err != nil {
				return fmt.Errorf("layer %s: %w", l.Digest.Short(), err)
			}
			progress("%s: downloaded %s (%s) in %s", ref.Familiar(), l.Digest.Short(), humanBytes(l.Size), time.Since(start).Round(time.Millisecond))
			if err := s.UnpackLayer(l.Digest, l.MediaType, diffID); err != nil {
				return fmt.Errorf("unpack %s: %w", l.Digest.Short(), err)
			}
			progress("%s: unpacked %s", ref.Familiar(), l.Digest.Short())
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Image{}, err
	}
	im := Image{
		Name:     ref.String(),
		Manifest: manDigest,
		Config:   man.Config.Digest,
		Layers:   cfg.RootFS.DiffIDs,
		Size:     total,
		Created:  time.Now().UTC(),
	}
	if err := s.Tag(im); err != nil {
		return Image{}, err
	}
	progress("%s: pulled image %s", ref.Familiar(), im.Config.Short())
	return im, nil
}

func (s *Store) fetch(ctx context.Context, ref Reference, d Descriptor) error {
	if s.HasBlob(d.Digest) {
		return nil
	}
	rc, _, err := s.Client.GetBlob(ctx, ref, d.Digest)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, _, err = s.PutBlob(rc, d.Digest)
	return err
}

// SelectPlatform picks the manifest for os/arch from an index.
func SelectPlatform(idx Index, os, arch string) (Descriptor, error) {
	var avail []string
	for _, m := range idx.Manifests {
		if m.Platform == nil {
			continue
		}
		if m.Platform.OS == os && m.Platform.Architecture == arch {
			if arch != "arm" || m.Platform.Variant == "" || m.Platform.Variant == "v7" {
				return m, nil
			}
		}
		avail = append(avail, m.Platform.OS+"/"+m.Platform.Architecture)
	}
	return Descriptor{}, fmt.Errorf("no manifest for %s/%s (available: %s)", os, arch, strings.Join(avail, ", "))
}

// ---------------------------------------------------------------------------
// helpers

// WriteFileAtomic writes via temp file + fsync + rename.
func WriteFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func flock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// HumanBytes formats a byte count for display.
func HumanBytes(n int64) string { return humanBytes(n) }
