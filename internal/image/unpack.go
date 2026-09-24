package image

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aggarwalpulkit596/roundhouse/internal/fsutil"
)

// OCI whiteouts (image-spec layer.md). A layer deletes a lower file by
// shipping an empty ".wh.<name>" entry, and hides a whole lower directory's
// contents with ".wh..wh..opq". Overlayfs expresses the same things
// differently, so the unpacker translates:
//
//	.wh.<name>      →  character device 0/0 named <name>
//	.wh..wh..opq    →  xattr trusted.overlay.opaque="y" on the directory
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
	opaqueXattr    = "trusted.overlay.opaque"
)

// UnpackLayer decompresses a stored blob into its layer directory. The
// directory is built under a temp name and renamed when complete, so a
// half-unpacked layer is never visible.
func (s *Store) UnpackLayer(blob Digest, mediaType string, diffID Digest) error {
	if s.HasLayer(diffID) {
		return nil
	}
	f, err := os.Open(s.BlobPath(blob))
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	switch mediaType {
	case MediaTypeOCILayerGzip, MediaTypeDockerLayer, "":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	case MediaTypeOCILayer:
	case MediaTypeOCILayerZstd:
		return errors.New("zstd layers are not supported (exercise: add github.com/klauspost/compress/zstd)")
	default:
		return fmt.Errorf("unsupported layer media type %q", mediaType)
	}

	tmp, err := os.MkdirTemp(filepath.Join(s.Root, "tmp"), "layer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fsDir := filepath.Join(tmp, "fs")
	if err := os.Mkdir(fsDir, 0o755); err != nil {
		return err
	}
	got, err := UnpackTar(r, fsDir)
	if err != nil {
		return err
	}
	// The diff ID is the hash of the *uncompressed* tar. Checking it proves
	// the layer we unpacked is the one the image config names.
	if diffID != "" && got != diffID {
		return fmt.Errorf("diff ID mismatch: config says %s, layer is %s", diffID.Short(), got.Short())
	}
	if err := os.WriteFile(filepath.Join(tmp, "done"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		return err
	}
	dst := filepath.Join(s.Root, "layers", got.Hex())
	if err := os.Rename(tmp, dst); err != nil {
		if s.HasLayer(got) {
			return nil // a concurrent pull won the race
		}
		// A stale incomplete directory from a crash: replace it.
		_ = os.RemoveAll(dst)
		return os.Rename(tmp, dst)
	}
	return nil
}

// UnpackTar extracts a layer tar into dir in overlayfs format and returns the
// sha256 of the tar stream (the diff ID).
func UnpackTar(r io.Reader, dir string) (Digest, error) {
	privileged := os.Geteuid() == 0
	h := sha256.New()
	tee := io.TeeReader(r, h)
	tr := tar.NewReader(tee)
	type dirTimes struct {
		path  string
		mtime time.Time
	}
	var dirs []dirTimes
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		name := filepath.Clean("/" + hdr.Name) // strips any leading "../"
		if name == "/" {
			continue
		}
		parentRel, base := filepath.Split(name)
		// Resolve the parent with symlinks scoped to dir: a layer that
		// ships "etc -> /host/etc" and then "etc/passwd" must not write
		// outside the layer (the bug class behind CVE-2018-15664).
		parent, err := fsutil.SecureJoin(dir, parentRel)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return "", err
		}
		target := filepath.Join(parent, base)

		switch {
		case base == whiteoutOpaque:
			if err := unix.Setxattr(parent, opaqueXattr, []byte("y"), 0); err != nil {
				return "", fmt.Errorf("opaque whiteout %s: %w", name, err)
			}
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			wt := filepath.Join(parent, strings.TrimPrefix(base, whiteoutPrefix))
			_ = os.RemoveAll(wt)
			if err := unix.Mknod(wt, unix.S_IFCHR, 0); err != nil {
				return "", fmt.Errorf("whiteout %s: %w", name, err)
			}
			continue
		}

		// Tar may contain an entry twice; later entries win.
		if fi, err := os.Lstat(target); err == nil {
			if !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
				if err := os.RemoveAll(target); err != nil {
					return "", err
				}
			}
		}

		mode := os.FileMode(hdr.Mode).Perm() | os.FileMode(hdr.Mode)&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, 0o755); err != nil && !os.IsExist(err) {
				return "", err
			}
			dirs = append(dirs, dirTimes{target, hdr.ModTime})
		case tar.TypeReg, tar.TypeRegA:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return "", err
			}
		case tar.TypeLink:
			src, err := fsutil.SecureJoin(dir, hdr.Linkname)
			if err != nil {
				return "", err
			}
			if err := os.Link(src, target); err != nil {
				return "", fmt.Errorf("hardlink %s -> %s: %w", name, hdr.Linkname, err)
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			var kind uint32 = unix.S_IFIFO
			if hdr.Typeflag == tar.TypeChar {
				kind = unix.S_IFCHR
			} else if hdr.Typeflag == tar.TypeBlock {
				kind = unix.S_IFBLK
			}
			dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
			if err := unix.Mknod(target, kind|uint32(mode.Perm()), int(dev)); err != nil {
				return "", err
			}
		case tar.TypeXGlobalHeader:
			continue
		default:
			return "", fmt.Errorf("%s: unsupported tar type %q", name, hdr.Typeflag)
		}

		// Only root can give files away. An unprivileged unpack (tests,
		// rootless tools) keeps its own uid, as `tar` does for non-root.
		if privileged {
			if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
				return "", err
			}
		}
		for k, v := range hdr.PAXRecords {
			if x, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
				_ = unix.Lsetxattr(target, x, []byte(v), 0)
			}
		}
		if hdr.Typeflag != tar.TypeSymlink && hdr.Typeflag != tar.TypeLink {
			// chmod after chown: chown clears setuid bits.
			if err := os.Chmod(target, mode); err != nil {
				return "", err
			}
		}
		if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeLink {
			_ = lutimes(target, hdr.ModTime)
		}
	}
	// Set directory times last: creating children bumps the parent's mtime.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = lutimes(dirs[i].path, dirs[i].mtime)
	}
	// Drain trailing padding so the hash covers the whole stream.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return "", err
	}
	return FromHash(h.Sum(nil)), nil
}

func lutimes(path string, t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}
