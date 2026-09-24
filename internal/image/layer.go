package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// LayerOptions control how a directory becomes a layer tarball.
type LayerOptions struct {
	// Epoch, when non-nil, clamps every mtime to at most this time
	// (SOURCE_DATE_EPOCH). Together with sorted entries and fixed headers it
	// makes layer digests reproducible: the same inputs always produce the
	// same bytes, so caches and registries deduplicate them.
	Epoch *time.Time
}

// WriteLayer tars dir (an overlayfs-format layer: whiteouts are 0/0 char
// devices, opaque dirs carry trusted.overlay.opaque) into w as an OCI layer
// and returns the digest of the uncompressed tar (diff ID).
func WriteLayer(dir string, w io.Writer, opt LayerOptions) (Digest, error) {
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(w, h))
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	inodes := map[uint64]string{} // hardlink detection
	for _, p := range paths {
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		st := fi.Sys().(*syscall.Stat_t)

		// Overlay whiteout → OCI ".wh.<name>".
		if fi.Mode()&os.ModeCharDevice != 0 && st.Rdev == 0 {
			d, b := filepath.Split(rel)
			hdr := &tar.Header{Name: d + whiteoutPrefix + b, Typeflag: tar.TypeReg, Mode: 0o600, Format: tar.FormatPAX}
			clampTimes(hdr, time.Unix(0, 0), opt)
			if err := tw.WriteHeader(hdr); err != nil {
				return "", err
			}
			continue
		}

		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return "", err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return "", err
		}
		hdr.Name = rel
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
		hdr.Uname, hdr.Gname = "", "" // names are host-specific noise
		hdr.Format = tar.FormatPAX
		if fi.Mode().IsRegular() && st.Nlink > 1 {
			if first, ok := inodes[st.Ino]; ok {
				hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
			} else {
				inodes[st.Ino] = rel
			}
		}
		if fi.Mode()&os.ModeCharDevice != 0 || fi.Mode()&os.ModeDevice != 0 {
			hdr.Devmajor, hdr.Devminor = int64(unix.Major(st.Rdev)), int64(unix.Minor(st.Rdev))
		}
		for _, x := range listXattrs(p) {
			if strings.HasPrefix(x, "trusted.overlay.") {
				continue
			}
			if v, err := getXattr(p, x); err == nil {
				if hdr.PAXRecords == nil {
					hdr.PAXRecords = map[string]string{}
				}
				hdr.PAXRecords["SCHILY.xattr."+x] = string(v)
			}
		}
		clampTimes(hdr, fi.ModTime(), opt)
		if err := tw.WriteHeader(hdr); err != nil {
			return "", err
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			f, err := os.Open(p)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return "", err
			}
		}
		// Opaque directory → ".wh..wh..opq" inside it.
		if fi.IsDir() {
			if v, err := getXattr(p, opaqueXattr); err == nil && string(v) == "y" {
				oh := &tar.Header{Name: rel + "/" + whiteoutOpaque, Typeflag: tar.TypeReg, Mode: 0o600, Format: tar.FormatPAX}
				clampTimes(oh, time.Unix(0, 0), opt)
				if err := tw.WriteHeader(oh); err != nil {
					return "", err
				}
			}
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return FromHash(h.Sum(nil)), nil
}

func clampTimes(h *tar.Header, mtime time.Time, opt LayerOptions) {
	if opt.Epoch != nil && mtime.After(*opt.Epoch) {
		mtime = *opt.Epoch
	}
	h.ModTime = mtime.Truncate(time.Second)
	h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
}

