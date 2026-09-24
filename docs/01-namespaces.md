# Module 1: Processes and namespaces

> **Goal:** explain, syscall by syscall, what happens between `rh run alpine sh` and the shell prompt, and why each step is there.

## The one-sentence answer

A container is an ordinary Linux process that was started with **new namespaces** (so it sees its own PIDs, hostname, mounts and network), placed in a **cgroup** (so it is limited and measured), given a **different root filesystem** (with `pivot_root`), and stripped of most **capabilities** (so root inside is weaker than root outside). There is no "container" object in the kernel. There are only these primitives, and a runtime that combines them.

## Namespaces

A namespace wraps one global resource so that processes inside it get their own isolated instance.

| Namespace | Flag              | Isolates                                     | Roundhouse uses it for                          |
| --------- | ----------------- | -------------------------------------------- | ----------------------------------------------- |
| PID       | `CLONE_NEWPID`    | process IDs                                  | the app is PID 1 and sees only its own tree     |
| Mount     | `CLONE_NEWNS`     | the mount table                              | `/proc`, `/dev` and the pivoted root            |
| UTS       | `CLONE_NEWUTS`    | hostname and domain name                     | `--hostname`                                    |
| IPC       | `CLONE_NEWIPC`    | System V IPC, POSIX message queues           | isolating shared memory segments                |
| Network   | `CLONE_NEWNET`    | interfaces, routes, iptables, sockets, ports | the container's own `eth0` and `lo`             |
| Cgroup    | `CLONE_NEWCGROUP` | the view of `/proc/self/cgroup`              | hiding the host's cgroup paths                  |
| User      | `CLONE_NEWUSER`   | UID/GID mappings, capabilities               | not implemented: an [exercise](12-exercises.md) |
| Time      | `CLONE_NEWTIME`   | `CLOCK_MONOTONIC`/`BOOTTIME` offsets         | not used (useful for checkpoint/restore)        |

Three syscalls manage them:

- `clone(flags)` creates a child process in new namespaces.
- `unshare(flags)` moves the _calling_ process into new namespaces (with a PID-namespace twist, below).
- `setns(fd, type)` joins an existing namespace, given a file descriptor from `/proc/<pid>/ns/<type>`.

Every process's namespaces are visible as symlinks:

```sh
ls -l /proc/self/ns/
# lrwxrwxrwx ... pid -> 'pid:[4026531836]'   # the inode number identifies the namespace
```

Two processes are in the same namespace if and only if these inode numbers match.

## Lab 1: namespaces by hand, no runtime

```sh
# A new UTS namespace: changing the hostname does not affect the host.
sudo unshare --uts sh -c 'hostname sandbox; hostname'; hostname

# A new PID namespace. The first attempt surprises everyone:
sudo unshare --pid sh -c 'echo $$; ps | head -3'
#   → "sh: fork: Cannot allocate memory"? or you see host processes. Why?

sudo unshare --pid --fork sh -c 'echo $$'           # → 1
sudo unshare --pid --fork sh -c 'ps | head -3'      # → still host processes!
sudo unshare --pid --fork --mount-proc sh -c 'ps'   # → only sh and ps
```

The two surprises are the core lessons of this module:

1. **`unshare(CLONE_NEWPID)` does not move the caller.** A process's PID never changes, so the caller stays in the old namespace; only its _next child_ becomes PID 1 of the new one. That is why `--fork` is needed. (`man 7 pid_namespaces`: "the calling process is not moved into the new namespace.")
2. **`ps` reads `/proc`, and `/proc` shows the PID namespace of whoever mounted it.** Until you mount a fresh `proc` from inside the new namespace (which needs a new mount namespace, or you would replace the host's `/proc`), `ps` shows host processes. Isolation is only as good as the view.

Now the same with Roundhouse:

```sh
sudo -E rh run --rm --hostname box mirror.gcr.io/library/alpine:3.20 sh -c 'hostname; echo $$; ps'
ls -l /proc/$$/ns/ ; sudo -E rh run -d --name ns-demo mirror.gcr.io/library/alpine:3.20 sleep 600
sudo ls -l /proc/$(sudo cat $RH_ROOT/containers/*/state.json | grep '"pid"' | head -1 | grep -o '[0-9]*')/ns/
# Compare the inode numbers with your shell's: every one differs except user and time.
```

## How Roundhouse starts a container

Read [`internal/runtime/runtime.go`](../internal/runtime/runtime.go) and [`init.go`](../internal/runtime/init.go) alongside this.

### Step 1: re-execute ourselves

```go
cmd := exec.Command("/proc/self/exe", "__init")
cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: CLONE_NEWPID | CLONE_NEWNS | ...}
```

Why not `fork()` and continue in the child, as a C runtime might? Because the Go runtime is multi-threaded (garbage collector, scheduler threads), and after `fork()` only the calling thread exists in the child: locks held by other threads are held forever. Go therefore only offers fork-and-exec. The trick every Go runtime uses (runc included, and Liz Rice's talk) is to exec **its own binary** with a marker argument. `/proc/self/exe` is a magic link to the running executable, so this works even if the binary was deleted or replaced on disk.

`main()` in [`cmd/rh/main.go`](../cmd/rh/main.go) checks for that marker before anything else runs and jumps to `runtime.Init()`.

### Step 2: synchronize parent and child

The child is born in its new namespaces but must not proceed yet: the parent still has to put it in a cgroup (on cgroup v1) and plug in its network (the veth can only be moved into the network namespace once the namespace exists, and it exists only once the child does). So the child blocks reading a pipe:

```text
parent                                       child (__init)
──────                                       ──────────────
clone(CLONE_NEW*) + CLONE_INTO_CGROUP  ───►  born; blocks reading fd 3
add pid to cgroup (v1)
BeforeRelease(pid): veth into netns
write spec JSON to fd 3                ───►  reads the spec, continues
read fd 4 until EOF                          sethostname, mounts, pivot_root,
                                             drop capabilities, setuid,
                                     ◄───    execve(app)  (fd 4 is O_CLOEXEC:
                                                  exec closes it → parent sees EOF)
```

The error pipe is a neat trick borrowed from runc: the child marks it close-on-exec, so a _successful_ `execve` closes it and the parent reads EOF, while any failure before that writes a message the parent returns to the user. The parent learns "exec succeeded" without any extra round trip.

### Step 3: `LockOSThread`

Namespaces, capabilities and some `prctl` flags are attributes of a **thread**, not a process. If the Go scheduler moved the init goroutine to another OS thread halfway through (after `unshare(CLONE_NEWCGROUP)` but before `execve`, say), the exec would happen from a thread that never unshared. So `init()` in `init.go` calls `runtime.LockOSThread()` before `main` starts.

### Step 4: PID 1 is special

Inside the container, the application is PID 1, which comes with two kernel rules that most applications were never written for:

- **Signals:** the kernel does not apply default actions to PID 1 for signals it has not installed a handler for. `SIGTERM` to a shell script that is PID 1 does nothing. This is why `docker stop` often waits the full 10 seconds before `SIGKILL`.
- **Orphans:** when any process in the namespace dies, its children are re-parented to PID 1, which must `wait()` for them or they stay zombies forever, eventually exhausting the PID limit.

`rh run --init` keeps a ~40-line init as PID 1 ([`runInit`](../internal/runtime/init.go)): it forwards signals to the app and reaps every child. This is what [tini](https://github.com/krallin/tini) (`docker run --init`) does. The engine always uses it for deployed services.

```sh
# Feel the difference:
time sudo -E rh stop -t 5s $(sudo -E rh run -d mirror.gcr.io/library/alpine:3.20 sleep 600)          # ~5s: SIGTERM ignored
time sudo -E rh stop -t 5s $(sudo -E rh run -d --init mirror.gcr.io/library/alpine:3.20 sleep 600)   # instant
```

## `rh exec`: joining namespaces

`exec` must put a _new_ process into an _existing_ container. That is `setns()`, with two kernel constraints that shape the whole implementation ([`internal/nsenter/nsenter.go`](../internal/nsenter/nsenter.go)):

1. **`setns(CLONE_NEWNS)` fails with `EINVAL` in a multi-threaded process.** By the time Go's `main` runs, there are already several threads. The solution is a C function marked `__attribute__((constructor))`, which the dynamic loader runs before the Go runtime starts, while the process is still single-threaded. runc's `libcontainer/nsenter/nsexec.c` exists for exactly this reason.
2. **`setns(CLONE_NEWPID)` only affects children, and threads cannot be created until the process is in the namespace.** After joining the PID namespace, `clone(CLONE_THREAD)` returns `EINVAL`, and Go needs threads. So the constructor forks: the child is a real member of the container's PID namespace and goes on to run Go, while the parent waits and relays signals and the exit status. (Roundhouse hit this bug during development: the first version crashed with `pthread_create failed: Invalid argument`. See the git history.)

It also joins the container's **cgroup** before entering the mount namespace, because inside the container `/sys/fs/cgroup` is not the host's.

## Production comparison

| Roundhouse                 | runc                                                                                                      |
| -------------------------- | --------------------------------------------------------------------------------------------------------- |
| `runtime.Start` / `Init`   | `libcontainer/process_linux.go` (`initProcess.start`), `standard_init_linux.go`                           |
| JSON over a pipe           | same idea: bootstrap data over a socketpair (`_LIBCONTAINER_INITPIPE`)                                    |
| C constructor in `nsenter` | `libcontainer/nsenter/nsexec.c`, much more elaborate (user namespaces, the "stage-1/stage-2" double fork) |
| `runInit` (optional)       | not in runc; containerd/Docker's `--init` bundles tini                                                    |

runc's nsexec double-forks even on `run`, because with user namespaces the UID mapping must be written by the parent between stages. Roundhouse skips user namespaces, which is why it can use `SysProcAttr.Cloneflags` directly.

## Check yourself

1. Draw the parent/child sequence above from memory, including which pipe carries what.
2. Why is `/proc/self/exe` used rather than `os.Args[0]`?
3. A containerized shell script ignores `docker stop`. Explain the kernel rule, and give two fixes.
4. What would break if `init()` did not call `LockOSThread`?
5. Why does `rh exec` have two processes (`rh-exec` twice in `ps`) on the host side?
6. You `setns` into a container's network namespace from a Go program without the C constructor. Does it work? (Try it: network namespaces do not have the single-thread restriction, but the setting only applies to the calling thread.)
