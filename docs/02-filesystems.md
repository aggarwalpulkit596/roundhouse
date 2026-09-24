# Module 2: Filesystems

> **Goal:** know exactly what a container's `/` is, how it is assembled from image layers in milliseconds, and the three classic ways to get it wrong.

## chroot is not isolation

`chroot(dir)` changes one thing: the directory the calling process resolves `/` against. It does not change the mount table, it does not close file descriptors that point outside, and a process with `CAP_SYS_CHROOT` can escape it:

```c
mkdir("x"); chroot("x");        // new root is below our cwd…
chdir("../../../../..");        // …so cwd is now outside the root and can climb
chroot(".");                    // make the real root our root again
```

The Liz Rice talk uses `chroot` because it is the simplest thing that works on stage. Real runtimes use `pivot_root`.

## pivot_root

`pivot_root(new_root, put_old)` swaps the root **mount** of the whole mount namespace: `new_root` becomes `/` and the old root is moved to `put_old`, from where it can be unmounted. Once it is unmounted, the host filesystem is not hidden: it is simply not in this namespace any more, and there is no path to climb to.

Roundhouse's [`pivotRoot`](../internal/runtime/mounts.go) uses runc's modern trick, which needs no temporary directory:

```go
unix.Chdir(root)
unix.PivotRoot(".", ".")                      // old root now stacked on top of new root, at "."
unix.Mount("", ".", "", MS_REC|MS_SLAVE, "")  // stop unmount events propagating anywhere
unix.Unmount(".", MNT_DETACH)                 // peel the old root off
unix.Chdir("/")
```

`pivot_root` has preconditions that explain the lines before it in `setupRootfs`:

1. **We need our own mount namespace** (`CLONE_NEWNS`), or we would pivot the host.
2. **Mounts must not propagate back to the host.** systemd makes `/` a _shared_ mount, so a mount created in a child namespace would appear on the host too. Hence `mount("", "/", MS_REC|MS_PRIVATE)` first. Read `man 7 mount_namespaces` on shared, private, slave and unbindable propagation.
3. **`new_root` must be a mount point.** An overlay mount already is, but runc bind-mounts the rootfs onto itself anyway to make any directory work. So does Roundhouse.

Between the bind and the pivot, the runtime builds the container's view:

| Mount            | Type                   | Why                                                                                                               |
| ---------------- | ---------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `/proc`          | `proc`                 | Must be mounted from inside the PID namespace to show only its processes                                          |
| `/dev`           | `tmpfs`                | A fresh, nearly empty `/dev`: no disks, no `/dev/mem`, no `/dev/kmsg`                                             |
| `/dev/null` …    | `mknod` char devs      | `null zero full random urandom tty`, the set POSIX programs assume                                                |
| `/dev/pts`       | `devpts` (newinstance) | The container's own pseudo-terminals                                                                              |
| `/dev/shm`       | `tmpfs`                | POSIX shared memory, sized so it cannot eat host RAM                                                              |
| `/sys`           | `sysfs` read-only      | Only possible with a new network namespace (sysfs is tied to it)                                                  |
| `/sys/fs/cgroup` | `cgroup2` ro           | With a cgroup namespace, shows only the container's own cgroup (the JVM reads `memory.max` here to size its heap) |
| volumes          | bind mounts            | Host directories; the target is resolved with `SecureJoin` (below)                                                |

After the pivot, [`maskPaths`](../internal/runtime/mounts.go) hides `/proc/kcore`, `/proc/keys`, `/proc/sched_debug` and friends behind `/dev/null`, and remounts `/proc/sys`, `/proc/sysrq-trigger` and similar read-only. The same lists appear in the OCI runtime spec's defaults (`maskedPaths`, `readonlyPaths`).

## overlayfs: how a 1 GB image starts in milliseconds

Copying an image's files for each container would take seconds and waste disk. Instead, the image layers are stacked with overlayfs:

```text
mount -t overlay overlay \
  -o lowerdir=/layers/L3:/layers/L2:/layers/L1,upperdir=/c/upper,workdir=/c/work  /c/rootfs
```

- **lowerdirs** are the image layers, read-only, **topmost first** (L3 is the newest layer). Many containers share them.
- **upperdir** is per-container and starts empty. All writes land here.
- **workdir** is scratch space overlayfs needs on the same filesystem as upperdir (for atomic copy-up).
- **Reading** a file returns it from the topmost layer that has it.
- **Writing** to a lower file triggers _copy-up_: the whole file is copied into upper first. (Appending one byte to a 2 GB log in a lower layer copies 2 GB. That is why databases put data on volumes.)
- **Deleting** a lower file creates a _whiteout_ in upper: a character device with device number 0/0.
- **Replacing a directory** marks it _opaque_ with the xattr `trusted.overlay.opaque=y`: nothing below shows through.

