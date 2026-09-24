# Module 4: Security

> **Goal:** be able to say precisely what stands between a malicious process in a container and the host, what Roundhouse does, and what it does not do yet.

A multi-tenant platform runs code written by strangers next to code written by other strangers. Every layer below either reduces what that code can do or reduces the damage when a layer fails. Interviewers for platform infrastructure roles will probe this, so be precise.

## The layers, in order of what Roundhouse applies

| Layer                    | Mechanism                                       | Roundhouse                                | Where                          |
| ------------------------ | ----------------------------------------------- | ----------------------------------------- | ------------------------------ |
| Visibility               | namespaces                                      | pid, mnt, uts, ipc, net, cgroup           | `runtime.go`, `init.go`        |
| Filesystem               | `pivot_root`, fresh `/dev`, masked `/proc`      | yes                                       | `mounts.go`                    |
| Resource exhaustion      | cgroups (memory, cpu, pids)                     | yes                                       | `cgroups.go`                   |
| Privilege                | capability bounding/effective sets              | Docker's default 14 capabilities          | `caps.go`, `init.go`           |
| Privilege escalation     | `PR_SET_NO_NEW_PRIVS`                           | on for every container and exec           | `init.go` (`applyCredentials`) |
| Identity                 | run as non-root (`USER`)                        | honored, resolved from the image's passwd | `container/etc.go`             |
| Syscall surface          | seccomp-bpf                                     | **no**: exercise                          | —                              |
| UID mapping              | user namespaces                                 | **no**: exercise                          | —                              |
| Mandatory access control | AppArmor / SELinux                              | **no**                                    | —                              |
| Kernel boundary          | microVM (Firecracker, Cloud Hypervisor), gVisor | **no**: see [module 9](09-scale.md)       | —                              |

## Capabilities

Traditional Unix has two kinds of process: root (UID 0, bypasses all permission checks) and everyone else. Linux splits root's power into ~41 **capabilities** (`man 7 capabilities`): `CAP_NET_ADMIN` configures networking, `CAP_SYS_ADMIN` does… almost everything else (mount, most namespace operations, many ioctls), `CAP_SYS_PTRACE` inspects other processes, and so on.

Each thread has several capability sets. The two that matter most:

- **Bounding set:** the ceiling. No `execve` can ever grant a capability outside it. Dropping from it is permanent for the process tree.
- **Effective set:** what the kernel actually checks right now.

For a root process with no file capabilities, after `execve` the effective set is essentially the bounding set. So **the bounding set is the real control for root-in-a-container**, and it is what Roundhouse shrinks first ([`dropBounding`](../internal/runtime/caps.go)), keeping Docker's default list:

```text
CHOWN DAC_OVERRIDE FSETID FOWNER MKNOD NET_RAW SETGID SETUID SETFCAP SETPCAP
NET_BIND_SERVICE SYS_CHROOT KILL AUDIT_WRITE
```

`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `CAP_SYS_MODULE`, `CAP_SYS_PTRACE`, `CAP_SYS_TIME` and the rest are gone. Run the lab below and decode `CapEff` with `capsh --decode`.

The order in [`applyCredentials`](../internal/runtime/init.go) matters and is a common interview follow-up:

1. Drop the bounding set (needs `CAP_SETPCAP`, so it happens while still privileged).
2. `PR_SET_KEEPCAPS=1`, so the next step does not wipe the permitted set.
3. `setgroups`, `setresgid`, `setresuid`. Group first: after dropping to a non-root UID you could no longer change groups.
4. `PR_SET_KEEPCAPS=0`, then `capset` the effective/permitted/inheritable sets to exactly the kept list.
5. `PR_SET_NO_NEW_PRIVS=1`.

For a non-root user, the kernel clears the effective set on `execve` (no ambient capabilities are set), which is why the integration test sees `CapEff: 0000000000000000` for `-u 65534`.

## no_new_privs

`PR_SET_NO_NEW_PRIVS` makes `execve` unable to grant privileges: setuid binaries (`sudo`, `ping` on old systems) and file capabilities stop working. It is inherited and irreversible. It is also a prerequisite for unprivileged processes to install seccomp filters. Roundhouse sets it for every container and every exec. The cost is that images which rely on `sudo` inside the container break, which is usually what you want.

## What root in a Roundhouse container can still do

Be able to list these honestly:

- **Everything a UID-0 process may do to files it can see**, including volumes mounted into it. Volume data is exposed to the container's root.
- **Raw sockets** (`CAP_NET_RAW`, kept for `ping`): craft packets, ARP-spoof other containers on the same bridge. Docker keeps it too. Many platforms drop it.
- **The whole syscall surface.** Without seccomp, the container can call `keyctl`, `bpf` (without `CAP_BPF` most operations fail, but the attack surface is reachable), `userfaultfd`, old and rarely audited socket families… Kernel exploits come through syscalls. Docker's default seccomp profile blocks ~50 of ~350.
- **It is UID 0 on the host.** If anything else goes wrong (a kernel bug, a runtime bug like CVE-2019-5736, the runc `/proc/self/exe` overwrite), the attacker is real root. User namespaces fix exactly this: root in the container maps to an unprivileged UID (e.g. 100000) on the host.

## CVE-2019-5736: the runtime's own binary

A malicious container could open `/proc/<runc-pid>/exe` while runc was executing inside it and overwrite the **runc binary on the host**. The next `docker exec` ran attacker code as host root. runc's fix was to re-exec from a sealed memfd copy of itself (a "cloned binary"), later replaced by other hardening. Roundhouse's `rh exec` uses the same `/proc/self/exe` technique and has **not** implemented that defence. That is a good talking point about why runtime code is security-critical, and a hard exercise.

## Lab 4

```sh
A=mirror.gcr.io/library/alpine:3.20
sudo -E rh run --rm $A grep Cap /proc/self/status
capsh --decode=00000000a80425fb 2>/dev/null || echo "install libcap2-bin to decode"

# CAP_SYS_ADMIN is gone: mounts fail even as root
sudo -E rh run --rm $A sh -c 'mount -t tmpfs none /mnt && echo mounted || echo "mount denied"'
# CAP_NET_ADMIN is gone: cannot change the container's own network
sudo -E rh run --rm $A sh -c 'ip link set lo down && echo changed || echo "denied"'
# Non-root user: nothing effective
sudo -E rh run --rm -u nobody $A grep CapEff /proc/self/status
# Masked and read-only /proc paths (which files exist varies by kernel config)
sudo -E rh run --rm $A sh -c 'wc -c /proc/keys /proc/timer_list; echo x > /proc/sys/kernel/hostname'
#   → 0 bytes each (bound over /dev/null), then "Read-only file system"
```

## Check yourself

1. Why is the bounding set, not the effective set, the control that matters for root in a container?
2. Why must `setgid` come before `setuid`, and why must bounding-set drops come before both?
3. Name three things root in a Roundhouse container can still do that a hardened platform would prevent, and the mechanism that would prevent each.
4. What does a user namespace change about the consequences of a container escape?
5. When are containers not enough, and what do platforms use instead? (Module 9.)
