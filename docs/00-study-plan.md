# Study plan: from platform engineer to infrastructure engineer

You have run platforms: you have written Kubernetes manifests, debugged a CrashLoopBackOff, tuned resource limits, wired CI to a registry, and maybe run Terraform against a cloud. A platform infrastructure role like Railway's asks for the layer underneath: what `resources.limits.memory` actually writes to the kernel, what a "pod IP" physically is, why a deploy can drop connections, and how you would build the thing that runs everyone else's containers.

This plan gets you there in six weeks at roughly 10 to 12 hours a week. Every week has four parts:

- **Read/watch:** the primary sources, not summaries.
- **Lab:** commands to run, with Roundhouse and with raw Linux tools, so you see the primitive with and without the abstraction.
- **Code:** the Roundhouse file(s) to read line by line, and the production code they mirror.
- **Check:** questions you should be able to answer out loud, without notes. They are the interview.

You need a Linux machine where you are root: a cloud VM (any 2 vCPU / 4 GB instance with Ubuntu 24.04), a local VM, or WSL2. Containers-in-containers work if the outer one is privileged.

## The map: what you already know, and what it is underneath

| You know (platform view)          | It is (kernel/runtime view)                                                             | Where in Roundhouse                                       |
| --------------------------------- | --------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| A container                       | A process tree with its own namespaces, a cgroup, a pivoted root and fewer capabilities | `internal/runtime/runtime.go`, `init.go`                  |
| `kubectl exec`                    | `setns(2)` into the target's namespaces, then fork                                      | `internal/nsenter/nsenter.go`, `internal/runtime/exec.go` |
| `resources.limits.memory: 256Mi`  | `echo 268435456 > memory.max` in the pod's cgroup                                       | `internal/cgroups/cgroups.go`                             |
| `limits.cpu: 500m`                | `echo "50000 100000" > cpu.max` (CFS quota per period); throttling, not a speed cap     | `internal/cgroups/cgroups.go`                             |
| `OOMKilled`, exit code 137        | The kernel OOM killer sent SIGKILL (128+9) inside the memory cgroup                     | `internal/container/start.go` (`finish`)                  |
| An image layer                    | A tar file; unpacked, a directory stacked by overlayfs                                  | `internal/image/unpack.go`, `internal/rootfs/overlay.go`  |
| An image digest                   | sha256 of the manifest JSON, which lists sha256s of config and layers                   | `internal/image/types.go`, `store.go`                     |
| A pod IP                          | An address on one end of a veth pair, inside a network namespace                        | `internal/network/network.go`                             |
| `Service` / cluster DNS           | A resolver that maps a name to the healthy backends' IPs                                | `internal/engine/dns.go`                                  |
| `ports: 8080:80`, an Ingress      | A proxy (userland or iptables DNAT) from a host port to the container IP                | `internal/network/proxy.go`                               |
| A `Deployment` rollout            | A reconcile loop: create new, wait for readiness, move traffic, drain old               | `internal/engine/sync.go`                                 |
| `readinessProbe`                  | A prober whose results gate routing and promotion                                       | `internal/engine/health.go`                               |
| `restartPolicy`, CrashLoopBackOff | Exponential backoff computed from the observed exit and restart count                   | `internal/engine/sync.go` (`maintain`)                    |
| kubelet surviving a restart       | Supervisors (shims) own containers; the control plane only observes and acts            | `internal/container/start.go` (`ShimMain`)                |
| `docker build` / BuildKit         | A DAG of steps, each with a content-addressed cache key, executed in containers         | `internal/builder/builder.go`                             |
| A registry                        | An HTTP API over a content-addressed blob store                                         | `internal/registry/registry.go`                           |
| kube-scheduler                    | Filter nodes by hard constraints, score by soft preferences, pick the best              | `internal/scheduler/scheduler.go`                         |
| Billing per usage                 | Sampling cumulative cgroup CPU and memory counters                                      | `internal/engine/metering.go`                             |

Keep this table open. By week 6 you should be able to explain every row from memory, and point to the lines of code.

## Week 1: processes and namespaces

**Read/watch**

