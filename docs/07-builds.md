# Module 7: Builds

> **Goal:** understand how BuildKit makes builds fast (graph, cache keys, parallelism, pruning), how Roundhouse's builder implements the same ideas in ~1,000 lines, and how a platform runs builds at scale.

The job description for Railway's platform role talks about efficient OCI build and transfer. For users, the build is most of the time between `git push` and "live". Railway reports running more than 50 million builds a month on BuildKit ([_Counting to 3 with a new builder_](https://blog.railway.com/p/new-builder-scale-big)).

## From Dockerfile to graph

A classic `docker build` executed a Dockerfile top to bottom, committing a layer per step. BuildKit instead compiles it into a graph (LLB: low-level build definition) of operations (image source, exec, file copy, merge) and then **solves** the graph:

- Nodes with no path between them run **concurrently**.
- Nodes the requested target does not depend on are **never run**.
- Every node has a **cache key** derived from its definition and its inputs' keys, so any subgraph that was built before is reused, even from another Dockerfile.

Roundhouse's builder ([`internal/builder`](../internal/builder)) does this at stage granularity:

```text
Railfile                                     graph
─────────                                    ─────
FROM golang AS build      ─┐                 [build] ──┐
FROM busybox AS assets     ├── Parse() ──►   [assets] ─┼──► [final]      [unused] (pruned)
FROM busybox AS unused     │                           │
FROM busybox               │   Deps: FROM <stage>, COPY --from=<stage>
COPY --from=build …       ─┘
COPY --from=assets …
```

[`Railfile.Needed`](../internal/builder/parse.go) walks dependencies from the target. [`Build`](../internal/builder/builder.go) starts a goroutine per needed stage; each waits only on its own dependencies' `done` channels. The `examples/hello` build shows `build` and `assets` running interleaved, and the `unused` stage's 30-second `sleep` never happens.

## Cache keys

Every step's key is a hash of **the previous step's key** plus **everything that can change its output**:

| Step          | Key inputs                                                                                   |
| ------------- | -------------------------------------------------------------------------------------------- |
| `FROM`        | the base image's manifest digest (not the tag: `alpine:3.20` moves)                          |
| `RUN`         | command, environment (including `ARG` values), working directory, user, network mode         |
| `COPY`        | destination, `--chown`, and the **content** of every source file: relative path, mode, bytes |
| `COPY --from` | the source stage's final key (or image digest) and the paths                                 |
| metadata      | `ENV`, `WORKDIR`, `USER` and so on update the key without producing a layer                  |

Because keys chain, changing one input invalidates exactly that step and everything after it, which is why Dockerfiles copy dependency manifests (`go.mod`, `package.json`) and install dependencies _before_ copying the source.

Two deliberate choices, both matching BuildKit:

- **File mtimes are not in the key.** A fresh `git clone` gives every file a new mtime. If mtimes mattered, CI would never hit the cache.
- **Proxy variables are passed to RUN but kept out of the key and the image.** A build behind a corporate proxy should share cache with one that is not.

The cache itself is a directory of small JSON records, `key → {diffID, compressed blob}` ([`cacheLookup`](../internal/builder/builder.go)). A record only counts as a hit if its layer still exists in the store, so deleting layers can never produce a broken image.

## Executing RUN

A RUN step is a container ([`buildRun.run`](../internal/builder/builder.go)):

1. Mount an overlay: the stage's current layers as lowerdirs, a fresh upperdir.
2. Start `/bin/sh -c <cmd>` with the runtime package: new PID/mount/UTS/IPC namespaces, host network by default (like `docker build` behind proxies; `--network none` for hermetic steps), the image's user, and `HOME` from the image's `/etc/passwd`.
3. Bind the host's `resolv.conf`/`hosts` read-only over the image's copies, so DNS works without writing those files into the layer.
4. On exit 0, unmount. **The upperdir is the layer.** It is already in overlay format (whiteouts as 0/0 char devices), so the builder tars it into an OCI layer (translating whiteouts back) and moves the directory straight into the layer store. Nothing is ever extracted twice.

BuildKit runs steps with runc (or in rootless mode) and snapshots with containerd's snapshotters, the same shape at production strength.

## Reproducible layers

Build the same inputs twice and you get the same digests only if nothing incidental leaks into the tar: file order, mtimes, uid/gid names, gzip headers. [`WriteLayer`](../internal/image/layer.go) sorts entries, drops user and group names, zeroes atime and ctime, uses a gzip header with no name or timestamp and, when `SOURCE_DATE_EPOCH` is set, clamps every mtime to it. The integration test builds twice and asserts identical image IDs. Reproducibility matters for a platform because identical bytes deduplicate across builds, registries and hosts, and because users can verify what they run.

## Builds at scale: what Railway describes

From Railway's [builder post](https://blog.railway.com/p/new-builder-scale-big): builds run on a static pool of large bare-metal hosts (256 vCPU / 512 GB), each split into **build cells**, microVMs of 32 vCPU / 64 GB running `buildkitd`. A build is routed to **one of three cells on a consistent-hash ring**, so repeat builds of the same project land where their cache is warm while still spreading load. The post also discusses the operational problems that came with it (DNS reliability inside VMs, clock drift, snapshot uploads to object storage). Why each design choice:

- **microVMs, not containers:** builds run arbitrary user code as root with network access; a VM boundary is a much smaller attack surface than a shared kernel.
- **Static cells on bare metal:** predictable performance and no per-build VM cold starts; cells are reused across builds.
- **Hash ring with a small candidate set:** a pure hash gives perfect cache affinity but no load balancing; pure least-loaded gives balance but cold caches. Picking the least loaded of the key's top-3 gets most of both.

Roundhouse implements that last idea in [`scheduler.PickBuilder`](../internal/scheduler/scheduler.go) with rendezvous hashing, which is the simplest form of consistent hashing (no ring, no virtual nodes). Try it:

```sh
rh schedule --nodes b1:1:1,b2:1:1,b3:1:1,b4:1:1,b5:1:1 --build-key github.com/acme/api
rh schedule --nodes b1:1:1,b2:1:1,b4:1:1,b5:1:1 --build-key github.com/acme/api   # b3 gone
```

[`TestRendezvousIsStableAndMinimallyDisruptive`](../internal/scheduler/scheduler_test.go) proves the key property: removing a builder only moves the keys that builder owned.

## Lab 7

```sh
cd roundhouse
sudo -E rh build -t hello:v1 examples/hello        # cold: pulls golang, compiles
sudo -E rh build -t hello:v1 examples/hello        # warm: every step CACHED
echo '// tweak' >> examples/hello/main.go
sudo -E rh build -t hello:v2 examples/hello        # predict which steps rebuild first!
git checkout examples/hello/main.go
SOURCE_DATE_EPOCH=0 sudo -E rh build --no-cache -t r:1 examples/hello
SOURCE_DATE_EPOCH=0 sudo -E rh build --no-cache -t r:2 examples/hello
sudo -E rh images | grep '^r:'                      # same IMAGE ID? If not, find what leaked.
```

The last experiment may **not** give identical IDs: the Go compiler embeds a build ID, and the `assets` stage writes the current date into a file. Finding and fixing the sources of non-determinism is the actual skill.

## Check yourself

1. What goes into a COPY step's cache key, and why not mtimes?
2. Why does BuildKit prune unused stages, and why could the classic builder not?
3. Explain how a RUN step's filesystem changes become an OCI layer, including deletions.
4. Why run builds in microVMs rather than containers on a multi-tenant platform?
5. Design build routing for 1,000 builders so caches stay warm but no builder is overloaded. What happens when a builder dies?
