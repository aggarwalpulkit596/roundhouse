# Roundhouse

A container engine and deployment platform built from Linux primitives, as a hands-on curriculum for infrastructure engineering roles such as [Railway's Senior Infra Engineer: Platform](https://railway.com/careers/infra-platform).

Roundhouse is small enough to read in a week and real enough to deploy with: it pulls images from real registries, isolates them with namespaces and cgroups, networks them over a bridge, builds images with a cached multi-stage builder, pushes to its own OCI registry, and rolls services out with health checks and zero-downtime traffic switching. It never shells out to Docker, runc or containerd. Every mechanism is in this directory, and every one has a test that runs against a real kernel.

> A roundhouse is the building where a railway services its locomotives. This one is for learning how the engines work.

**~11,600 lines of commented Go · 3 dependencies (x/sys, netlink, x/sync) · 53 unit tests · 12 root integration tests on a real kernel**

## What it does

```text
                        rh CLI  ──── unix socket / HTTP ────┐
                                                           ▼
 ┌──────────────────────────── rh daemon (per node) ─────────────────────────────┐
 │  API ── desired state (services, deployments) ── work-queue reconciler        │
 │   │                                                  │        │               │
 │   │   edge proxy :public ◄── routing ◄── health prober        │               │
 │   │   private DNS  *.rh.internal ◄──┘                        ▼               │
 │   │   usage meter (cgroups → $/second)            container manager           │
 └───┼──────────────────────────────────────────────────────┼─────────────────────┘
     │                                                      ▼
     │               ┌──────── rh-shim (one per container, survives daemon restarts)
     │               │           logs, exit status, port proxies
     │               ▼
     │        rh-init (PID 1)  ── namespaces · pivot_root · cgroups · capabilities
     │               │
     │         your process   ◄── overlayfs rootfs ◄── image layers ◄── registry
     ▼
 rh build (Railfile → stage DAG → cached steps → OCI image) ── rh push ──► rh registry
```

| Layer            | You know it as                  | Roundhouse implements it in                                                                | Production equivalent                    |
| ---------------- | ------------------------------- | ------------------------------------------------------------------------------------------ | ---------------------------------------- |
| Runtime          | `docker run`, a pod's container | [`internal/runtime`](internal/runtime), [`internal/nsenter`](internal/nsenter)             | runc, crun                               |
| Resource control | `resources.limits`              | [`internal/cgroups`](internal/cgroups)                                                     | runc/libcontainer cgroups, systemd       |
| Filesystem       | image layers                    | [`internal/rootfs`](internal/rootfs), [`internal/image`](internal/image)                   | containerd overlayfs snapshotter         |
| Images           | `docker pull/push`              | [`internal/image`](internal/image), [`internal/registry`](internal/registry)               | containerd content store, distribution   |
| Networking       | a pod IP, `ports:`, Service DNS | [`internal/network`](internal/network), [`internal/engine/dns.go`](internal/engine/dns.go) | CNI bridge plugin, kube-proxy, CoreDNS   |
| Supervision      | "the container keeps running"   | [`internal/container`](internal/container) (shim)                                          | containerd-shim-runc-v2, conmon          |
| Control loop     | Deployment controller, kubelet  | [`internal/engine`](internal/engine)                                                       | Kubernetes controllers, Railway's engine |
| Builds           | `docker build`, Railway builds  | [`internal/builder`](internal/builder)                                                     | BuildKit                                 |
| Placement        | kube-scheduler                  | [`internal/scheduler`](internal/scheduler)                                                 | kube-scheduler, Nomad, Borg              |

## Quick start

Requirements: Linux (a VM is fine), root, Go 1.24+. Roundhouse works on cgroup v1, hybrid and v2 hosts.

```sh
git clone https://github.com/aggarwalpulkit596/roundhouse && cd roundhouse
go build -o rh ./cmd/rh
export RH_ROOT=/var/lib/roundhouse          # state directory (the default)

# 1. A container from first principles
sudo -E ./rh run --rm mirror.gcr.io/library/alpine:3.20 sh -c 'echo "I am PID $$ on $(hostname)"; ps'

# 2. Limits you can watch the kernel enforce
sudo -E ./rh run --rm -m 32m mirror.gcr.io/library/alpine:3.20 sh -c 'x=a; while :; do x=$x$x; done'; echo "exit $?"   # 137: OOM-killed
sudo -E ./rh run --rm --pids 20 mirror.gcr.io/library/alpine:3.20 sh -c ':(){ :|:& };:'                                  # fork bomb contained

# 3. Build a multi-stage image (parallel stages, cached steps)
sudo -E ./rh build -t hello:v1 examples/hello

# 4. Run the platform and deploy it
sudo -E ./rh daemon &
sudo -E ./rh deploy -i hello:v1 --port 8080 --public 8000 --replicas 2 --health /health -e VERSION=v1 hello
curl localhost:8000
sudo -E ./rh deploy -i hello:v1 --port 8080 --public 8000 --replicas 2 --health /health -e VERSION=v2 hello   # zero-downtime
sudo -E ./rh svc status hello
sudo -E ./rh usage
```

`mirror.gcr.io` is Google's Docker Hub mirror, used because Docker Hub rate-limits anonymous pulls. Any registry works: `rh pull alpine` goes to Docker Hub.

## Commands

| Group      | Commands                                                                                |
| ---------- | --------------------------------------------------------------------------------------- |
| Containers | `run` `create` `start` `ps` `exec` `logs` `stop` `kill` `rm` `stats` `inspect`          |
| Images     | `pull` `images` `tag` `rmi`                                                             |
| Build/ship | `build` `push` `registry`                                                               |
| Platform   | `daemon` `deploy` `svc ls/status/logs/redeploy/rollback/rm` `events` `usage` `schedule` |

Run `rh <command> -h` for flags.

## Learning path

Start with the [study plan](docs/00-study-plan.md). It is written for someone who already knows platform engineering (Kubernetes, CI/CD, Terraform) and wants to go one level down into the kernel and the runtime.

| #   | Module                                                    | Question it answers                                                 |
| --- | --------------------------------------------------------- | ------------------------------------------------------------------- |
| 0   | [Study plan](docs/00-study-plan.md)                       | What do I study, in what order, and how do I know I know it?        |
| 1   | [Processes and namespaces](docs/01-namespaces.md)         | What is a container, really?                                        |
| 2   | [Filesystems](docs/02-filesystems.md)                     | chroot vs pivot_root, overlayfs, layers, whiteouts                  |
| 3   | [cgroups](docs/03-cgroups.md)                             | How are limits enforced and usage measured and billed?              |
| 4   | [Security](docs/04-security.md)                           | Capabilities, no_new_privs, masked paths, what is still missing     |
| 5   | [Networking](docs/05-networking.md)                       | veth, bridges, NAT, proxies, private DNS                            |
| 6   | [Images and registries](docs/06-images.md)                | OCI image and distribution specs, content addressing                |
| 7   | [Builds](docs/07-builds.md)                               | How BuildKit-style caching and parallelism work                     |
| 8   | [The provisioning engine](docs/08-provisioning-engine.md) | Designing the control loop: the interview project                   |
| 9   | [Orchestration at scale](docs/09-scale.md)                | Scheduling, multi-node, microVMs, and when not to use Kubernetes    |
| 10  | [Interview prep](docs/10-interview-prep.md)               | Question bank, design walkthrough, reading runc/containerd/BuildKit |
| 11  | [Capital Lab on Roundhouse](docs/11-capital-lab.md)       | Deploying a real app (and its database) on your platform            |
| 12  | [Exercises](docs/12-exercises.md)                         | Features to add, ordered by difficulty                              |

## Testing

```sh
go test -race ./...                                             # unit tests, no root needed
sudo env "PATH=$PATH" go test -tags integration -v ./integration/   # real kernel, ~20s
```

The engine's control loop runs against a fake runtime in its unit tests, so rollouts, failed deploys, crash loops and daemon restarts are tested in milliseconds. The integration suite runs the actual binary: it proves PID 1 isolation, OOM kills, fork-bomb containment, capability dropping, bridge networking, `exec` into namespaces, reproducible cached builds, registry push/pull, and a rolling update that drops zero requests under load. CI runs both on every push and pull request ([workflow](.github/workflows/ci.yml)).

## Scope and honesty

Roundhouse is a teaching implementation. It is complete enough to run real workloads on one machine and to reason about many, but it is not hardened. Missing on purpose, and listed as [exercises](docs/12-exercises.md): seccomp filters, user namespaces (rootless), AppArmor/SELinux, zstd layers, image garbage collection, TLS/auth on the API and registry, and a multi-node control plane (the scheduler library is there; the controller is an exercise). The [security module](docs/04-security.md) explains what each gap would let an attacker do.

Nothing here is Railway's code or a description of Railway's internals beyond what Railway has published. Where the docs compare with Railway, they cite the public source.

## License

Apache License 2.0. See [LICENSE](LICENSE).
