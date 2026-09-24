package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/cgroups"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/fsutil"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/spec"
)

// setupRootfs builds the container's view of the filesystem inside its new
// mount namespace and then swaps it in with pivot_root.
func setupRootfs(s *spec.Spec) error {
	root := s.RootFS

	// 1. Stop mount events propagating back to the host. Many distros make
	//    "/" a shared mount (systemd does), so without this our /proc mount
	//    would appear on the host too.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}

	// 2. pivot_root requires new_root to be a mount point. Bind it onto
	//    itself to guarantee that, even if RootFS is a plain directory.
	if err := unix.Mount(root, root, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs: %w", err)
	}

	type m struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}
	const nsd = unix.MS_NOSUID | unix.MS_NODEV
	mounts := []m{
		// /proc shows the new PID namespace's processes only because it is
		// mounted from inside that namespace.
		{"proc", "/proc", "proc", nsd | unix.MS_NOEXEC, ""},
		{"tmpfs", "/dev", "tmpfs", unix.MS_NOSUID | unix.MS_STRICTATIME, "mode=755,size=65536k"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"},
		{"shm", "/dev/shm", "tmpfs", nsd | unix.MS_NOEXEC, "mode=1777,size=65536k"},
	}
	if s.Namespaces.IPC {
		mounts = append(mounts, m{"mqueue", "/dev/mqueue", "mqueue", nsd | unix.MS_NOEXEC, ""})
	}
	for _, mt := range mounts {
		dst := filepath.Join(root, mt.target)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(mt.source, dst, mt.fstype, mt.flags, mt.data); err != nil {
			return fmt.Errorf("mount %s: %w", mt.target, err)
		}
	}

	// sysfs can only be freshly mounted by the owner of the network
	// namespace; with host networking fall back to a read-only bind.
	sys := filepath.Join(root, "sys")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		return err
	}
	if s.Namespaces.Net {
		if err := unix.Mount("sysfs", sys, "sysfs", nsd|unix.MS_NOEXEC|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("mount /sys: %w", err)
		}
	} else if err := bindReadonly("/sys", sys, true); err != nil {
		return fmt.Errorf("bind /sys: %w", err)
	}
	if s.Namespaces.Cgroup && cgroups.IsV2() {
		// A cgroup-namespaced cgroup2 mount shows the container only its own
		// subtree. Runtimes such as the JVM read memory.max from here to
		// size their heaps.
		cg := filepath.Join(sys, "fs", "cgroup")
		if err := unix.Mount("cgroup", cg, "cgroup2", nsd|unix.MS_NOEXEC|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("mount cgroup2: %w", err)
		}
	}

	if err := createDevices(root); err != nil {
		return err
	}

	for _, vm := range s.Mounts {
		if err := bindVolume(root, vm); err != nil {
			return err
		}
	}

	if err := pivotRoot(root); err != nil {
		return err
	}

	maskPaths()

	if s.ReadonlyRootfs {
		if err := unix.Mount("", "/", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount / ro: %w", err)
		}
	}
	return nil
}

// pivotRoot swaps the root mount. Unlike chroot, which only changes one
// process's idea of "/", pivot_root moves the whole namespace's root and lets
// us detach the old one, so there is no host tree left to escape into.
//
// The pivot_root(".", ".") form stacks the old root on top of the new one in
// the same directory; unmounting "." then removes the old root. This is the
// trick runc uses to avoid creating a temporary put_old directory.
func pivotRoot(root string) error {
	if err := unix.Chdir(root); err != nil {
		return fmt.Errorf("chdir rootfs: %w", err)
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// Make the old root private before detaching so the unmount does not
	// propagate anywhere.
	if err := unix.Mount("", ".", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("slave old root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return unix.Chdir("/")
}

// createDevices populates the fresh /dev tmpfs with the handful of nodes
// POSIX programs assume exist. Everything else in the host /dev (disks,
// /dev/kmsg, /dev/mem) is simply absent.
func createDevices(root string) error {
	devs := []struct {
		name         string
		major, minor uint32
	}{
		{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7},
		{"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0},
	}
	oldMask := unix.Umask(0)
	defer unix.Umask(oldMask)
	for _, d := range devs {
		p := filepath.Join(root, "dev", d.name)
		if err := unix.Mknod(p, unix.S_IFCHR|0o666, int(unix.Mkdev(d.major, d.minor))); err != nil {
			// mknod is refused in some sandboxes; bind the host node instead.
			f, ferr := os.Create(p)
			if ferr != nil {
				return fmt.Errorf("mknod %s: %w", d.name, err)
			}
			f.Close()
			if berr := unix.Mount("/dev/"+d.name, p, "", unix.MS_BIND, ""); berr != nil {
				return fmt.Errorf("mknod %s: %w (bind fallback: %v)", d.name, err, berr)
			}
		}
	}
	links := map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
		"ptmx":   "pts/ptmx",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, "dev", name)); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// bindVolume bind-mounts a host path into the container. The target is
// resolved with SecureJoin so a malicious image cannot plant a symlink
// (e.g. /data -> /etc on the host) that redirects the mount.
func bindVolume(root string, vm spec.Mount) error {
	dst, err := fsutil.SecureJoin(root, vm.Target)
	if err != nil {
		return err
	}
	fi, err := os.Stat(vm.Source)
	if err != nil {
		return fmt.Errorf("volume source %s: %w", vm.Source, err)
	}
	if fi.IsDir() {
		err = os.MkdirAll(dst, 0o755)
	} else {
		if err = os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
			var f *os.File
			if f, err = os.OpenFile(dst, os.O_CREATE, 0o644); err == nil {
				f.Close()
			}
		}
	}
	if err != nil {
		return fmt.Errorf("volume target %s: %w", vm.Target, err)
	}
	if err := unix.Mount(vm.Source, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s: %w", vm.Target, err)
	}
	if vm.ReadOnly {
		return unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, "")
	}
	return nil
}

func bindReadonly(src, dst string, rec bool) error {
	flags := uintptr(unix.MS_BIND)
	if rec {
		flags |= unix.MS_REC
	}
	if err := unix.Mount(src, dst, "", flags, ""); err != nil {
		return err
	}
	return unix.Mount("", dst, "", flags|unix.MS_REMOUNT|unix.MS_RDONLY, "")
}

// maskedPaths leak host information or allow host-level actions even from a
// namespaced /proc. They are hidden behind /dev/null or an empty tmpfs.
var maskedPaths = []string{
	"/proc/kcore", "/proc/keys", "/proc/timer_list", "/proc/sched_debug",
	"/proc/latency_stats", "/proc/acpi", "/proc/scsi", "/sys/firmware",
}

// readonlyPaths stay visible but cannot be written, e.g. sysctls.
var readonlyPaths = []string{
	"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
}

func maskPaths() {
	for _, p := range maskedPaths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			_ = unix.Mount("tmpfs", p, "tmpfs", unix.MS_RDONLY, "size=0")
		} else {
			_ = unix.Mount("/dev/null", p, "", unix.MS_BIND, "")
		}
	}
	for _, p := range readonlyPaths {
		if _, err := os.Stat(p); err == nil {
			_ = bindReadonly(p, p, true)
		}
	}
}
