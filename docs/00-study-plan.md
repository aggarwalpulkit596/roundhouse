# The 20-day intensive: from CI/CD and Compose to infrastructure engineer

This plan is for someone who has written CI pipelines and used Docker Compose, and wants to be ready for a senior platform infrastructure interview (the Railway role this project was built around) in 20 days of full-time study, **6 to 8 hours a day**, about 140 to 160 hours in total.

That is enough time to go deep, if every hour is spent doing, not just reading. The plan is built on three rules:

1. **See it, then see under it.** Every concept is met three times: with Docker (what you know), with raw Linux commands (the primitive), and in Roundhouse's source (how an engine uses the primitive).
2. **Write it down.** A lab notebook entry for every lab: what you ran, what you expected, what happened. The notebook becomes your interview material.
3. **Say it out loud.** Every day ends with the module's "Check yourself" questions, answered aloud without notes. If you cannot explain it, you do not know it yet.

## Before day 1

Set up your lab machine: [F0: Setup](foundations/f0-setup.md). Budget one hour the evening before. Also get these, which you will use throughout:

- Julia Evans' zines _How Containers Work_, _Bite Size Linux_ and _Networking! ACK!_ ([wizardzines.com](https://wizardzines.com/)).
- Martin Kleppmann, _Designing Data-Intensive Applications_ (for days 12, 15 and 19).
- The [glossary](glossary.md). Every day, add the day's new terms to your own flashcards (paper, Anki, anything).

## The shape of a day (7 hours)

| Block     | Time  | What you do                                                                                        |
| --------- | ----- | -------------------------------------------------------------------------------------------------- |
| 1. Learn  | 2 h   | Read the day's module and primary sources. Take notes by hand.                                     |
| 2. Lab    | 2.5 h | Do the lab. Break things on purpose. Notebook entry for every surprise.                            |
| 3. Code   | 1.5 h | Read or change the Roundhouse code the module points to; run its tests.                            |
| 4. Review | 1 h   | "Check yourself" aloud; write a 10-line summary of the day; flashcards; revisit yesterday's cards. |

Take a real break between blocks, and a 10-minute break every hour. Twenty days at this pace is a marathon. **Days 7 and 14 end early by design** (the review block becomes catch-up), and each day marks optional items **(opt)** you can skip if behind.

## The map: what you know, and what it is underneath

Keep this table open. By day 20 you should be able to explain every row and point to the code.

| You know it from Docker / Compose             | It is (kernel / runtime view)                                                           | Where in Roundhouse                                       | Day |
| --------------------------------------------- | --------------------------------------------------------------------------------------- | --------------------------------------------------------- | --- |
| A container                                   | A process tree with its own namespaces, a cgroup, a pivoted root and fewer capabilities | `internal/runtime/runtime.go`, `init.go`                  | 6   |
| `docker exec`                                 | `setns(2)` into the container's namespaces, then fork                                   | `internal/nsenter/nsenter.go`, `internal/runtime/exec.go` | 6   |
| `docker stop` taking 10 seconds               | PID 1 ignores SIGTERM without a handler; SIGKILL after the grace period                 | `internal/runtime/init.go` (`runInit`)                    | 6   |
| An image, `FROM alpine`                       | A manifest listing a config and layer tarballs, all named by sha256                     | `internal/image/types.go`, `store.go`                     | 10  |
| Image layers                                  | Tar files unpacked into directories and stacked by overlayfs                            | `internal/image/unpack.go`, `internal/rootfs/overlay.go`  | 7   |
| Files you write inside a container            | The overlay's per-container upper directory (copy-up on first write)                    | `internal/rootfs/overlay.go`                              | 7   |
| `volumes:`                                    | A bind mount of a host directory                                                        | `internal/runtime/mounts.go` (`bindVolume`)               | 7   |
| `mem_limit: 256m`                             | `memory.max` in the container's cgroup                                                  | `internal/cgroups/cgroups.go`                             | 8   |
| `cpus: 0.5`                                   | `cpu.max = "50000 100000"`: a quota per period (throttling, not speed)                  | `internal/cgroups/cgroups.go`                             | 8   |
| Exit code 137, "OOMKilled"                    | The kernel OOM killer sent SIGKILL (128 + 9) inside the memory cgroup                   | `internal/container/start.go` (`finish`)                  | 8   |
| Running as root in a container                | UID 0 with most capabilities dropped and `no_new_privs` set                             | `internal/runtime/caps.go`, `init.go`                     | 8   |
| The container's IP, service names resolving   | A veth pair into a network namespace, a bridge, and a DNS server                        | `internal/network/network.go`, `internal/engine/dns.go`   | 9   |
| `ports: "8080:80"`                            | A proxy (userland or iptables DNAT) from a host port to the container                   | `internal/network/proxy.go`                               | 9   |
| `docker pull` / `push`                        | The OCI distribution HTTP API with token auth and content addressing                    | `internal/image/registry.go`, `internal/registry/`        | 10  |
| `docker build` and its cache                  | A graph of steps, each with a content-addressed cache key, run in containers            | `internal/builder/builder.go`                             | 11  |
| `restart: unless-stopped`                     | A restart policy with exponential backoff, computed from observed exits                 | `internal/engine/sync.go` (`maintain`)                    | 13  |
| `healthcheck:`                                | A prober whose results gate traffic and promotion                                       | `internal/engine/health.go`                               | 13  |
| `docker compose up` after a change (downtime) | A rolling deploy: start new, wait for health, switch traffic, drain old                 | `internal/engine/sync.go` (`rollout`)                     | 13  |
| Containers surviving a daemon restart         | Per-container shims own containers; the control plane only observes and acts            | `internal/container/start.go` (`ShimMain`)                | 13  |
| "Where should this run?"                      | A scheduler: filter nodes by hard constraints, score by preferences                     | `internal/scheduler/scheduler.go`                         | 15  |
| Your cloud bill                               | Sampling cumulative cgroup CPU and memory counters, priced per second                   | `internal/engine/metering.go`                             | 8   |

