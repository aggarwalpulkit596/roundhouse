// Package fsutil holds filesystem helpers shared by the runtime, the image
// unpacker and the builder.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SecureJoin joins unsafePath onto root, resolving symlinks as if root were
// "/". The result is always inside root. This is the core idea of
// github.com/cyphar/filepath-securejoin, which runc and containerd use to
// defend against symlink-escape CVEs (e.g. CVE-2018-15664).
func SecureJoin(root, unsafePath string) (string, error) {
	const maxLinks = 255
	var resolved string // path relative to root, always clean
	remaining := filepath.Clean("/" + unsafePath)
	links := 0
	for remaining != "" && remaining != "/" {
		remaining = trimLeadingSlash(remaining)
		var part string
		if i := indexSlash(remaining); i >= 0 {
			part, remaining = remaining[:i], remaining[i:]
		} else {
			part, remaining = remaining, ""
		}
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir("/" + resolved)[1:]
			continue
		}
		next := filepath.Join(resolved, part)
		fi, err := os.Lstat(filepath.Join(root, next))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Nothing to resolve: the rest is created literally.
				return filepath.Join(root, next, filepath.Clean("/"+remaining)), nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		links++
		if links > maxLinks {
			return "", fmt.Errorf("securejoin %s: too many symlinks", unsafePath)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			resolved = ""
		}
		remaining = target + "/" + remaining
	}
	return filepath.Join(root, resolved), nil
}

func trimLeadingSlash(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}

func indexSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
