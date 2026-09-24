// Package cgroups enforces and measures container resources.
//
// Two implementations share one interface:
//
//   - v2 (unified hierarchy): one directory per container under
//     /sys/fs/cgroup, controllers enabled through cgroup.subtree_control.
//     This is what every modern distro and Kubernetes node uses.
//   - v1 (legacy/hybrid): one directory per controller hierarchy
//     (/sys/fs/cgroup/memory/..., /sys/fs/cgroup/cpu/..., ...).
//
// The runtime only ever sees Manager, the same way runc's libcontainer hides
// fs vs fs2 behind cgroups.Manager.
package cgroups

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/spec"
)

// Root is the cgroup filesystem mount point. Tests may override it.
var Root = "/sys/fs/cgroup"

// Stats is a point-in-time resource reading.
type Stats struct {
	// CPUUsageNanos is cumulative CPU time consumed by all tasks.
	CPUUsageNanos uint64 `json:"cpuUsageNanos"`
	// MemoryBytes is the current charge (RSS + page cache).
	MemoryBytes uint64 `json:"memoryBytes"`
	// MemoryLimit is the enforced limit, 0 when unlimited.
	MemoryLimit uint64 `json:"memoryLimit"`
	// OOMKills counts processes the kernel OOM killer took from this group.
	OOMKills uint64 `json:"oomKills"`
	// Pids is the number of live tasks.
	Pids uint64 `json:"pids"`
}

// Manager controls one container's cgroup.
type Manager interface {
	// Create makes the cgroup and applies limits. It is idempotent.
	Create(r spec.Resources) error
	// Add moves a process into the cgroup.
	Add(pid int) error
	// Procs lists the pids currently in the cgroup.
	Procs() ([]int, error)
	// Stats reads counters.
	Stats() (Stats, error)
	// Freeze stops every task so a kill cannot race new forks.
	Freeze() error
	Thaw() error
	// Kill SIGKILLs every task in the cgroup.
	Kill() error
	// Destroy removes the cgroup. Tasks must be gone.
	Destroy() error
	// Path is the unified path (v2) or the memory path (v1), for debugging.
	Path() string
	// Version is 1 or 2.
	Version() int
	// ProcsFiles lists the cgroup.procs file(s) a process must be written
	// to in order to join (one on v2, one per controller on v1).
	ProcsFiles() []string
}

// IsV2 reports whether the host runs the unified hierarchy with controllers
// available at the root. Hybrid hosts that only mount an empty cgroup2 at
// /sys/fs/cgroup/unified are treated as v1.
func IsV2() bool {
	b, err := os.ReadFile(filepath.Join(Root, "cgroup.controllers"))
	return err == nil && strings.Contains(string(b), "memory")
}

// New returns the right Manager for this host. parent is a relative path such
// as "roundhouse" and id is the container id.
func New(parent, id string) Manager {
	if parent == "" {
		parent = "roundhouse"
	}
	if IsV2() {
		return &v2{path: filepath.Join(Root, parent, id), parent: filepath.Join(Root, parent)}
	}
	return &v1{parent: parent, id: id}
}

// ---------------------------------------------------------------------------
// v2

type v2 struct {
	path   string
	parent string
}

func (c *v2) Version() int         { return 2 }
func (c *v2) Path() string         { return c.path }
func (c *v2) file(n string) string { return filepath.Join(c.path, n) }

func (c *v2) Create(r spec.Resources) error {
	// Controllers must be delegated down every level: a child cgroup only
	// gets "memory" if its parent lists +memory in cgroup.subtree_control.
	if err := os.MkdirAll(c.parent, 0o755); err != nil {
		return err
	}
	for _, dir := range []string{Root, c.parent} {
		for _, ctrl := range []string{"cpu", "memory", "pids"} {
			// Ignore errors: the controller may be missing or already on.
			_ = write(filepath.Join(dir, "cgroup.subtree_control"), "+"+ctrl)
		}
	}
	if err := os.MkdirAll(c.path, 0o755); err != nil {
		return err
	}
	if r.MemoryBytes > 0 {
		if err := write(c.file("memory.max"), strconv.FormatInt(r.MemoryBytes, 10)); err != nil {
			return fmt.Errorf("memory.max: %w", err)
		}
		// Without this the "limit" silently spills into swap.
		_ = write(c.file("memory.swap.max"), "0")
	}
	if r.CPUMillis > 0 {
		const period = 100000 // 100ms, the kernel default CFS period
		quota := r.CPUMillis * period / 1000
		if err := write(c.file("cpu.max"), fmt.Sprintf("%d %d", quota, period)); err != nil {
			return fmt.Errorf("cpu.max: %w", err)
		}
	}
	if r.PidsMax > 0 {
		if err := write(c.file("pids.max"), strconv.FormatInt(r.PidsMax, 10)); err != nil {
			return fmt.Errorf("pids.max: %w", err)
		}
	}
	return nil
}

func (c *v2) ProcsFiles() []string { return []string{c.file("cgroup.procs")} }

func (c *v2) Add(pid int) error { return write(c.file("cgroup.procs"), strconv.Itoa(pid)) }

func (c *v2) Procs() ([]int, error) { return readPids(c.file("cgroup.procs")) }

func (c *v2) Stats() (Stats, error) {
	var s Stats
	kv, err := readKV(c.file("cpu.stat"))
	if err != nil {
		return s, err
	}
	s.CPUUsageNanos = kv["usage_usec"] * 1000
	s.MemoryBytes, _ = readUint(c.file("memory.current"))
	s.MemoryLimit, _ = readUint(c.file("memory.max")) // "max" parses as 0
	if ev, err := readKV(c.file("memory.events")); err == nil {
		s.OOMKills = ev["oom_kill"]
	}
	s.Pids, _ = readUint(c.file("pids.current"))
	return s, nil
}