## Part 1: foundations (days 1–5)

### Day 1: processes, system calls, file descriptors

- **Learn:** [F1](foundations/f1-linux.md) sections 1–3. Zine: _Bite Size Linux_. `man 2 fork`, `man 2 execve`, `man 2 wait`.
- **Lab:** F1 lab items 1, 2 and 6. Then `strace -f docker run --rm alpine true 2>&1 | grep -E 'execve|clone' | head` and try to explain what you see (you will fully understand it on day 6).
- **Code:** read [`cmd/rh/main.go`](../cmd/rh/main.go). Find the three "internal" entry points (`__init`, `__exec`, `__shim`) and write in your notebook what you guess each is for.
- **Check:** F1 questions 1, 2 and 5.

### Day 2: signals, users, mounts, `/proc`, systemd

- **Learn:** F1 sections 4–8. `man 7 signal` (the "Standard signals" table).
- **Lab:** F1 lab items 3, 4 and 5. Then: `docker run -d --name s alpine sleep 1000; time docker stop s`. Why 10 seconds? Write your hypothesis.
- **Code:** [`internal/runtime/init.go`](../internal/runtime/init.go), function `runInit` only (about 50 lines): it is a tiny PID 1. Match every line to something you learned today.
- **Check:** all F1 questions. Start your flashcards (glossary: process, PID, syscall, file descriptor, signal, exit status, zombie).

### Day 3: networking fundamentals

- **Learn:** [F2](foundations/f2-networking.md), all of it. Zine: _Networking! ACK!_. HPBN chapter 2 (TCP) **(opt)**.
- **Lab:** F2 labs 1–5. Keep the network drawing from lab 1: you will redraw it on day 9.
- **Code:** [`internal/network/proxy.go`](../internal/network/proxy.go): a TCP proxy in ~150 lines. Find where half-close is handled.
- **Check:** all F2 questions.

### Day 4: Go for infrastructure