Starting a container is therefore one `mount(2)` call. Read [`internal/rootfs/overlay.go`](../internal/rootfs/overlay.go). Note the page-size limit on mount options: the kernel copies them into a single page, so dozens of layers with long paths eventually stop fitting. containerd solves this by mounting from inside the layer directory with relative paths.

## Image layers and whiteouts

An OCI layer is a tar file (usually gzipped). A tar has no way to say "delete this file", so the [OCI image spec](https://github.com/opencontainers/image-spec/blob/main/layer.md#whiteouts) defines conventions:

| In the tar               | Meaning                                 | On disk after Roundhouse unpacks it             |
| ------------------------ | --------------------------------------- | ----------------------------------------------- |
| `etc/.wh.motd`           | `etc/motd` was deleted in this layer    | char device 0/0 at `etc/motd`                   |
| `var/cache/.wh..wh..opq` | everything below in `var/cache` is gone | xattr `trusted.overlay.opaque=y` on `var/cache` |

Roundhouse stores each layer **already translated to overlay format**, exactly like containerd's overlayfs snapshotter, so layers never need re-processing. The builder does the reverse when it turns a RUN step's upperdir back into a tar ([`WriteLayer`](../internal/image/layer.go)).

Because each layer unpacks into its own directory, unpacking is independent per layer and Roundhouse does it in parallel. A naive "extract every layer into one directory" engine (early Docker's vfs driver) has to go in order.

## Three classic ways to get it wrong

### 1. Symlink escapes during unpack

A malicious layer ships a symlink `etc -> /etc` followed by a file `etc/cron.d/evil`. An unpacker that simply opens `dir + "/etc/cron.d/evil"` follows the symlink and writes the **host's** `/etc/cron.d/evil`: root code execution on the next minute. The same bug class has hit `docker cp` (CVE-2018-15664) and runc volume mounts (CVE-2021-30465), both via symlinks swapped in at the right moment.

The fix is to resolve every path component as if `dir` were `/`, following symlinks but never leaving it. That is [`fsutil.SecureJoin`](../internal/fsutil/securejoin.go), a small version of [`cyphar/filepath-securejoin`](https://github.com/cyphar/filepath-securejoin). The unpacker uses it for every entry's parent, and for hardlink targets. [`TestUnpackCannotEscapeThroughSymlinks`](../internal/image/image_test.go) proves it. Modern kernels offer `openat2(RESOLVE_IN_ROOT)`, which does this in the kernel without the time-of-check/time-of-use race a userspace walk has. Upgrading to it is a good exercise.

### 2. Volume targets that are symlinks

The same attack via a volume mount target: the image contains `/data -> /etc`, and you bind-mount a host directory "at /data". [`bindVolume`](../internal/runtime/mounts.go) resolves the target with `SecureJoin` too.

### 3. Metadata-shadowing in the upper layer

When the builder runs `COPY app /tmp/app`, it creates `/tmp` in the new layer. If it creates it with mode `0755`, that directory's metadata **wins** in the overlay, and `/tmp` (normally `1777`) is no longer world-writable in the final image. [`mkdirLike`](../internal/builder/builder.go) copies each parent directory's mode and owner from the layers below. Docker had this bug once as well.

## Lab 2

```sh
sudo -E rh pull mirror.gcr.io/library/alpine:3.20
tree -L 3 $RH_ROOT/images | head -30
cat $RH_ROOT/images/images.json

# Run a container, change files, and look at its upper dir from the host:
sudo -E rh run -d --name fs-demo mirror.gcr.io/library/alpine:3.20 sh -c 'rm /etc/motd; mkdir -p /new; echo hi > /new/f; sleep 600'
sudo ls -la $RH_ROOT/containers/*/upper/etc/     # motd is a "c" (char device) whiteout
sudo getfattr -d -m - $RH_ROOT/containers/*/upper/new 2>/dev/null
mount | grep overlay | grep $RH_ROOT              # the lowerdir/upperdir line

# Copy-up in action:
sudo -E rh exec fs-demo sh -c 'echo >> /etc/profile'
sudo ls -la $RH_ROOT/containers/*/upper/etc/     # profile was copied up
sudo -E rh rm -f fs-demo
```

Then try the chroot escape from a small C or Go program in a chroot (not in a Roundhouse container: why not?).

## Check yourself

1. Why must `/` be made a private mount before `pivot_root`?
2. Explain copy-up. Which workloads suffer from it, and what do platforms do about it?
3. What is the difference between a whiteout and an opaque directory, and when does a build produce each?
4. Why is it safe for a thousand containers to share one lowerdir?
5. What does `SecureJoin("/root", "a/../../etc/passwd")` return, and why is that correct?
