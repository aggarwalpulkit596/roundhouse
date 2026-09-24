package builder

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// buildContext is the directory COPY reads from, with ignore rules applied.
type buildContext struct {
	root    string
	ignores []string
}

func newContext(root string) (*buildContext, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	c := &buildContext{root: abs}
	for _, name := range []string{".rhignore", ".dockerignore"} {
		f, err := os.Open(filepath.Join(abs, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			c.ignores = append(c.ignores, strings.Trim(path.Clean(l), "/"))
		}
		f.Close()
		break
	}
	return c, nil
}

// ignored reports whether rel (slash-separated) matches an ignore rule. A
// rule matches the path itself or any parent directory, like .dockerignore.
func (c *buildContext) ignored(rel string) bool {
	for _, pat := range c.ignores {
		neg := strings.HasPrefix(pat, "!")
		if neg {
			continue // negations are not supported; documented limitation
		}
		p := rel
		for {
			if ok, _ := path.Match(pat, p); ok {
				return true
			}
			i := strings.LastIndex(p, "/")
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	return false
}

// source is one file selected by a COPY.
type source struct {
	abs string // host path
	rel string // path relative to the COPY source root, slash-separated
	fi  fs.FileInfo
}

// resolve expands a COPY source (file, dir or glob, relative to the context)
// into files. Paths may not escape the context.
func (c *buildContext) resolve(src string) ([]source, error) {
	clean := path.Clean("/" + src)[1:]
	var matches []string
	if strings.ContainsAny(clean, "*?[") {
		m, err := filepath.Glob(filepath.Join(c.root, clean))
		if err != nil {
			return nil, err
		}
		matches = m
	} else {
		matches = []string{filepath.Join(c.root, clean)}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("COPY %s: no source files were specified", src)
	}
	var out []source
	for _, m := range matches {
		fi, err := os.Lstat(m)
		if err != nil {
			return nil, fmt.Errorf("COPY %s: %w", src, err)
		}
		relTop, _ := filepath.Rel(c.root, m)
		if strings.HasPrefix(relTop, "..") {
			return nil, fmt.Errorf("COPY %s: outside the build context", src)
		}
		if !fi.IsDir() {
			if !c.ignored(filepath.ToSlash(relTop)) {
				out = append(out, source{abs: m, rel: filepath.Base(m), fi: fi})
			}
			continue
		}
		// A directory copies its contents, not itself.
		err = filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rctx, _ := filepath.Rel(c.root, p)
			if p != m && c.ignored(filepath.ToSlash(rctx)) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if p == m {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(m, p)
			out = append(out, source{abs: p, rel: filepath.ToSlash(rel), fi: fi})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// hashSources is the cache input of a COPY: every file's relative path,
// mode and content. Timestamps are deliberately excluded, so a fresh git
// clone (new mtimes, same bytes) still hits the cache — BuildKit does the
// same.
func hashSources(groups [][]source) (string, error) {
	h := sha256.New()
	for _, g := range groups {
		sorted := append([]source(nil), g...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].rel < sorted[j].rel })
		for _, s := range sorted {
			fmt.Fprintf(h, "%s\x00%o\x00", s.rel, s.fi.Mode())
			switch {
			case s.fi.Mode().IsRegular():
				f, err := os.Open(s.abs)
				if err != nil {
					return "", err
				}
				_, err = io.Copy(h, f)
				f.Close()
				if err != nil {
					return "", err
				}
			case s.fi.Mode()&os.ModeSymlink != 0:
				t, _ := os.Readlink(s.abs)
				h.Write([]byte(t))
			}
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyInto copies sources under dst (a directory inside a layer dir).
func copyInto(srcs []source, dst string, uid, gid int, chown bool) error {
	for _, s := range srcs {
		target := filepath.Join(dst, filepath.FromSlash(s.rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch {
		case s.fi.IsDir():
			if err := os.MkdirAll(target, s.fi.Mode().Perm()); err != nil {
				return err
			}
			_ = os.Chmod(target, s.fi.Mode().Perm())
		case s.fi.Mode()&os.ModeSymlink != 0:
			t, err := os.Readlink(s.abs)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(t, target); err != nil {
				return err
			}
		case s.fi.Mode().IsRegular():
			if err := copyFile(s.abs, target, s.fi.Mode().Perm()); err != nil {
				return err
			}
		default:
			continue // sockets, devices: not copied
		}
		if chown {
			if err := os.Lchown(target, uid, gid); err != nil {
				return err
			}
		}
		if s.fi.Mode()&os.ModeSymlink == 0 {
			_ = os.Chtimes(target, s.fi.ModTime(), s.fi.ModTime())
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
