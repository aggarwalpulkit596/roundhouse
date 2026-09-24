# Module 9: Orchestration at scale

> **Goal:** extend the one-node engine to a fleet in your head: placement, a multi-node control plane, networking across hosts, stateful services, isolation stronger than containers, and when Kubernetes is (and is not) the right substrate.

Roundhouse runs one node for real. This module is the design discussion for many, with the pieces that exist in code ([`internal/scheduler`](../internal/scheduler)) and the ones that are exercises.

## From one node to many

```text
                        ┌──────────── control plane (replicated) ─────────────┐
 users / CI ──► API ──► │ desired state (Postgres/etcd, versioned per service) │
                        │ scheduler: which node gets which replica             │
                        │ node registry: capacity, heartbeats, leases          │
                        └───────────────┬───────────────────────────┬──────────┘
                                        │ assignments               │ assignments
                          ┌─────────────▼──────────┐   ┌────────────▼───────────┐
                          │ node agent (≈ rh daemon)│  │ node agent              │ …
                          │ reconciles its share    │  │                         │
                          │ reports observed state  │  │                         │
                          └─────────────────────────┘  └─────────────────────────┘
                                   ▲  WireGuard / VXLAN overlay between nodes  ▲
                           edge fleet (L4/L7 proxies) routes public traffic to healthy instances
```

The per-node part of Roundhouse's engine is roughly what an **agent** does: it owns containers on its host and reconciles toward the assignments it is given. What changes:

