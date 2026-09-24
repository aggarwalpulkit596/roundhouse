# Module 10: Interview prep

> **Goal:** turn what you built and read into answers you can give under time pressure, and a design interview you have rehearsed.

The brief you are preparing for: a senior infrastructure role on a platform team, a technical interview built around **architecting a container provisioning engine**, and a culture that values ownership and people who build things. Your advantages: you have been a user of platforms (CI/CD, Docker Compose), so you know what developers expect from one, and after this curriculum you have a working engine you studied and extended, which you can reason about line by line. Say so plainly: interviewers value someone who went deep fast over someone who claims experience they cannot explain.

## How to use this module

1. Answer each question below **out loud**, in under two minutes, before reading the model answer.
2. Where a model answer cites a Roundhouse file, open it and find the lines. Interviewers love "here is how I implemented that".
3. Do the mock design interview (section 3) twice, timed. Record yourself the second time.
4. Prepare your stories (section 5).

## 1. Question bank with model answers

### Runtime and kernel

**What is a container?**
A process (tree) with its own namespaces (pid, mount, uts, ipc, net, cgroup, optionally user), placed in a cgroup for limits and accounting, given its own root filesystem via `pivot_root` (usually an overlayfs of image layers), and restricted with dropped capabilities, `no_new_privs`, seccomp and an LSM profile. The kernel has no container object; a runtime composes these primitives. (`internal/runtime`)

**Walk me through `docker run nginx` down to syscalls.**
CLI → daemon → containerd pulls and unpacks layers (snapshotter prepares an overlay) → creates an OCI bundle → shim → runc `create`: runc re-executes itself (`/proc/self/exe init`) with `clone(CLONE_NEW*)`, the child blocks on a pipe, the parent sets up cgroups and waits for networking (CNI moves a veth into the netns), then the child mounts `/proc`, `/dev`, `/sys`, pivots root, masks paths, drops capabilities, applies seccomp, `setuid`s and `execve`s nginx. The shim holds stdio and reaps the exit. Roundhouse does the same sequence in `runtime.Start`/`Init`.

**Why does a Go runtime re-exec itself instead of forking?**
Go is multi-threaded; `fork()` copies only the calling thread, so locks held by other threads (GC, scheduler) stay locked in the child forever. Go only supports fork+exec. Exec'ing `/proc/self/exe` with a marker gives a fresh single-purpose process. `setns(CLONE_NEWNS)` additionally requires a single-threaded caller, hence runc's C `nsexec` constructor (Roundhouse: `internal/nsenter`).

**Why does `docker stop` sometimes take 10 seconds?**
PID 1 in a PID namespace gets no default signal handlers: SIGTERM is ignored unless the program installs a handler. Shell-wrapped apps and many runtimes don't. After the grace period the runtime sends SIGKILL. Fixes: a real init (`--init`/tini), `exec` in entrypoint scripts, handling SIGTERM in the app.

**chroot vs pivot_root?**
chroot changes one process's root directory and is escapable by a root process (chroot deeper, then `..` up from the old cwd). pivot_root swaps the root mount of the mount namespace; after unmounting the old root, the host filesystem is not reachable at all.

**How do memory limits work and what is exit code 137?**
`memory.max` caps charged memory (anon + page cache). At the limit the kernel reclaims; if it can't, the cgroup OOM killer SIGKILLs a process: 128 + 9 = 137. Disable swap for the cgroup or the limit leaks.

**Why can a CPU limit hurt latency?**
`cpu.max` is a quota per period (e.g. 50 ms per 100 ms). Multi-threaded apps burn the quota early in the period and are throttled for the rest, adding up to tens of ms of latency even at low average CPU. Watch `nr_throttled`. Use weights (requests) for sharing, and limits sparingly.

**How do you stop a fork bomb?**
`pids.max` on the container's cgroup. `fork()` returns EAGAIN in the container; the host is unaffected.

