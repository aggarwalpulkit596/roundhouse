package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecureJoinStaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.Symlink("/etc", filepath.Join(root, "abs"))       // absolute: re-rooted
	os.Symlink("../../../..", filepath.Join(root, "up")) // climbs: clamped at root
	os.Symlink("etc", filepath.Join(root, "rel"))        // relative: followed
	os.Symlink("loop2", filepath.Join(root, "loop1"))    // cycle
	os.Symlink("loop1", filepath.Join(root, "loop2"))

	cases := map[string]string{
		"/etc/passwd":      "etc/passwd",
		"abs/passwd":       "etc/passwd",
		"up/etc/shadow":    "etc/shadow",
		"rel/hosts":        "etc/hosts",
		"../../../../x":    "x",
		"/a/b/../../c":     "c",
		"missing/dir/file": "missing/dir/file",
	}
	for in, want := range cases {
		got, err := SecureJoin(root, in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != filepath.Join(root, want) {
			t.Errorf("SecureJoin(%q) = %s, want %s", in, strings.TrimPrefix(got, root), want)
		}
	}
	if _, err := SecureJoin(root, "loop1/x"); err == nil {
		t.Error("symlink loops must be detected")
	}
}