// CommitLayer turns a finished overlay-format directory into a stored layer:
// the gzipped tar goes to the blob store and the directory itself is moved
// into place as the unpacked layer, so it never needs re-extracting.
func (s *Store) CommitLayer(dir string, opt LayerOptions) (diffID Digest, blob Descriptor, err error) {
	tmp, err := os.CreateTemp(filepath.Join(s.Root, "tmp"), "layer-*.tar.gz")
	if err != nil {
		return "", Descriptor{}, err
	}
	defer os.Remove(tmp.Name())
	gh := sha256.New()
	cw := &countWriter{w: io.MultiWriter(tmp, gh)}
	// A zero gzip header (no name, no mtime) keeps the blob reproducible.
	gz, _ := gzip.NewWriterLevel(cw, gzip.DefaultCompression)
	diffID, err = WriteLayer(dir, gz, opt)
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", Descriptor{}, err
	}
	blobDigest := FromHash(gh.Sum(nil))
	if err := os.Rename(tmp.Name(), s.BlobPath(blobDigest)); err != nil {
		return "", Descriptor{}, err
	}
	blob = Descriptor{MediaType: MediaTypeOCILayerGzip, Digest: blobDigest, Size: cw.n}

	if !s.HasLayer(diffID) {
		ldir := filepath.Join(s.Root, "layers", diffID.Hex())
		_ = os.RemoveAll(ldir)
		if err := os.MkdirAll(ldir, 0o755); err != nil {
			return "", Descriptor{}, err
		}
		if err := os.Rename(dir, filepath.Join(ldir, "fs")); err != nil {
			// Different filesystem: fall back to unpacking the blob.
			_ = os.RemoveAll(ldir)
			if err := s.UnpackLayer(blobDigest, MediaTypeOCILayerGzip, diffID); err != nil {
				return "", Descriptor{}, err
			}
			return diffID, blob, nil
		}
		if err := os.WriteFile(filepath.Join(ldir, "done"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
			return "", Descriptor{}, err
		}
	} else {
		_ = os.RemoveAll(dir)
	}
	return diffID, blob, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func listXattrs(p string) []string {
	sz, err := unix.Llistxattr(p, nil)
	if err != nil || sz <= 0 {
		return nil
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(p, buf)
	if err != nil {
		return nil
	}
	var out []string
	for _, x := range strings.Split(string(buf[:sz]), "\x00") {
		if x != "" {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func getXattr(p, name string) ([]byte, error) {
	sz, err := unix.Lgetxattr(p, name, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sz)
	sz, err = unix.Lgetxattr(p, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:sz], nil
}

// SaveImage writes config + manifest blobs for a built image and tags it.
func (s *Store) SaveImage(name string, cfg Config, layers []Descriptor) (Image, error) {
	ref, err := ParseReference(name)
	if err != nil {
		return Image{}, err
	}
	cb, err := json.Marshal(cfg)
	if err != nil {
		return Image{}, err
	}
	cd, csize, err := s.PutBlob(strings.NewReader(string(cb)), "")
	if err != nil {
		return Image{}, err
	}
	man := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        Descriptor{MediaType: MediaTypeOCIConfig, Digest: cd, Size: csize},
		Layers:        layers,
	}
	if man.Layers == nil {
		man.Layers = []Descriptor{}
	}
	mb, err := json.Marshal(man)
	if err != nil {
		return Image{}, err
	}
	md, _, err := s.PutBlob(strings.NewReader(string(mb)), "")
	if err != nil {
		return Image{}, err
	}
	var total int64
	for _, l := range layers {
		total += l.Size
	}
	im := Image{Name: ref.String(), Manifest: md, Config: cd, Layers: cfg.RootFS.DiffIDs, Size: total, Created: time.Now().UTC()}
	return im, s.Tag(im)
}

// Manifest reads an image's manifest.
func (s *Store) Manifest(im Image) (Manifest, []byte, error) {
	var m Manifest
	b, err := s.ReadBlob(im.Manifest)
	if err != nil {
		return m, nil, err
	}
	return m, b, json.Unmarshal(b, &m)
}

// TagAs gives an existing image another name.
func (s *Store) TagAs(src, dst string) (Image, error) {
	im, err := s.Get(src)
	if err != nil {
		return Image{}, err
	}
	ref, err := ParseReference(dst)
	if err != nil {
		return Image{}, err
	}
	im.Name = ref.String()
	return im, s.Tag(im)
}

// Push uploads an image. Blobs the registry already has are skipped after a
// HEAD request: this is what makes pushing a one-line change to a 1 GB image
// cost one small layer.
func (s *Store) Push(ctx context.Context, src, dst string, progress Progress) error {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	im, err := s.Get(src)
	if err != nil {
		return err
	}
	ref, err := ParseReference(dst)
	if err != nil {
		return err
	}
	man, mb, err := s.Manifest(im)
	if err != nil {
		return fmt.Errorf("image %s has no stored manifest: %w", src, err)
	}
	blobs := append([]Descriptor{man.Config}, man.Layers...)
	for _, d := range blobs {
		ok, err := s.Client.BlobExists(ctx, ref, d.Digest)
		if err != nil {
			return err
		}
		if ok {
			progress("%s: %s already exists", ref.Familiar(), d.Digest.Short())
			continue
		}
		start := time.Now()
		path := s.BlobPath(d.Digest)
		open := func() (io.ReadCloser, error) { return os.Open(path) }
		if err := s.Client.PutBlob(ctx, ref, d.Digest, d.Size, open); err != nil {
			return fmt.Errorf("push %s: %w", d.Digest.Short(), err)
		}
		progress("%s: pushed %s (%s) in %s", ref.Familiar(), d.Digest.Short(), humanBytes(d.Size), time.Since(start).Round(time.Millisecond))
	}
	mt := man.MediaType
	if mt == "" {
		mt = MediaTypeOCIManifest
	}
	if err := s.Client.PutManifest(ctx, ref, ref.Identifier(), mt, mb); err != nil {
		return err
	}
	progress("%s: pushed manifest %s", ref.Familiar(), FromBytes(mb).Short())
	return nil
}