**How do you kill a container reliably?**
Kill the cgroup, not PID 1: `cgroup.kill` on v2, or freeze → SIGKILL every task → thaw on v1. Otherwise daemonized grandchildren survive. (`cgroups.Kill`)

**What does user-namespacing buy you?**
Root in the container maps to an unprivileged UID on the host, so an escape lands as nobody instead of root. It also makes capabilities namespaced. Costs: file ownership in images needs shifting (idmapped mounts), some kernel features are restricted.

**What was CVE-2019-5736?**
A container process could open the runc binary via `/proc/<runc-pid>/exe` during `exec` and overwrite it on the host, gaining host root on the next invocation. Fixed by re-executing runc from a sealed memfd copy. The lesson: the runtime itself runs in the attacker's reach during setup.

### Images, builds and transfer

**Image ID vs digest vs diff ID?**
Image ID = digest of the config JSON. Manifest digest = digest of the manifest JSON (what `@sha256:` pins). Diff ID = digest of an uncompressed layer tar; the layer blob digest is of the compressed bytes. Recompressing changes manifest and blob digests, not the image ID or diff IDs.

**How does a registry push avoid re-uploading layers?**
`HEAD /v2/<name>/blobs/<digest>` per blob; skip the ones that exist. Cross-repo mount links blobs across repositories without upload. The manifest PUT fails if any referenced blob is missing. (`Store.Push`, `internal/registry`)

**How does BuildKit's cache work?**
The Dockerfile compiles to a DAG. Each vertex's cache key = hash(operation definition, input keys, and for file inputs a content checksum). Hits are reused regardless of which build produced them; independent vertices run in parallel; unreachable stages are pruned. File checksums ignore mtimes so fresh clones hit. (`internal/builder`)

**How do you make builds reproducible?**
Sorted tar entries, clamped mtimes (`SOURCE_DATE_EPOCH`), no uid/gid names, deterministic gzip headers, pinned base image digests, and hermetic steps (no network, fixed timestamps in tools). Test by building twice and comparing digests.

**Deploys are slow because pulls are slow. What do you do?**
In order of effort: smaller images (multi-stage, distroless), pull-through cache per region, zstd, pre-pull on likely hosts, place on hosts that have the layers, lazy pulling (eStargz/SOCI), P2P distribution.

### Control plane

**Design zero-downtime deploys.**
Start new replicas alongside old; gate on readiness; atomically switch routing; drain old for a window; SIGTERM, grace, SIGKILL. Failure before promotion leaves the old version serving. Order is everything. (`rollout` in `internal/engine/sync.go`)

**Level- vs edge-triggered reconciliation?**
Edge: react to events, and a lost event means permanent drift. Level: repeatedly compare desired with observed and act on the difference; events only speed things up. Level-triggered controllers survive crashes, restarts and dropped messages.

**Where does the control plane get observed state?**
From the nodes, not from its own database: list containers by label (Roundhouse) or kubelet status reports. The database stores intent only, so it cannot disagree with reality about what is running.

**The control plane crashes mid-deploy. What happens?**
Nothing is lost: containers belong to per-container supervisors (shims), desired state is durable, and on restart the reconciler observes what exists, adopts it, and continues. Tested in `TestEngineRestartAdoptsRunningContainers` and live.

**How do you prevent two workers reconciling the same service?**
A work queue with per-key exclusivity: a key being processed is not handed out again; adds during processing mark it dirty and re-queue it afterwards. (`queue.go`) Across control-plane replicas: leader election or per-service leases with fencing.

**Readiness vs liveness?**
Readiness gates traffic and promotion. Liveness restarts. Liveness checks that touch shared dependencies cause fleet-wide restart storms when the dependency fails. Default to readiness, and make liveness shallow (is the process responsive at all?).

**Restart policy design?**
Exponential backoff (1s → 30s cap), a crash budget that only counts quick deaths, and a terminal CRASHED state that keeps the last container and logs for debugging. Never hot-loop.

