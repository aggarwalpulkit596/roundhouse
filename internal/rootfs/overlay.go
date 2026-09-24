// Package rootfs assembles a container's root filesystem from image layers
// with overlayfs: the read-only layers are stacked as lowerdirs, and each
// container gets a private writable upperdir. This is what containerd's
// overlayfs snapshotter and Docker's overlay2 driver do.
//
//	merged/  (what the container sees)
//	  ├── upper/   container writes land here (copy-up on first write)
//	  ├── lower1/  top image layer
//	  ├── lower2/
//	  └── lowerN/  base layer
//
// Starting a container therefore costs one mount, not a copy of the image.
package rootfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Layout are the per-container directories under a container's state dir.
type Layout struct {
	Upper  string
	Work   string
	Merged string
}

// NewLayout returns the standard layout under dir.
func NewLayout(dir string) Layout {
	return Layout{
		Upper:  filepath.Join(dir, "upper"),
		Work:   filepath.Join(dir, "work"),
		Merged: filepath.Join(dir, "rootfs"),
	}
}

// Mount stacks layers (bottom first, as listed in the image config) under a
// fresh upper directory and mounts the result at l.Merged.
func Mount(layers []string, l Layout) error {
	for _, d := range []string{l.Upper, l.Work, l.Merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if len(layers) == 0 {
		// An image with no layers ("FROM scratch"): overlay still needs a
		// lowerdir, so use an empty one.
		empty := filepath.Join(filepath.Dir(l.Upper), "empty")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			return err
		}
		layers = []string{empty}
	}
	// overlayfs lists lowerdirs top-most first.
	lower := make([]string, len(layers))
	for i, p := range layers {
		lower[len(layers)-1-i] = p
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lower, ":"), l.Upper, l.Work)
	if len(opts) >= unix.Getpagesize()-1 {
		// The kernel copies mount data into a single page.
		return fmt.Errorf("overlay options exceed a page (%d layers); flatten the image", len(layers))
	}
	if err := unix.Mount("overlay", l.Merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	return nil
}

// Unmount detaches the merged view. It is safe to call when not mounted.
func Unmount(l Layout) error {
	err := unix.Unmount(l.Merged, unix.MNT_DETACH)
	if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// IsMounted reports whether path is a mount point.
func IsMounted(path string) bool {
	var st, parent unix.Stat_t
	if unix.Lstat(path, &st) != nil || unix.Lstat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}
