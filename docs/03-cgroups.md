# Module 3: cgroups

> **Goal:** know what `resources.limits` turns into, how the kernel enforces it, how to read what a container actually used, and how a usage-billed platform turns that into money.

Namespaces decide what a process can **see**. Control groups decide how much it can **use**, and they are also how you **measure** what it used. For a platform that bills per second of CPU and memory, as Railway does, cgroups are the cash register.

## v1 vs v2

|                      | cgroup v1 (legacy)                                      | cgroup v2 (unified)                                      |
| -------------------- | ------------------------------------------------------- | -------------------------------------------------------- |
| Hierarchies          | one per controller: `/sys/fs/cgroup/memory/…`, `/cpu/…` | one tree: `/sys/fs/cgroup/…`                             |
| A process is in      | one cgroup _per controller_ (can differ)                | exactly one cgroup                                       |
| Enabling controllers | mount the controller                                    | write `+memory` to the parent's `cgroup.subtree_control` |
| Memory limit         | `memory.limit_in_bytes`                                 | `memory.max` (plus `memory.high` for soft throttling)    |
| CPU limit            | `cpu.cfs_quota_us` + `cpu.cfs_period_us`                | `cpu.max` = `"<quota> <period>"`                         |
| OOM count            | `memory.oom_control` (`oom_kill`)                       | `memory.events` (`oom_kill`)                             |
| Kill everything      | freeze, iterate `cgroup.procs`, SIGKILL, thaw           | `echo 1 > cgroup.kill` (5.14+)                           |
| Pressure (PSI)       | no                                                      | `cpu.pressure`, `memory.pressure`, `io.pressure`         |

Every current mainstream distribution defaults to v2, and Kubernetes has supported it as GA since 1.25. You will still meet v1 (and "hybrid": v1 controllers with an empty v2 mounted at `/sys/fs/cgroup/unified`) on older hosts and in some sandboxes. Roundhouse was in fact developed on a hybrid host, and CI runs it on v2. [`internal/cgroups`](../internal/cgroups/cgroups.go) hides both behind one interface, as runc does (`fs` and `fs2` managers).

## What each limit really does

### Memory

`memory.max = 268435456` is a hard limit on the cgroup's charged memory: anonymous memory (heap, stacks) **and page cache** from files it reads and writes. When a charge would exceed the limit, the kernel first reclaims (drops clean page cache, and swaps if allowed), and only if that fails does it invoke the **OOM killer inside that cgroup**, which picks a victim (usually the biggest process) and sends `SIGKILL`. That is where exit code **137** (128 + 9) and `OOMKilled` come from.

Roundhouse also writes `memory.swap.max = 0`. Without it, a "256 MB" container on a host with swap can use far more memory, slowly, and the limit stops meaning what users think it means.

Page cache counting surprises people: a container that reads a 1 GB file shows 1 GB+ of usage even though its heap is tiny. That memory is reclaimable, which is why `memory.current` alone is a poor OOM predictor, and why Kubernetes evicts on the _working set_ (`memory.current` minus inactive file pages from `memory.stat`).

### CPU

`cpu.max = "50000 100000"` means: in every 100 ms period, this cgroup may run for 50 ms of CPU time **in total, across all cores**. It is a _quota_, not a speed:

- A single-threaded process never notices. It runs at full speed for 50 ms, then is throttled for 50 ms.
- A 4-thread process burns the 50 ms in 12.5 ms of wall time, then sleeps for 87.5 ms. Latency spikes, while average CPU looks low. This is the classic "we set a CPU limit and p99 got worse" story. Watch `nr_throttled` and `throttled_usec` in `cpu.stat`.
- `cpu.weight` (v2) / `cpu.shares` (v1) is the alternative: relative priority only under contention, no hard cap. Kubernetes maps _requests_ to weights and _limits_ to `cpu.max`.

### PIDs

`pids.max` caps the number of tasks (processes and threads) in the cgroup. It is the only thing that stops a fork bomb from taking down the node: `fork()` starts failing with `EAGAIN` inside the container while the host is unaffected.

## Getting a process into a cgroup without a race

The obvious sequence (start the process, then write its PID to `cgroup.procs`) leaves a window where the process runs unconstrained and can fork children who are not moved. Two fixes:

