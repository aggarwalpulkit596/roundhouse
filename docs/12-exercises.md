# Module 12: Exercises

Extending a system teaches more than reading it. Each exercise names the files to change, what "done" means (a test), and what you will learn. They are ordered roughly by difficulty. Do at least one from each tier before an interview.

## Tier 1: an afternoon each

**1. `rh top`.** Show the processes inside a container with host and container PIDs side by side. _Files:_ `cmd/rh/local.go`, `cgroups.Procs`. _Learn:_ `/proc/<pid>/status` has an `NSpid:` line with a process's PID in every nested namespace.

**2. `rh volume ls/rm`.** List named volumes with size and which services use them; refuse to remove one in use. _Files:_ new `cmd/rh/volume.go`. _Test:_ unit test with a temp `RH_ROOT`.

**3. Memory high-water mark.** Record `memory.peak` (v2) / `memory.max_usage_in_bytes` (v1) in the container's exit state and show it in `rh ps -a`. _Learn:_ why peak beats sampled current for right-sizing.

**4. `exec` health checks.** Add `"type": "exec", "command": [...]` to `Healthcheck`, run it with `Manager.Exec`, and treat exit 0 as healthy. Use it for `pg_isready`. _Files:_ `engine/types.go`, `engine/health.go`, `engine/runtime.go`.

**5. Graceful proxy draining.** Instead of a fixed `drainSeconds`, retire old instances as soon as the edge reports zero active connections to them (with the timer as a cap). _Files:_ `network/proxy.go` (per-backend counters), `engine/sync.go`.

## Tier 2: a weekend each

**6. seccomp.** Install a default-deny BPF filter allowing the ~300 syscalls in Docker's default profile. Build it with `golang.org/x/net/bpf` or hand-assembled `sock_filter`s, install with `prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER)` after `no_new_privs`, right before exec. _Test:_ an integration test where `unshare` (or `keyctl`) fails with `EPERM`. _Learn:_ BPF, the syscall ABI, why the filter must be the last thing before `execve`.

**7. zstd layers.** Accept `application/vnd.oci.image.layer.v1.tar+zstd` in `image/unpack.go` (`github.com/klauspost/compress/zstd`) and add `rh build --compression zstd`. _Measure:_ pull and unpack time for a large image, gzip vs zstd.

**8. Image garbage collection.** Mark every blob and layer reachable from `images.json`, running containers and the build cache; sweep the rest. Take the image-store lock for the mark phase so a concurrent pull cannot lose a blob. _Learn:_ why containerd uses leases.

**9. Autoscaling.** Add `minReplicas`/`maxReplicas` and scale on CPU usage from the meter (or on cgroup v2 PSI `cpu.pressure`, which measures _stalled_ time and is a better signal). Add hysteresis and a cooldown. _Test:_ a fake runtime whose stats you control.

**10. Surge and unavailability budgets.** Add `maxSurge` and `maxUnavailable` so a 20-replica service rolls a few replicas at a time instead of doubling. _Files:_ `engine/sync.go` (`rollout`), tests in `engine_test.go`.

**11. Cron jobs.** Add one-shot and scheduled jobs: `restart: never`, completion tracking, history, no routing. _Learn:_ how "job" and "service" differ in a control loop.

## Tier 3: a week or more each

**12. User namespaces (rootless containers).** Map container UID 0 to an unprivileged range. You will need the parent to write `uid_map`/`gid_map` between clone and release (another reason runc's nsexec has stages), idmapped mounts (`mount_setattr` with `MOUNT_ATTR_IDMAP`) or chowned layer copies, and to rethink cgroup and network setup (slirp4netns or pasta for unprivileged networking). _Learn:_ everything about why rootless is hard.

**13. A multi-node control plane.** Add `rh controller`: it accepts service specs, uses `scheduler.Place` against the agents' `/v1/node`, and pushes per-node specs (with each node's share of replicas) to the agents. Add heartbeats, leases and a rescheduling loop for dead nodes. Run two agents on one machine with different bridges (`RH_BRIDGE`, `RH_SUBNET`) and state roots. _Learn:_ module 9 in practice: fencing, partial failure, where state lives.

**14. Cross-host networking.** Connect two Roundhouse nodes' bridges with a WireGuard tunnel (or VXLAN), give each node its own /24, and route between them. Then make private DNS answer with instances on both nodes. _Learn:_ overlays, MTU, why Railway uses WireGuard.

**15. A microVM runtime.** Implement `engine.Runtime` with [Firecracker](https://github.com/firecracker-microvm/firecracker): turn image layers into a root block device (or use virtio-fs), boot a minimal kernel with an init that execs the image's entrypoint, and connect a tap device to the bridge. The engine should not change at all. _Learn:_ what containers give you for free, and the isolation that VMs buy.

**16. Checkpoint and restore.** Use CRIU to checkpoint a running container and restore it (on the same host first, then another). _Learn:_ the building block behind live migration and fast "scale from zero".

**17. Harden `exec` against CVE-2019-5736.** Re-execute from a sealed `memfd` copy of the binary (`memfd_create` + `F_SEAL_*`) so a container can never write to the host's `rh` binary through `/proc/<pid>/exe`. _Learn:_ how runc fixed it, and why the fix later changed.

**18. eBPF metering.** Replace cgroup polling with an eBPF program (via `cilium/ebpf`) that accounts CPU per cgroup on scheduler events, or network bytes per container. _Learn:_ where platforms are moving for observability and networking.

## How to submit your own work

Treat each exercise like a real change: a failing test first, then the code, then run `go test -race ./...` and the integration suite as root. Keep the comment style: explain _why_ each primitive is there. When you finish one, add a row to the tables in the README and the study plan so the map stays true.