1. **Desired state moves off the node** into a replicated store, and assignments ("replica 2 of web rev 7 runs on node-14") become the agent's input.
2. **Nodes heartbeat** and hold **leases**. A node that misses its lease is presumed dead, and its replicas are rescheduled. When a partitioned node comes back, it must not keep serving the old replicas: agents stop anything whose assignment they can no longer confirm (fencing). This is where split-brain bugs live.
3. **The scheduler** decides placement (below).
4. **Networking** must span hosts: each node gets a subnet (for example a /24 out of a /16, or an IPv6 /64), and nodes route between subnets over an overlay (VXLAN, WireGuard) or natively. Railway uses a WireGuard mesh with eBPF NAT and per-environment IPv6 ([docs](https://docs.railway.com/networking/private-networking/how-it-works)).
5. **Discovery and the edge** read endpoints from the control plane rather than from one daemon.

## Scheduling

[`scheduler.Place`](../internal/scheduler/scheduler.go) is kube-scheduler's model in miniature:

1. **Filter:** drop nodes that cannot run the replica (cordoned, not enough free CPU or memory). Every rejection is recorded with a reason, because "why is my deploy pending?" is a support ticket you want the system to answer.
2. **Score:** rank the survivors by soft preferences:
   - **Bin-packing** (Kubernetes `MostAllocated`): prefer the fullest node that fits. Fewer, fuller machines means lower cost and whole machines that can be drained for maintenance. For a usage-billed platform on its own hardware, this is the default instinct.
   - **Spreading** (`LeastAllocated`): prefer the emptiest node, leaving headroom everywhere. Better for bursty workloads.
   - **Anti-affinity:** a heavy penalty per existing replica of the same service on the node, and a smaller one per replica in the same zone. Two replicas on one host are one kernel panic away from zero.
3. **Pick** the best, update its reservation, repeat for the next replica.

```sh
rh schedule --nodes a:8:16384:6:12000:z1,b:8:16384:1:2000:z1,c:8:16384:2:4000:z2,d:2:1024::z2 --replicas 3
rh schedule --nodes a:8:16384:6:12000:z1,b:8:16384:1:2000:z1,c:8:16384:2:4000:z2 --strategy spread
rh schedule --agents unix:///run/roundhouse.sock      # a live node's real capacity
```

Topics to be ready for:

- **Requests vs actual usage.** Scheduling on declared reservations wastes capacity (users over-ask). Scheduling on measured usage risks overcommit and noisy neighbours. Platforms mix both: reserve on requests, then overcommit a percentage based on observed usage, and rely on cgroup weights and eviction.
- **Rescheduling and bin-packing drift.** Placement that was optimal a week ago is not today. A descheduler or live migration consolidates.
- **Scheduling speed.** Filtering 10,000 nodes per replica is slow. Sample a subset (kube-scheduler scores a percentage of nodes in big clusters) or shard.
- **Stateful services** are pinned to where their volume lives, unless the storage is network-attached or replicated.

## Builds: affinity with balance

Covered in [module 7](07-builds.md): rendezvous hashing to a small candidate set, least-loaded among them. [`PickBuilder`](../internal/scheduler/scheduler.go).

## Isolation stronger than containers

Containers share the host kernel. A kernel bug reachable through a syscall is a path from one tenant to all of them. Options, in increasing isolation and cost:

| Approach                                | Boundary                                      | Cost                                                                             |
| --------------------------------------- | --------------------------------------------- | -------------------------------------------------------------------------------- |
| Container + seccomp + userns + LSM      | shared kernel, reduced surface                | ~zero                                                                            |
| gVisor                                  | a user-space kernel intercepts syscalls       | syscall-heavy workloads slow down; compatibility gaps                            |
| microVM (Firecracker, Cloud Hypervisor) | hardware virtualization, minimal device model | ~125 ms boot, some memory overhead per VM, needs KVM (bare metal or nested virt) |
| Dedicated VM / host per tenant          | everything                                    | expensive                                                                        |

Firecracker ([NSDI 2020 paper](https://www.usenix.org/conference/nsdi20/presentation/agache)) exists because AWS Lambda needed VM isolation with container-like density and start time. Railway describes running its builds in microVM "build cells" ([builder post](https://blog.railway.com/p/new-builder-scale-big)): builds execute arbitrary code as root, which is exactly the case for a VM boundary. Owning bare metal ("Railway Metal") is what makes KVM-based isolation cheap: in most clouds you would need nested virtualization or bare-metal instances.

A good exercise for the interview: sketch a `Runtime` implementation for Roundhouse that starts a Firecracker microVM instead of a namespace container. The engine would not change at all. That is the payoff of the interface.

## Kubernetes or build your own?

You will be asked this. Arguments you should be able to make on both sides:

**For Kubernetes (or Nomad) as the substrate**

- Scheduling, health checking, rolling deploys, service discovery, RBAC, and a huge ecosystem, all battle-tested.
- Hiring: people know it. Managed offerings remove the control-plane burden.

**For a custom engine** (a platform like Railway, Fly.io or Render at scale)

- The **product** is the platform. Deploy semantics (immutable deployments with user-facing timelines, instant rollback, per-second billing, sleeping services, per-PR environments) are the product, and mapping them onto Deployments, ReplicaSets and Services adds a layer to fight rather than build on.
- **Density and control:** per-pod overheads, etcd limits, and the control-plane's scale envelope (Kubernetes documents ~5,000 nodes and 150,000 pods per cluster), so large multi-tenant platforms end up running many clusters and building a federation layer anyway.
- **Multi-tenancy:** Kubernetes' tenancy model (namespaces, RBAC, network policy) was not designed for hostile tenants sharing nodes; hard multi-tenancy needs extra layers (microVMs, custom networking) either way.
- **Bare metal:** when you own the hardware, networking and storage, many of Kubernetes' cloud integrations do not apply.

The honest summary: Kubernetes is an excellent default for running _your own_ services, and a heavyweight, leaky abstraction for _being_ a platform that runs everyone else's. Read Railway's engineering blog for their view in their own words ([blog.railway.com/engineering](https://blog.railway.com/engineering)); this module argues it from first principles and does not claim to describe Railway's internals.

## Regions, sleeping services and other product-shaped problems

These come up because they are product features that force infrastructure design:

- **Regions:** a service deployed to several regions needs global routing (anycast or GeoDNS), per-region placement, and a decision about where its database lives. Most apps are not multi-region safe, so the default is one region and replicas within it.
- **Sleeping / scale-to-zero:** stop idle services and start them on the first request. The edge must hold the request while the container boots, which makes start latency (image pull, boot, readiness) a product metric. Pre-pulled images and snapshot/restore (CRIU, VM snapshots) are how platforms make this fast.
- **Preview environments:** every PR gets a copy of a project's services. Cheap only if builds are cached and images are shared.
- **Volumes:** local NVMe is fast but pins a service to a host; network storage moves but is slower and a shared failure domain. Snapshots and backups are the platform's job.

## Check yourself

1. List what must change to turn Roundhouse's per-node engine into an agent under a central control plane.
2. What is fencing, and what goes wrong without it?
3. Bin-pack or spread for a usage-billed platform on owned hardware? What about for latency-sensitive tenants?
4. Where would a Firecracker-based runtime plug into Roundhouse, and what would it change for images and networking?
5. Make the case for and against running a PaaS on Kubernetes, in two minutes each.
