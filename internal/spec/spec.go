// Package spec defines the configuration handed from the container manager to
// the low-level runtime. It is a deliberately small cousin of the OCI runtime
// spec (github.com/opencontainers/runtime-spec): every field here maps to one
// kernel primitive, so reading this file is a checklist of what "a container"
// actually is.
package spec

// Spec is everything the runtime needs to turn a root filesystem and a command
// into an isolated process tree.
type Spec struct {
	// ID names the container. It is used for the cgroup path and log lines.
	ID string `json:"id"`

	// RootFS is the absolute host path of the fully assembled root filesystem
	// (usually an overlayfs mount). The init process pivot_roots into it.
	RootFS string `json:"rootfs"`

	Process Process `json:"process"`

	// Hostname is set inside the UTS namespace.
	Hostname string `json:"hostname,omitempty"`

	Namespaces Namespaces `json:"namespaces"`

	Resources Resources `json:"resources"`

	// Mounts are extra bind mounts (volumes) applied after the standard
	// /proc, /sys and /dev mounts and before pivot_root.
	Mounts []Mount `json:"mounts,omitempty"`

	// ReadonlyRootfs remounts / read-only after pivot_root.
	ReadonlyRootfs bool `json:"readonlyRootfs,omitempty"`

	// CgroupParent is the cgroup directory (relative to each hierarchy root)
	// under which this container's cgroup is created. Default "roundhouse".
	CgroupParent string `json:"cgroupParent,omitempty"`
}

// Process describes the program that runs inside the container.
type Process struct {
	Args []string `json:"args"`
	Env  []string `json:"env,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	UID  uint32   `json:"uid"`
	GID  uint32   `json:"gid"`

	// AdditionalGIDs are supplementary groups (setgroups).
	AdditionalGIDs []uint32 `json:"additionalGids,omitempty"`

	// Capabilities is the set of capability names (e.g. "CAP_NET_BIND_SERVICE")
	// kept in the bounding, permitted, effective and inheritable sets. Nil
	// means DefaultCapabilities.
	Capabilities []string `json:"capabilities,omitempty"`

	// NoNewPrivileges sets PR_SET_NO_NEW_PRIVS so setuid binaries and file
	// capabilities cannot raise privileges again.
	NoNewPrivileges bool `json:"noNewPrivileges,omitempty"`

	// Init keeps a tiny init (signal forwarding + zombie reaping) as PID 1
	// and runs Args as its child, like `docker run --init` / tini.
	Init bool `json:"init,omitempty"`

	// Rlimits applied before exec, e.g. {"RLIMIT_NOFILE", 1024, 1024}.
	Rlimits []Rlimit `json:"rlimits,omitempty"`
}

// Namespaces selects which namespaces are unshared. A namespace left false is
// shared with the host (Net false is "host networking").
type Namespaces struct {
	PID   bool `json:"pid"`
	UTS   bool `json:"uts"`
	IPC   bool `json:"ipc"`
	Mount bool `json:"mount"`
	Net   bool `json:"net"`
	// Cgroup namespace hides the host cgroup path from /proc/self/cgroup.
	Cgroup bool `json:"cgroup"`
}

// Resources are enforced by cgroups.
type Resources struct {
	// MemoryBytes is the hard memory limit. 0 means unlimited.
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	// CPUMillis is the CPU quota in thousandths of a core (500 = half a core).
	// 0 means unlimited.
	CPUMillis int64 `json:"cpuMillis,omitempty"`
	// PidsMax caps the number of tasks, which is what stops a fork bomb.
	// 0 means unlimited.
	PidsMax int64 `json:"pidsMax,omitempty"`
}

// Mount is a bind mount from the host into the container.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Rlimit mirrors setrlimit(2).
type Rlimit struct {
	Type string `json:"type"`
	Soft uint64 `json:"soft"`
	Hard uint64 `json:"hard"`
}

// DefaultCapabilities is the same conservative set Docker grants by default.
// Everything else, notably CAP_SYS_ADMIN, is dropped.
var DefaultCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FSETID",
	"CAP_FOWNER",
	"CAP_MKNOD",
	"CAP_NET_RAW",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT",
	"CAP_KILL",
	"CAP_AUDIT_WRITE",
}

// AllNamespaces returns a Namespaces value with every namespace enabled.
func AllNamespaces() Namespaces {
	return Namespaces{PID: true, UTS: true, IPC: true, Mount: true, Net: true, Cgroup: true}
}