1. **Block the child until it is placed.** Roundhouse's init blocks on the sync pipe until the parent has written its PID (the v1 path).
2. **`CLONE_INTO_CGROUP`** (Linux 5.7, cgroup v2): `clone3()` takes a cgroup directory file descriptor and the child is _born_ in the cgroup. Go exposes it as `SysProcAttr.UseCgroupFD` / `CgroupFD`, and Roundhouse uses it on v2 hosts ([`runtime.Start`](../internal/runtime/runtime.go)).

After the process is placed, init unshares a **cgroup namespace**, so `/proc/self/cgroup` in the container reads `0::/` instead of leaking the host's hierarchy.

## Killing a container properly

Killing PID 1 is not enough: a daemonized grandchild can outlive it, having been re-parented away. The container is the cgroup, so you kill the cgroup:

- **v2:** `echo 1 > cgroup.kill` kills every task atomically.
- **v1:** freeze the cgroup (`freezer.state = FROZEN`) so nothing can fork during the sweep, SIGKILL every PID in `cgroup.procs`, then thaw so the signals are delivered.

That is [`Kill`](../internal/cgroups/cgroups.go). `rh stop` sends `SIGTERM` to PID 1, waits for the grace period, then kills the cgroup: the same contract as Kubernetes' `terminationGracePeriodSeconds`.

## Metering and billing

A usage-billed platform needs to know how much CPU time and memory each service used, per second, without double counting and without losing data across restarts. Roundhouse's [meter](../internal/engine/metering.go) samples every running container on an interval:

```text
cpu_delta   = cpu.stat usage_usec (now)  −  (previous sample for this container)
cpu_seconds += cpu_delta
gb_seconds  += memory.current / 2^30  ×  interval
cost         = cpu_seconds/60 × $/vCPU-min  +  gb_seconds/60 × $/GB-min
```

Design points worth discussing in an interview:

- **Counters, not gauges, for CPU.** `usage_usec` is cumulative, so a missed sample loses no CPU time; the next delta includes it. Memory is a gauge, so a missed sample is a small estimation error. Sampling it more often, or reading `memory.peak`, reduces the error.
- **Restarts.** The previous reading is kept in memory only. After a daemon restart, the first sample only primes it, so no interval is counted twice. The cost is at most one interval of unbilled CPU per restart: the platform, not the customer, eats the error.
- **A container that restarts has a new cgroup**, and its counter starts from zero. Keying the previous reading by container ID and treating "new < old" as a reset avoids negative deltas.
- **Idempotent ingestion.** In a real system these samples would flow to a billing pipeline. Tag each sample with (container, sample time) so a retry cannot double bill. Capital Lab's ledger in the parent repository is built on the same principle.

```sh
sudo -E rh daemon --meter 2s &
sudo -E rh deploy -i mirror.gcr.io/library/busybox:latest --cmd 'md5sum /dev/zero' --cpu 0.25 spinner
sleep 20; sudo -E rh usage     # ~0.25 vCPU-seconds per second: the quota at work
```

## Lab 3

```sh
A=mirror.gcr.io/library/alpine:3.20
# Memory: watch the limit and the OOM kill
sudo -E rh run --name oom -m 32m $A sh -c 'x=a; while :; do x=$x$x; done'; echo "exit=$?"
sudo -E rh ps -a | grep oom          # Exited (137) … OOMKilled
sudo -E rh rm oom

# CPU throttling: a busy loop limited to 0.2 cores
sudo -E rh run -d --name spin --cpus 0.2 $A sh -c 'while :; do :; done'
sudo -E rh stats -w spin              # CPU % hovers around 20 (Ctrl-C to stop)
cat /sys/fs/cgroup/roundhouse/*/cpu.stat 2>/dev/null || cat /sys/fs/cgroup/cpu/roundhouse/*/cpu.stat
#   nr_throttled grows every period
sudo -E rh rm -f spin

# PIDs: a fork bomb, contained
sudo -E rh run --rm --pids 50 $A sh -c ':(){ :|:& };:'
```

## Check yourself

1. What is charged to a container's memory cgroup besides its heap? Why does it matter for OOM decisions?
2. Explain why a CPU limit can raise p99 latency while average CPU stays low.
3. What race does `CLONE_INTO_CGROUP` close?
4. Why is killing PID 1 not enough to stop a container, and what do v1 and v2 each do instead?
5. Design per-second billing from cgroup counters that survives agent restarts and never double bills.