func (c *v2) Freeze() error { return write(c.file("cgroup.freeze"), "1") }
func (c *v2) Thaw() error   { return write(c.file("cgroup.freeze"), "0") }

func (c *v2) Kill() error {
	// cgroup.kill (Linux 5.14+) kills the whole tree atomically.
	if err := write(c.file("cgroup.kill"), "1"); err == nil {
		return nil
	}
	return killAll(c)
}

func (c *v2) Destroy() error { return rmdirRetry(c.path) }

// ---------------------------------------------------------------------------
// v1

type v1 struct {
	parent, id string
}

var v1Controllers = []string{"memory", "cpu", "cpuacct", "pids", "freezer"}

func (c *v1) Version() int { return 1 }
func (c *v1) dir(ctrl string) string {
	return filepath.Join(Root, ctrl, c.parent, c.id)
}
func (c *v1) Path() string { return c.dir("memory") }

func (c *v1) Create(r spec.Resources) error {
	for _, ctrl := range v1Controllers {
		if _, err := os.Stat(filepath.Join(Root, ctrl)); err != nil {
			continue // controller not mounted on this host
		}
		if err := os.MkdirAll(c.dir(ctrl), 0o755); err != nil {
			return err
		}
	}
	if r.MemoryBytes > 0 {
		if err := write(filepath.Join(c.dir("memory"), "memory.limit_in_bytes"), strconv.FormatInt(r.MemoryBytes, 10)); err != nil {
			return fmt.Errorf("memory.limit_in_bytes: %w", err)
		}
		_ = write(filepath.Join(c.dir("memory"), "memory.memsw.limit_in_bytes"), strconv.FormatInt(r.MemoryBytes, 10))
		_ = write(filepath.Join(c.dir("memory"), "memory.swappiness"), "0")
	}
	if r.CPUMillis > 0 {
		const period = 100000
		_ = write(filepath.Join(c.dir("cpu"), "cpu.cfs_period_us"), strconv.Itoa(period))
		if err := write(filepath.Join(c.dir("cpu"), "cpu.cfs_quota_us"), strconv.FormatInt(r.CPUMillis*period/1000, 10)); err != nil {
			return fmt.Errorf("cpu.cfs_quota_us: %w", err)
		}
	}
	if r.PidsMax > 0 {
		if err := write(filepath.Join(c.dir("pids"), "pids.max"), strconv.FormatInt(r.PidsMax, 10)); err != nil {
			return fmt.Errorf("pids.max: %w", err)
		}
	}
	return nil
}

func (c *v1) ProcsFiles() []string {
	var out []string
	for _, ctrl := range v1Controllers {
		if _, err := os.Stat(c.dir(ctrl)); err == nil {
			out = append(out, filepath.Join(c.dir(ctrl), "cgroup.procs"))
		}
	}
	return out
}

func (c *v1) Add(pid int) error {
	for _, ctrl := range v1Controllers {
		d := c.dir(ctrl)
		if _, err := os.Stat(d); err != nil {
			continue
		}
		if err := write(filepath.Join(d, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
			return fmt.Errorf("%s: %w", ctrl, err)
		}
	}
	return nil
}

func (c *v1) Procs() ([]int, error) {
	// pids is the controller most likely to hold every task.
	for _, ctrl := range []string{"pids", "memory", "cpu"} {
		if p, err := readPids(filepath.Join(c.dir(ctrl), "cgroup.procs")); err == nil {
			return p, nil
		}
	}
	return nil, os.ErrNotExist
}

func (c *v1) Stats() (Stats, error) {
	var s Stats
	var err error
	s.CPUUsageNanos, err = readUint(filepath.Join(c.dir("cpuacct"), "cpuacct.usage"))
	if err != nil {
		return s, err
	}
	s.MemoryBytes, _ = readUint(filepath.Join(c.dir("memory"), "memory.usage_in_bytes"))
	if lim, err := readUint(filepath.Join(c.dir("memory"), "memory.limit_in_bytes")); err == nil && lim < 1<<62 {
		s.MemoryLimit = lim
	}
	if kv, err := readKV(filepath.Join(c.dir("memory"), "memory.oom_control")); err == nil {
		s.OOMKills = kv["oom_kill"]
	}
	s.Pids, _ = readUint(filepath.Join(c.dir("pids"), "pids.current"))
	return s, nil
}

func (c *v1) Freeze() error { return write(filepath.Join(c.dir("freezer"), "freezer.state"), "FROZEN") }
func (c *v1) Thaw() error   { return write(filepath.Join(c.dir("freezer"), "freezer.state"), "THAWED") }
func (c *v1) Kill() error   { return killAll(c) }

func (c *v1) Destroy() error {
	var errs []error
	for _, ctrl := range v1Controllers {
		if err := rmdirRetry(c.dir(ctrl)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// helpers

// killAll freezes the group so no task can fork during the sweep, SIGKILLs
// every member, then thaws so the kernel can deliver the signals.
func killAll(m Manager) error {
	frozen := m.Freeze() == nil
	pids, err := m.Procs()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	if frozen {
		return m.Thaw()
	}
	return nil
}

func rmdirRetry(dir string) error {
	var err error
	for i := 0; i < 50; i++ {
		err = syscall.Rmdir(dir)
		if err == nil || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		// EBUSY while the last tasks are still exiting.
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("rmdir %s: %w", dir, err)
}

func write(path, v string) error {
	return os.WriteFile(path, []byte(v), 0o644)
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

func readKV(path string) (map[string]uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			out[f[0]] = v
		}
	}
	return out, nil
}

func readPids(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(f); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}
