package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// capNames maps capability names to their bit numbers (linux/capability.h).
var capNames = map[string]uint{
	"CAP_CHOWN": 0, "CAP_DAC_OVERRIDE": 1, "CAP_DAC_READ_SEARCH": 2,
	"CAP_FOWNER": 3, "CAP_FSETID": 4, "CAP_KILL": 5, "CAP_SETGID": 6,
	"CAP_SETUID": 7, "CAP_SETPCAP": 8, "CAP_LINUX_IMMUTABLE": 9,
	"CAP_NET_BIND_SERVICE": 10, "CAP_NET_BROADCAST": 11, "CAP_NET_ADMIN": 12,
	"CAP_NET_RAW": 13, "CAP_IPC_LOCK": 14, "CAP_IPC_OWNER": 15,
	"CAP_SYS_MODULE": 16, "CAP_SYS_RAWIO": 17, "CAP_SYS_CHROOT": 18,
	"CAP_SYS_PTRACE": 19, "CAP_SYS_PACCT": 20, "CAP_SYS_ADMIN": 21,
	"CAP_SYS_BOOT": 22, "CAP_SYS_NICE": 23, "CAP_SYS_RESOURCE": 24,
	"CAP_SYS_TIME": 25, "CAP_SYS_TTY_CONFIG": 26, "CAP_MKNOD": 27,
	"CAP_LEASE": 28, "CAP_AUDIT_WRITE": 29, "CAP_AUDIT_CONTROL": 30,
	"CAP_SETFCAP": 31, "CAP_MAC_OVERRIDE": 32, "CAP_MAC_ADMIN": 33,
	"CAP_SYSLOG": 34, "CAP_WAKE_ALARM": 35, "CAP_BLOCK_SUSPEND": 36,
	"CAP_AUDIT_READ": 37, "CAP_PERFMON": 38, "CAP_BPF": 39,
	"CAP_CHECKPOINT_RESTORE": 40,
}

// capMask converts names to a 64-bit mask. "ALL" keeps everything.
func capMask(names []string) (uint64, error) {
	var m uint64
	for _, n := range names {
		n = strings.ToUpper(n)
		if n == "ALL" {
			return ^uint64(0), nil
		}
		if !strings.HasPrefix(n, "CAP_") {
			n = "CAP_" + n
		}
		bit, ok := capNames[n]
		if !ok {
			return 0, fmt.Errorf("unknown capability %q", n)
		}
		m |= 1 << bit
	}
	return m, nil
}

// lastCap reads the highest capability the running kernel knows about.
func lastCap() uint {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 40
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 40
	}
	return uint(n)
}

// dropBounding removes every capability not in keep from the bounding set.
// The bounding set is the ceiling for what execve can grant; for a root
// process with no file capabilities, it is effectively the set the program
// runs with.
func dropBounding(keep uint64) error {
	last := lastCap()
	for c := uint(0); c <= last; c++ {
		if keep&(1<<c) != 0 {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil {
			return fmt.Errorf("drop bounding cap %d: %w", c, err)
		}
	}
	return nil
}

// setCaps sets effective, permitted and inheritable sets with capset(2).
// x/sys/unix exposes the raw types; the version-3 ABI splits the 64-bit sets
// into two 32-bit words.
func setCaps(keep uint64) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	for i := 0; i < 2; i++ {
		w := uint32(keep >> (32 * i))
		data[i] = unix.CapUserData{Effective: w, Permitted: w, Inheritable: w}
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	return nil
}

// currentCaps returns the effective set of this thread, for tests.
func currentCaps() (uint64, error) {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return 0, err
	}
	return uint64(data[0].Effective) | uint64(data[1].Effective)<<32, nil
}
