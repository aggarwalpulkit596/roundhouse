# F1: Linux fundamentals for infrastructure work

> **Goal:** be fluent in the Linux ideas every later module builds on: processes, system calls, file descriptors, signals, users and permissions, the filesystem tree, mounts, and `/proc`. Days 1–2 of the [20-day plan](../00-study-plan.md).

If you have mostly used Linux through `docker compose up` and CI logs, this module fills the gap. Do not skip it: every container concept is one of these ideas with a twist.

## 1. Everything is a process

A **process** is a running program: code, memory, open files, a user identity, and a parent. Every process has a **PID** and a parent PID (**PPID**). PID 1 (`systemd` on most servers) starts everything else, so processes form a tree.

```sh
ps -ef | head                 # every process: user, PID, PPID, command
pstree -p | head -30          # the tree
echo $$                       # your shell's PID
sleep 1000 &                  # start a background child
ps -o pid,ppid,stat,cmd --ppid $$   # your shell's children
kill %1                       # stop it
```

How a new process starts, which is the most important sequence in this curriculum:

1. **`fork()`** duplicates the calling process. The child is an almost exact copy, with a new PID.
2. **`execve(path, args, env)`** replaces the child's program with a new one. Same PID, new code.
3. The parent calls **`wait()`** to collect the child's **exit status** when it finishes.

A shell running `ls` does exactly this: fork, exec `ls` in the child, wait. A container runtime does the same with extra flags. That is literally what [module 1](../01-namespaces.md) is about.

**Exit status:** 0 means success; anything else is failure by convention. If a process is killed by a signal, shells report 128 + the signal number: **137 = 128 + 9 (SIGKILL)**, **143 = 128 + 15 (SIGTERM)**. You will see 137 every time a container runs out of memory.

**Zombies and orphans:** a child that has exited but not yet been `wait()`ed for is a **zombie** (state `Z` in `ps`): only its exit status remains. A child whose parent died is an **orphan** and gets re-parented to PID 1 (or to the nearest "subreaper"), which must reap it. Remember this for the "PID 1 problem" in module 1.

## 2. System calls: the kernel's API

