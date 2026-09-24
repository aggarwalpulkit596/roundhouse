package cgroups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aggarwalpulkit596/roundhouse/internal/spec"
)

// fakeV2 builds a directory tree that looks like a cgroup2 mount. The kernel
// is not involved, so this tests the file protocol, not enforcement (the
// integration tests cover that).
func fakeV2(t *testing.T) string {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory pids io"), 0o644)
	os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), nil, 0o644)
	return root
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestV2WritesLimits(t *testing.T) {
	old := Root
	Root = fakeV2(t)
	defer func() { Root = old }()

	m := New("rh", "c1")
	if m.Version() != 2 {
		t.Fatal("expected the v2 manager")
	}
	if err := m.Create(spec.Resources{MemoryBytes: 256 << 20, CPUMillis: 500, PidsMax: 64}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(Root, "rh", "c1")
	if got := read(t, filepath.Join(dir, "memory.max")); got != "268435456" {
		t.Errorf("memory.max = %s", got)
	}
	if got := read(t, filepath.Join(dir, "cpu.max")); got != "50000 100000" {
		t.Errorf("cpu.max = %s (half a core is 50ms per 100ms period)", got)
	}
	if got := read(t, filepath.Join(dir, "pids.max")); got != "64" {
		t.Errorf("pids.max = %s", got)
	}
	if got := read(t, filepath.Join(dir, "memory.swap.max")); got != "0" {
		t.Errorf("swap should be disabled, got %s", got)
	}
}

func TestV2ReadsStats(t *testing.T) {
	old := Root
	Root = fakeV2(t)
	defer func() { Root = old }()
	m := New("rh", "c2")
	m.Create(spec.Resources{})
	dir := filepath.Join(Root, "rh", "c2")
	os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 1500000\nuser_usec 1000000\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "memory.current"), []byte("1048576\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "memory.events"), []byte("low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pids.current"), []byte("7\n"), 0o644)
	s, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUUsageNanos != 1_500_000_000 || s.MemoryBytes != 1<<20 || s.MemoryLimit != 0 || s.OOMKills != 1 || s.Pids != 7 {
		t.Fatalf("stats = %+v", s)
	}
}