**How do you bill per second?**
Sample cumulative cgroup CPU counters (deltas, robust to missed samples) and memory gauges on an interval; key by container; handle resets; idempotent ingestion keyed by (container, sample time); after agent restart, re-prime instead of double counting. (`metering.go`)

### Networking

**How does a container reach the internet?**
Its `eth0` is one end of a veth; the other end is on a host bridge; default route via the bridge IP; the host forwards and MASQUERADEs the source to its own address; conntrack remembers the flow to reverse-translate replies.

**How do services find each other?**
Private DNS (`<svc>.rh.internal`) answering with healthy instances' IPs, short TTL; or a virtual IP per service with kernel load balancing; or client-side load balancing from a control-plane API. Mention DNS caching problems.

**Multi-host container networking?**
A subnet per host plus an overlay (VXLAN, WireGuard) or native routing (BGP); a control plane distributes endpoint and route information; policy enforced at the host (iptables/eBPF). Railway: WireGuard mesh with eBPF NAT and IPv6.

### Scale and judgement

**Kubernetes for a PaaS?** See module 9's two-sided argument. Have a crisp opinion and its failure modes.

**When are containers not enough isolation?** Untrusted code from many tenants on shared hosts, especially builds. Use microVMs (Firecracker) or gVisor, plus seccomp/userns on the container path.

**What would you build first on joining?** Ask about the biggest source of user pain (deploy time? reliability?) and measure before proposing. It is a judgement question; show how you decide.

## 2. Reading guide: production code, mapped to what you wrote

Paths are as of runc 1.2, containerd 2.x and BuildKit 0.2x; repositories reorganize, so search by file name if a path moved.