Programs cannot touch hardware, other processes or the network directly. They ask the kernel through **system calls**: `open`, `read`, `write`, `fork`, `execve`, `socket`, `connect`, `mount`, `kill`, and a few hundred more. Libraries (libc, Go's runtime) wrap them.

`strace` shows every system call a program makes. It is the single most useful debugging tool you will learn:

```sh
strace -f -e trace=process sh -c 'ls > /dev/null'   # fork/clone, execve, wait4
strace -e trace=openat cat /etc/hostname            # which files it opens
strace -c ls /                                      # a summary: which calls, how many
```

Read `man 2 fork` and `man 2 execve`. Section 2 of the manual is system calls; section 7 is overviews (`man 7 signal`, `man 7 namespaces`). [man7.org](https://man7.org/linux/man-pages/) has them all online.

## 3. File descriptors: everything is a file

A process refers to open files, pipes, sockets and devices by small integers called **file descriptors** (fds). By convention 0 is stdin, 1 stdout, 2 stderr.

```sh
ls -l /proc/$$/fd                   # your shell's open fds
sleep 1000 > /tmp/out 2>&1 &        # redirect: fd 1 and 2 point at a file
ls -l /proc/$!/fd
```

- A **pipe** (`a | b`) connects one process's fd 1 to another's fd 0.
- Fds are **inherited** across `fork` and **kept** across `execve` unless marked close-on-exec (`O_CLOEXEC`). Roundhouse uses this deliberately: it passes pipes to the container's init as fd 3 and 4, and relies on close-on-exec to learn that `execve` succeeded (module 1).
- `docker logs` works because the runtime holds the other end of the container's stdout and stderr pipes and writes what it reads to a file.

## 4. Signals

A **signal** is a tiny asynchronous message to a process. The ones you will meet constantly:

| Signal  | Number | Default action                  | Typical meaning                                             |
| ------- | ------ | ------------------------------- | ----------------------------------------------------------- |
| SIGINT  | 2      | terminate                       | Ctrl-C                                                      |
| SIGTERM | 15     | terminate                       | "please shut down cleanly" (what `docker stop` sends first) |
| SIGKILL | 9      | terminate, **cannot be caught** | "die now" (after the grace period, or the OOM killer)       |
| SIGHUP  | 1      | terminate                       | terminal closed; often "reload config" for daemons          |
| SIGCHLD | 17     | ignore                          | "one of your children changed state"; reap it               |

A program can install a **handler** for most signals (graceful shutdown: stop accepting work, finish in-flight requests, exit). SIGKILL and SIGSTOP cannot be handled.

```sh
sh -c 'trap "echo got TERM; exit 0" TERM; while :; do sleep 1; done' &
kill -TERM $!         # the handler runs
sleep 1000 & kill -9 $!; wait $!; echo "exit: $?"   # 137
```

## 5. Users, groups and permissions

Every process runs as a **user ID (UID)** and **group IDs**. Files have an owner, a group and permission bits (`rwx` for owner, group, others). **UID 0 (root)** bypasses most permission checks. In [module 4](../04-security.md) you will see that root's power is actually split into ~40 **capabilities**, and containers drop most of them.

```sh
id                      # your uid, gid, groups
ls -l /etc/shadow       # only root can read it
stat /tmp               # note the "t" (sticky bit): anyone can create, only owners delete
sudo -u nobody id
```

## 6. The filesystem tree and mounts

Linux has one directory tree starting at `/`. Disks, memory-backed filesystems (`tmpfs`), and special kernel filesystems (`proc`, `sysfs`, `cgroup2`) are **mounted** onto directories in that tree.

```sh
findmnt | head -30      # the mount tree
df -h                   # mounted filesystems with space
mount | grep -E ' /proc | /sys | /sys/fs/cgroup '
```

Special filesystems worth knowing:

- **`/proc`**: a window into the kernel's view of processes. `/proc/<pid>/status`, `/proc/<pid>/cmdline`, `/proc/<pid>/fd/`, `/proc/<pid>/ns/` (namespaces), `/proc/<pid>/cgroup`. Tools like `ps` just read `/proc`.
- **`/sys`**: devices and kernel objects.
- **`/sys/fs/cgroup`**: resource control ([module 3](../03-cgroups.md)).
- **`/dev`**: device files. `/dev/null` discards everything; `/dev/zero` returns zeros; `/dev/urandom` returns random bytes.

A **bind mount** makes a directory appear at a second place: `sudo mount --bind /src /dst`. Docker volumes are bind mounts.

## 7. Memory and CPU, briefly

- `free -m`: total, used, **buff/cache** (file cache the kernel will give back under pressure), available.
- The **OOM killer** kills a process when memory truly runs out, system-wide or inside a cgroup.
- `top`/`htop`: CPU per process. `load average` is the number of processes running or waiting for CPU (and uninterruptible I/O), averaged.
- `nproc`: number of CPUs.

## 8. Services and logs (systemd)

Servers run long-lived programs as **services** managed by systemd:

```sh
systemctl status docker
journalctl -u docker --since "10 min ago"
systemctl cat docker       # the unit file: how it is started, restarted, limited
```

Look at `Restart=` and `KillMode=` in unit files: they are a single-host restart policy, the same idea as the engine's restart policy in [module 8](../08-provisioning-engine.md).

## Lab F1

Do all of these, and write down each surprise:

1. Use `strace -f` on `sh -c 'echo hi | cat'` and find the `pipe`, `clone` and `execve` calls.
2. Start `sleep 1000` in the background, kill its parent shell with SIGKILL from another terminal, and find out who its new parent is (`ps -o ppid= -p <pid>`).
3. Write a 10-line shell script that ignores SIGTERM and prove that `kill` does nothing but `kill -9` works.
4. Create a zombie: `sh -c 'sleep 1 & exec sleep 100'`, then look for `Z` in `ps`. Why is it a zombie? (The `sleep 100` replaced the shell and never calls `wait`.)
5. Bind-mount a directory onto another and write a file through each path.
6. Read `/proc/self/status` for `cat` itself: find its PID, PPID, UID and memory usage.

## Resources

- William Shotts, [_The Linux Command Line_](https://linuxcommand.org/tlcl.php) (free book). Chapters 1 to 11 if the shell is new to you.
- Julia Evans' zines at [wizardzines.com](https://wizardzines.com/), especially _Bite Size Linux_, _Linux debugging tools_ and _How containers work_. Short, visual and accurate.
- Brian Ward, _How Linux Works_ (No Starch Press), chapters on processes, devices, the kernel and user space.
- Michael Kerrisk, _The Linux Programming Interface_: the reference. Read chapters 24 to 28 (processes), 20 to 22 (signals) and 44 (pipes) when you want depth.

## Check yourself

1. Describe what happens, in system calls, when your shell runs `ls`.
2. What does exit status 137 mean, and what usually causes it in a container?
3. What is a zombie process, and whose job is it to clean it up?
4. Why does `ps` inside a container show only the container's processes? (Answer after module 1.)
5. What is a file descriptor, and what happens to open fds across `fork` and `execve`?