- Liz Rice, _Containers From Scratch_ (GOTO 2018): [video](https://www.youtube.com/watch?v=8fi7uSYlOdc), [code](https://github.com/lizrice/containers-from-scratch). Type it out yourself before reading Roundhouse.
- `man 7 namespaces`, `man 7 pid_namespaces`, `man 2 clone`, `man 2 setns`, `man 2 unshare` ([man7.org](https://man7.org/linux/man-pages/man7/namespaces.7.html)).
- Ivan Velichko, [_Learning Containers From The Bottom Up_](https://iximiuz.com/en/posts/container-learning-path/), and the hands-on playgrounds at [labs.iximiuz.com](https://labs.iximiuz.com/).
- [Module 1: Processes and namespaces](01-namespaces.md).

**Lab:** the exercises in module 1. Do them in both forms, `unshare`/`nsenter` and `rh`.

**Code:** `internal/runtime/runtime.go` (the parent), `internal/runtime/init.go` (the child), `internal/nsenter/nsenter.go`. Then runc's [`libcontainer/nsenter/nsexec.c`](https://github.com/opencontainers/runc/blob/main/libcontainer/nsenter/nsexec.c) and `libcontainer/process_linux.go`.

**Check**

1. Why does a Go container runtime re-execute `/proc/self/exe` instead of calling `fork()`?
2. `unshare --pid` then `ps`: why do you still see every host process? What fixes it?
3. Why does `docker stop` take 10 seconds for some images and not others?
4. Why must `rh exec` use a C constructor, and why does it fork after `setns`?
5. What is the difference between the namespaces a process is _in_ and the ones its _children_ will be in?

## Week 2: filesystems and images

**Read/watch**

- `man 2 pivot_root`, `man 7 mount_namespaces` (propagation: shared, private, slave).
- Kernel docs: [Overlay filesystem](https://docs.kernel.org/filesystems/overlayfs.html).
- [OCI image spec](https://github.com/opencontainers/image-spec): `manifest.md`, `config.md`, `layer.md` (whiteouts).
- [OCI distribution spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md): pull and push flows.
- [Module 2: Filesystems](02-filesystems.md) and [Module 6: Images](06-images.md).

**Lab:** pull an image with `rh pull`, then walk `RH_ROOT/images/` by hand: find the manifest by digest, read the config, match `diff_ids` to layer directories, find a whiteout. Use `curl` to perform the token dance against Docker Hub yourself (module 6).

**Code:** `internal/image/registry.go`, `store.go`, `unpack.go`, `internal/rootfs/overlay.go`, `internal/fsutil/securejoin.go`. Then containerd's overlayfs snapshotter (`plugins/snapshots/overlay/` in containerd 2.x) and [`cyphar/filepath-securejoin`](https://github.com/cyphar/filepath-securejoin).

**Check**

1. `chroot` vs `pivot_root`: how does a root process escape a chroot, and why can it not escape a pivot_root?
2. What exactly is an image ID? A digest? A diff ID? Which ones change if you recompress a layer?
3. Delete a file in a Dockerfile `RUN` step: what goes into the layer tar? What does it become on disk?
4. Why does starting a container from a 1 GB image take milliseconds with overlayfs?
5. A layer contains `etc -> /` and then `etc/passwd`. What happens with a naive unpacker?

## Week 3: cgroups, resources and metering

**Read/watch**

- Kernel docs: [Control Group v2](https://docs.kernel.org/admin-guide/cgroup-v2.html). Read "Controllers" for cpu, memory and pids.
- `man 7 cgroups`, `man 7 cgroup_namespaces`.
- Railway's pricing is per-second usage of CPU and memory of persistent containers; see [Railway docs: pricing](https://docs.railway.com/reference/pricing).
- [Module 3: cgroups](03-cgroups.md).

**Lab:** watch `memory.current` climb and the OOM kill happen; throttle a CPU loop to 0.2 cores and read `cpu.stat`'s `nr_throttled`; contain a fork bomb; run the daemon and watch `rh usage` accumulate.

**Code:** `internal/cgroups/cgroups.go`, `internal/engine/metering.go`. Then runc's cgroup manager ([opencontainers/cgroups](https://github.com/opencontainers/cgroups), `fs2/`).

**Check**

1. `cpu: 500m` on an 8-core node: can the process ever use two cores for a moment? What does it pay for that?
2. Why does the runtime disable swap (`memory.swap.max = 0`) when setting a memory limit?
3. What does `CLONE_INTO_CGROUP` fix?
4. How would you bill a customer per second for CPU and memory without double counting after a daemon restart?
5. Why does killing a container's init not necessarily kill everything, and how does `cgroup.kill` fix that?

## Week 4: security and networking

**Read/watch**

- `man 7 capabilities` (read "Transformation of capabilities during execve"), `man 2 prctl` (`PR_SET_NO_NEW_PRIVS`), `man 2 seccomp`.
- `man 4 veth`, `man 8 ip-netns`, and an iptables NAT tutorial (the `nat` table's `POSTROUTING`/`MASQUERADE`).
- Railway: [How private networking works](https://docs.railway.com/networking/private-networking/how-it-works) (a WireGuard mesh with eBPF-based NAT and DNS interception).
- [Module 4: Security](04-security.md) and [Module 5: Networking](05-networking.md).

**Lab:** build a two-namespace network by hand with `ip netns`/`ip link`/`iptables`, then compare with what `rh run` creates. Resolve `web.rh.internal` from a container while you roll the service.

**Code:** `internal/runtime/caps.go`, `mounts.go` (`maskPaths`), `internal/network/network.go`, `proxy.go`, `internal/engine/dns.go`.

**Check**

1. Root in a container without `CAP_SYS_ADMIN`: what can it still do that should worry you? What would user namespaces change?
2. Trace a packet from a container to the internet and back: which interfaces, which NAT rule, which conntrack entry?
3. Userland proxy vs iptables DNAT for port publishing: trade-offs?
4. Why does Roundhouse continue IP allocation after the last address instead of reusing the lowest free one?
5. How could a platform give every service a stable private DNS name while its instances churn on every deploy?

## Week 5: builds and the provisioning engine

**Read/watch**

- BuildKit: [README](https://github.com/moby/buildkit), the [solver design doc](https://github.com/moby/buildkit/blob/master/docs/dev/solver.md), and `frontend/dockerfile/dockerfile2llb/convert.go`.
- Railway engineering blog: [_Counting to 3 with a new builder processing 50M+ monthly builds_](https://blog.railway.com/p/new-builder-scale-big) (BuildKit in microVM build cells on bare metal, scheduled "1-of-3 on the ring" to keep caches warm).
- Kubernetes: [controller pattern](https://kubernetes.io/docs/concepts/architecture/controller/), and client-go's [workqueue](https://pkg.go.dev/k8s.io/client-go/util/workqueue).
- [Module 7: Builds](07-builds.md), [Module 8: The provisioning engine](08-provisioning-engine.md).

**Lab:** build `examples/hello` cold and warm; change one file and predict which steps rebuild before you run it. Run the daemon, deploy, break a deploy three different ways (crash on boot, failing health check, bad image) and read the event feed for each.

**Code:** `internal/builder/builder.go`, `internal/engine/sync.go`, `internal/engine/queue.go`, `internal/engine/engine_test.go`.

**Check**

1. What goes into a build step's cache key? Why not the file mtimes?
2. Why is "the previous deployment keeps serving if the new one fails" a property of the promotion order?
3. The daemon restarts in the middle of a rollout. Walk through what happens.
4. Level-triggered vs edge-triggered reconciliation: which one survives lost events, and why?
5. How does your engine avoid two workers reconciling the same service at the same time?

## Week 6: scale, design practice and the interview

**Read/watch**

- [Module 9: Orchestration at scale](09-scale.md) and [Module 10: Interview prep](10-interview-prep.md).
- Borg, Omega and Kubernetes (ACM Queue, 2016): [paper](https://queue.acm.org/detail.cfm?id=2898444).
- Firecracker (NSDI 2020): [paper](https://www.usenix.org/conference/nsdi20/presentation/agache), for when containers are not enough isolation.
- Railway's engineering blog index: [blog.railway.com/engineering](https://blog.railway.com/engineering). Read what they say about orchestration, bare metal, and when raw Kubernetes primitives help.

**Lab:** do two exercises from [module 12](12-exercises.md), one runtime-level (seccomp or user namespaces) and one control-plane-level (multi-node controller or autoscaling). Deploy [Capital Lab on Roundhouse](11-capital-lab.md).

**Practice:** do the mock design interview in module 10 out loud, timed, twice. Record yourself.

## How to know you are ready

- You can draw the diagram in the [README](../README.md) from memory and explain each arrow.
- You can explain, with the syscalls, what happens between `rh run` and the first line of your program's output.
- You can predict the output of the integration tests before running them, and explain each assertion.
- You have extended Roundhouse with at least two of the exercises, with tests.
- You can talk for 45 minutes about designing a container provisioning engine, and when interrupted with "and what if the node dies?", you already have an answer.