**runc** ([opencontainers/runc](https://github.com/opencontainers/runc))

| Read                                                                                      | Compare with                            |
| ----------------------------------------------------------------------------------------- | --------------------------------------- |
| `libcontainer/nsenter/nsexec.c`                                                           | `internal/nsenter/nsenter.go`           |
| `libcontainer/process_linux.go` (`initProcess.start`)                                     | `internal/runtime/runtime.go` (`Start`) |
| `libcontainer/standard_init_linux.go`                                                     | `internal/runtime/init.go` (`prepare`)  |
| `libcontainer/rootfs_linux.go` (`pivotRoot`, `maskPath`)                                  | `internal/runtime/mounts.go`            |
| `libcontainer/capabilities/`                                                              | `internal/runtime/caps.go`              |
| cgroups (now [opencontainers/cgroups](https://github.com/opencontainers/cgroups)): `fs2/` | `internal/cgroups/cgroups.go`           |

**containerd** ([containerd/containerd](https://github.com/containerd/containerd))

| Read                                                                              | Compare with                                    |
| --------------------------------------------------------------------------------- | ----------------------------------------------- |
| `core/runtime/v2/README.md` (the shim API)                                        | `internal/container/start.go` (`ShimMain`)      |
| `cmd/containerd-shim-runc-v2/`                                                    | same                                            |
| `plugins/snapshots/overlay/`                                                      | `internal/rootfs/overlay.go`, `image/unpack.go` |
| `core/content/` (content store) and `core/remotes/docker/` (resolver, token auth) | `internal/image/store.go`, `registry.go`        |
| `core/unpack/`                                                                    | `Store.Pull` (parallel fetch and unpack)        |

**BuildKit** ([moby/buildkit](https://github.com/moby/buildkit))

| Read                                            | Compare with                                  |
| ----------------------------------------------- | --------------------------------------------- |
| `docs/dev/solver.md`                            | module 7                                      |
| `frontend/dockerfile/dockerfile2llb/convert.go` | `internal/builder/parse.go`, `builder.go`     |
| `solver/` (cache keys, `edge.go`, `jobs.go`)    | `hashKey`, `cacheLookup`, stage goroutines    |
| `cache/contenthash/` (file checksums for COPY)  | `internal/builder/context.go` (`hashSources`) |

**Kubernetes** ([kubernetes/kubernetes](https://github.com/kubernetes/kubernetes)): `pkg/controller/deployment/rolling.go` (compare with `rollout`), `pkg/kubelet/pleg/` (how the kubelet observes containers), and client-go's `util/workqueue` (compare with `queue.go`).

## 3. Mock design interview (45 minutes)

Have a friend read the prompt and interrupt with the follow-ups. Or record yourself and play both parts.

> **Prompt:** "Design the container provisioning engine for a platform like ours. Users push code, we build it, and we must run it: start it, deploy new versions, keep it healthy, and bill for usage. Start with one region."

Checkpoints and follow-ups:

- **(0–5)** Did you ask about workloads, statefulness, scale, SLOs and tenancy before drawing? _Follow-up: "Assume 10,000 hosts and 1 million running containers."_
- **(5–15)** Data model and API. Immutable deployments? Idempotent PUT? _"How does rollback work?" "What does the user see while a deploy runs?"_
- **(15–25)** Control loop. Desired vs observed state, level-triggering, the work queue, the node agent and its supervisor. _"The agent crashes during a deploy." "The control plane loses its database for 5 minutes." "A node is partitioned for 10 minutes and then comes back."_
- **(25–35)** Data path. Zero-downtime deploy ordering, health checks, the edge, service discovery, image distribution to 10,000 hosts. _"A popular base image is updated and 50,000 containers redeploy at once." "Deploys take 90 seconds, 70 of them pulling images: fix it."_
- **(35–42)** Isolation, noisy neighbours, billing. _"A tenant's container fork-bombs." "A tenant is mining crypto." "How do you bill per second?"_
- **(42–45)** What you would build first, and what you would measure.

Score yourself: did you state trade-offs rather than solutions, name failure modes before being asked, and reference things you have actually built?

## 4. The "tell me about something you built" answer

A two-minute version you can adapt:

> I wanted to understand the layer under the platforms I had been building on, so I built one from the kernel up: Roundhouse, a container engine and deploy platform in Go with no Docker or runc underneath. It pulls OCI images and verifies every digest; runs containers with namespaces, pivot_root, cgroups v1 and v2, and dropped capabilities; networks them over a bridge with its own IPAM, NAT and private DNS; builds images with a BuildKit-style cached, parallel multi-stage builder; and deploys services with health-gated rolling updates. The part I am proudest of is the control loop: it is level-triggered and stores only desired state, rediscovering containers by label, so you can kill it mid-deploy and it picks up where it left off. There is a test for that, and an integration test that runs a rollout under load and asserts zero failed requests. Along the way I hit real kernel constraints, like not being able to create threads after joining a PID namespace, which is why runc's nsexec forks the way it does.

Adjust it to what you actually extended (module 12). Be ready to open any file and explain it.

## 5. Stories to prepare (behavioural)

For each, one concrete situation from your own experience (CI/CD, development, or this curriculum itself), told as situation → what you did → result → what you learned:

- An outage you owned end to end, and what you changed so it could not recur.
- A time you simplified a system instead of adding to it.
- A time you shipped something small and fast versus waiting for the complete version.
- A disagreement about a technical direction, and how it resolved.
- A time you went down a layer (debugged the kernel, the network, the runtime) to fix a problem.
- Something you taught yourself quickly because the work needed it. (This curriculum is a fine answer.)

## 6. Final checklist

- [ ] I can draw the architecture from the [README](../README.md) from memory.
- [ ] I can explain every row of the table in the [study plan](00-study-plan.md).
- [ ] I have run every lab in modules 1–8.
- [ ] I have read the runc and BuildKit files in section 2 and can compare them with Roundhouse.
- [ ] I have extended Roundhouse with at least two exercises, with tests.
- [ ] I have done the mock interview twice, and fixed what I stumbled on.
- [ ] I have read Railway's engineering blog posts on builds, networking and orchestration, and can say what I would ask them about each.
