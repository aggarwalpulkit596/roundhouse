# F4: Docker and Compose, properly

> **Goal:** go from "I can run `docker compose up`" to knowing what every part of a Compose file does to the system, and how each maps onto the pieces you will build and study. Day 5 of the [20-day plan](../00-study-plan.md).

You have used Compose. This module makes you use it deliberately, while watching what it does underneath, so modules 1 to 8 feel like "oh, _that's_ how it works".

## 1. The architecture you are talking to

```text
docker / docker compose (CLI)
   │  REST API over /var/run/docker.sock
   ▼
dockerd  ── builds (BuildKit), networks, volumes, the API
   │  gRPC
   ▼
containerd  ── images (content store, snapshots), container lifecycle
   │
   ▼
containerd-shim-runc-v2  (one per container; keeps it alive if containerd restarts)
   │
   ▼
runc  ── creates namespaces and cgroups, pivots root, execs your process, then exits
```

Roundhouse has the same layers in one binary: `rh` CLI → `rh daemon` → container manager → `rh-shim` → runtime (`rh-init`). You will read each of them.

```sh
ps -ef --forest | grep -A3 containerd-shim    # the shim, and your container under it
sudo ls /run/containerd/                       # containerd's state
docker info | grep -iE 'cgroup|runtime|storage'
```

## 2. Every Compose field, and what it really does

| Compose field                               | What happens underneath                                                                          | Module |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------ | ------ |
| `image: postgres:17`                        | Resolve tag → manifest digest → pull config and layers → unpack as overlay layers                | 6, 2   |
| `build: .`                                  | BuildKit turns the Dockerfile into a graph, runs RUN steps in containers, caches steps           | 7      |
| `command:` / `entrypoint:`                  | Overrides the image config's `Cmd` / `Entrypoint`, then `execve`                                 | 1      |
| `environment:`                              | The `env` array passed to `execve`                                                               | F1     |
| `ports: "8080:80"`                          | Port publishing: iptables DNAT rules plus `docker-proxy`                                         | 5      |
| `networks:` / service names                 | A bridge per network, a veth per container, an embedded DNS server at 127.0.0.11                 | 5      |
| `volumes: data:/var/lib/…`                  | A directory under `/var/lib/docker/volumes`, bind-mounted into the container                     | 2      |
| `mem_limit`, `cpus`, `pids_limit`           | Values written into the container's cgroup                                                       | 3      |
| `restart: unless-stopped`                   | dockerd's restart policy with backoff                                                            | 8      |
| `healthcheck:`                              | dockerd execs the command periodically and records healthy/unhealthy                             | 8      |
| `depends_on: condition: service_healthy`    | Start order gated on health                                                                      | 8      |
| `user:`                                     | `setuid`/`setgid` before `execve`                                                                | 4      |
| `cap_drop`, `read_only`, `security_opt`     | Capabilities, read-only root, seccomp/AppArmor profiles                                          | 4      |
| `init: true`                                | tini as PID 1: signal forwarding and zombie reaping                                              | 1      |
| `stop_grace_period`                         | Time between SIGTERM and SIGKILL                                                                 | F1, 8  |
| `docker compose up` after editing a service | Recreate: stop the old container, then start the new one. **Not** a zero-downtime rolling deploy | 8      |

The last row matters for interviews: Compose is a single-host tool that recreates containers. Platforms (Kubernetes, Railway, Roundhouse's engine) do rolling deploys with health gating, which is the core of [module 8](../08-provisioning-engine.md).

## 3. Dockerfiles that build fast and run well

- **Order steps from least to most frequently changed.** Copy dependency manifests and install dependencies before copying source, so a code change does not reinstall everything (module 7 explains the cache keys).
- **Multi-stage builds:** build in a big image, copy only the artefact into a small one. `examples/hello/Railfile` does this.
- **`.dockerignore`:** keep `node_modules`, `.git` and build outputs out of the context, both for speed and for cache hits.
- **Exec form (`CMD ["app"]`) over shell form (`CMD app`):** shell form makes `/bin/sh` PID 1, which does not forward SIGTERM.
- **Run as non-root (`USER`)**, and do not bake secrets into layers: every layer is readable by anyone who can pull the image.

## Lab F4

1. Write a Compose file with a web app, Postgres with a named volume and a health check, and Redis. Bring it up, then answer from the host, with commands, not docs:
   - Which bridge did Compose create? (`ip link`, `docker network inspect`)
   - Which iptables rules implement your `ports:`? (`sudo iptables -t nat -S | grep DOCKER`)
   - Where is the volume's data on disk? (`docker volume inspect`)
   - What cgroup files did `mem_limit` change? (`cat /sys/fs/cgroup/system.slice/docker-<id>.scope/memory.max`)
   - What is PID 1 in the web container, and what is its host PID? (`docker top`)
2. Change an environment variable and run `docker compose up -d`. Use `curl` in a loop during the change: how long is the service down? Now you know what rolling deploys fix.
3. Rebuild the web image after changing one source file with `docker build --progress=plain .` and identify which steps were cached.
4. Run the same app with Roundhouse (`rh run`, then `rh daemon` + `rh deploy`) and compare, piece by piece.

## Resources

- Docker docs: [Get started](https://docs.docker.com/get-started/), the [Compose file reference](https://docs.docker.com/reference/compose-file/), and [Dockerfile best practices](https://docs.docker.com/build/building/best-practices/).
- Nigel Poulton, _Docker Deep Dive_: readable, and covers the containerd/runc split.
- Julia Evans, _How Containers Work_ zine.

## Check yourself

1. Name every process between `docker run` and your application, and what each one is for.
2. What happens to running containers when you restart dockerd? Why? (Hint: the shim; also see `live-restore`.)
3. Why is `CMD app` (shell form) a problem for graceful shutdown?
4. `docker compose up -d` after a config change: why is there downtime, and what would a rolling deploy do differently?
5. Why do Dockerfiles copy `package.json` and install dependencies before copying the rest of the source?