- **Learn:** [F3](foundations/f3-go.md). The whole [Tour of Go](https://go.dev/tour/) (about 3 hours), then _Go by Example_ pages on goroutines, channels, select, WaitGroups, mutexes, context.
- **Lab:** F3 lab item 2 (the concurrent downloader). This is the most important coding exercise of the week.
- **Code:** F3 lab items 3–5 in Roundhouse (tests, the work queue, the race detector).
- **Check:** all F3 questions.

### Day 5: Docker and Compose, properly, and your first container from scratch

- **Learn:** [F4](foundations/f4-docker-and-compose.md). Watch Liz Rice, [_Containers From Scratch_](https://www.youtube.com/watch?v=8fi7uSYlOdc) (40 minutes). Watch it twice.
- **Lab (morning):** F4 lab items 1–3.
- **Lab (afternoon), the milestone of week 1:** write your own container runtime in Go, in a new directory, following the talk: `run` re-executes itself as `child`, with `CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNS`, sets a hostname, `chroot`s into an unpacked Alpine rootfs (`docker export $(docker create alpine) | tar -x -C rootfs`), mounts `/proc`, and runs a shell. About 60 lines. Do not copy the code; type it while understanding it. [Reference repo](https://github.com/lizrice/containers-from-scratch) only when stuck.
- **Check:** F4 questions. Then explain your 60-line runtime aloud, line by line.

## Part 2: the container stack (days 6–11)

### Day 6: namespaces and how a runtime starts a process

- **Learn:** [Module 1](01-namespaces.md). `man 7 namespaces`, `man 7 pid_namespaces`, `man 2 setns`.
- **Lab:** module 1 lab (unshare/nsenter, then `rh`). Answer your day-2 `docker stop` question for real.
- **Code:** `internal/runtime/runtime.go` and `init.go` in full. Draw the parent/child sequence diagram from memory. Then compare with your day-5 runtime: list five things Roundhouse does that yours does not, and why each matters.
- **Code (opt):** runc's [`libcontainer/nsenter/nsexec.c`](https://github.com/opencontainers/runc/blob/main/libcontainer/nsenter/nsexec.c), first 200 lines.
- **Check:** module 1 questions.

### Day 7: filesystems, overlayfs and layers (ends early)

- **Learn:** [Module 2](02-filesystems.md). `man 2 pivot_root`, the [overlayfs docs](https://docs.kernel.org/filesystems/overlayfs.html) (sections "Upper and Lower", "whiteouts").
- **Lab:** module 2 lab. Then upgrade your day-5 runtime from `chroot` to `pivot_root` (with a private mount namespace), and prove the chroot escape no longer works.
- **Code:** `internal/runtime/mounts.go`, `internal/rootfs/overlay.go`, `internal/fsutil/securejoin.go` and its test.
- **Review (short) + catch-up** on anything unfinished from days 1–6.

### Day 8: cgroups and security

- **Learn:** [Module 3](03-cgroups.md) and [Module 4](04-security.md). Kernel [cgroup v2 docs](https://docs.kernel.org/admin-guide/cgroup-v2.html): "Basic Operations", then the memory, cpu and pids controllers. `man 7 capabilities` ("Transformation of capabilities during execve").
- **Lab:** module 3 and module 4 labs. Add a memory limit and a pids limit to your day-5 runtime by writing cgroup files yourself, and fork-bomb it.
- **Code:** `internal/cgroups/cgroups.go`, `internal/runtime/caps.go`, `applyCredentials` in `init.go`, `internal/engine/metering.go`.
- **Check:** modules 3 and 4 questions.

### Day 9: container networking

- **Learn:** [Module 5](05-networking.md).
- **Lab:** module 5 lab: build the two-namespace network by hand, then add networking to your day-5 runtime (a veth into the child's namespace, an IP, a route) using `ip` commands from the parent while the child waits. Redraw your day-3 network drawing with a running container.
- **Code:** `internal/network/network.go` (`Attach`, IPAM), `internal/engine/dns.go`, and how `BeforeRelease` in `runtime.go` connects them.
- **Check:** module 5 questions.

### Day 10: images and registries

- **Learn:** [Module 6](06-images.md). The [OCI image spec](https://github.com/opencontainers/image-spec) `manifest.md` and `layer.md`, and the [distribution spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md) "Pull" and "Push" sections.
- **Lab:** module 6 lab, including the manual token dance with `curl`. Then pull an image **by hand**: fetch its manifest and layers with `curl`, verify each sha256, and unpack it into a rootfs for your day-5 runtime. Your runtime now needs no Docker at all.
- **Code:** `internal/image/registry.go`, `store.go` (`Pull`), `unpack.go` and `image_test.go`.
- **Check:** module 6 questions.

### Day 11: builds

- **Learn:** [Module 7](07-builds.md). BuildKit's [solver doc](https://github.com/moby/buildkit/blob/master/docs/dev/solver.md). Railway's [builder post](https://blog.railway.com/p/new-builder-scale-big).
- **Lab:** module 7 lab: cold and warm builds, predicting cache hits, reproducibility hunting.
- **Code:** `internal/builder/parse.go`, `builder.go` (`Build`, `step`, `run`, `copy`), `internal/image/layer.go`.
- **Build:** add a small feature to the builder, for example a `LABEL`-from-`ARG` test, or support for `WORKDIR` creating the directory in a layer, with a test.
- **Check:** module 7 questions.

## Part 3: the control plane (days 12–16)

### Day 12: Kubernetes concepts and distributed-systems basics

- **Learn:** [F5](foundations/f5-kubernetes.md) and [F6](foundations/f6-distributed-systems.md) sections 1–4. DDIA chapter 8.
- **Lab:** F5 hands-on with kind; F6 lab items 1–2.
- **Code:** skim `pkg/controller/deployment/rolling.go` in [Kubernetes](https://github.com/kubernetes/kubernetes) **(opt)**.
- **Check:** F5 and F6 questions.

### Day 13: the provisioning engine (the interview project), part 1

- **Learn:** [Module 8](08-provisioning-engine.md), all of it, slowly. This is the most important day.
- **Lab:** module 8 lab: deploy, roll, break a deploy three ways, roll back, kill the daemon mid-rollout.
- **Code:** read `internal/engine/` in this order: `types.go`, `queue.go`, `engine.go`, `sync.go`, `health.go`. Draw the deployment state machine and the architecture diagram from memory.
- **Check:** module 8 questions, then the 45-minute talk track (section 9) once, alone, out loud.

### Day 14: the provisioning engine, part 2 (ends early)

- **Learn:** `internal/engine/engine_test.go`: each test is a failure mode. For each one, predict what the engine does before reading the assertions.
- **Build:** [exercise 4](12-exercises.md) (exec health checks) or [exercise 5](12-exercises.md) (connection-aware draining), with tests. This is your first real feature in the engine.
- **Review (short) + catch-up.**

### Day 15: orchestration at scale

- **Learn:** [Module 9](09-scale.md), F6 sections 5–7. DDIA chapter 9 (consensus) sections on linearizability and Raft-style consensus. The Borg/Omega/Kubernetes paper **(opt)**.
- **Lab:** `rh schedule` experiments from module 9; F6 labs 3–4.
- **Build:** [exercise 10](12-exercises.md) (surge and unavailability budgets) or [exercise 9](12-exercises.md) (autoscaling).
- **Check:** module 9 questions.

### Day 16: build day

Pick one hard exercise and work on it all day: [exercise 6 (seccomp)](12-exercises.md) if you want kernel depth, or [exercise 13 (multi-node controller)](12-exercises.md) if you want control-plane depth. Do not expect to finish exercise 13 in a day; a working controller that places replicas on two agents is a great outcome. Write up what you built and what you would do next.

## Part 4: consolidation and interview (days 17–20)

### Day 17: security, real code, and a real app

- **Learn:** re-read module 4. Read about CVE-2019-5736 (runc) and the fix.
- **Lab:** [Module 11](11-capital-lab.md): run PostgreSQL on Roundhouse, and reproduce the recreate-vs-rolling lesson with a throwaway database (then read the fix and its test).
- **Code:** production reading from [module 10, section 2](10-interview-prep.md): runc's `rootfs_linux.go`, containerd's shim README, BuildKit's `convert.go`. For each, write three differences from Roundhouse.

### Day 18: gaps and depth

- Go through your notebook and flashcards. List every topic where you hesitated. Spend the day on the three weakest, using the modules' resources and the labs again.
- Finish or polish your day-16 build; make sure its tests pass with `go test -race`.

### Day 19: interview preparation

- **Learn:** [Module 10](10-interview-prep.md): the question bank. Answer every question aloud before reading the model answer; mark the ones you missed.
- **Practice:** mock design interview #1 (module 10, section 3), timed, recorded. Watch it back and write down three things to improve.
- Prepare your "something you built" story (section 4) and your behavioural stories (section 5).

### Day 20: final mock and wrap-up

- Mock design interview #2, with a friend if possible, including their follow-up questions.
- Redo the questions you missed on day 19.
- Final checklist (module 10, section 6). Redraw the README architecture diagram from memory one last time.
- Write a one-page summary of what you learned; it is your pre-interview refresher.

## If you have more than 20 days

Keep the same daily shape and continue with:

1. Tier 3 exercises in order: [12 (user namespaces), 13 (multi-node), 14 (cross-host networking), 15 (Firecracker runtime)](12-exercises.md).
2. _Designing Data-Intensive Applications_ cover to cover, and the Google SRE book chapters on overload and cascading failures.
3. _The Linux Programming Interface_ chapters on processes, signals, sockets and namespaces.
4. A real open-source contribution: pick a "good first issue" in runc, containerd, BuildKit or a CNI plugin. Reviewers there will teach you more than any book.

## If you have fewer days

The minimum path to interview readiness (about 10 days): days 1, 3, 5, 6, 8, 9, 13, 14, 15 and 19, reading modules 2, 6 and 7 as time allows.

## Daily coaching

Claude checks in twice a day (10:00 and 22:00 IST) to quiz you, unblock you and keep your record in [`progress/`](../progress/README.md): the day tracker, a spaced-review queue of weak topics, and interview readiness per area. See [the coaching routine](coach.md) for exactly how the check-ins work.

You can also ask Claude at any time to explain a line of Roundhouse code, review your exercise code, or run an extra mock interview.
