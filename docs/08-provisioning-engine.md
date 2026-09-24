# Module 8: The container provisioning engine

> **Goal:** be able to design, defend and extend a container provisioning engine in an interview, using Roundhouse's engine as a worked, tested example.

The prompt usually sounds like: _"Design the system that takes a user's service definition and keeps it running on our machines: start containers, deploy new versions without downtime, restart what crashes, and tell the user what is going on."_ Everything below is organized the way you would answer it: requirements, model, architecture, the core loop, failure modes, then scale.

## 1. Requirements (ask before you design)

Clarifying questions worth asking, with the answers Roundhouse assumes:

| Question                                   | Assumption                                                                              |
| ------------------------------------------ | --------------------------------------------------------------------------------------- |
| Long-running services, jobs, or both?      | Long-running services (persistent containers, like Railway), restart policy for crashes |
| One node or many?                          | One node per engine; a scheduler places across nodes (module 9)                         |
| What does "deployed" mean?                 | Every replica is running **and healthy**; only then does traffic move                   |
| What happens when a new version is broken? | The old version keeps serving; the new one is marked failed with a reason and logs      |
| Stateful services?                         | Allowed with a volume, limited to one replica (as Railway volumes are)                  |
| How do users see what is happening?        | An event timeline per deployment, merged logs, status per instance                      |
| Billing?                                   | Per-second CPU and memory usage from cgroups                                            |
| Scale targets?                             | Hundreds of services per node; reconcile latency under a second                         |

Non-functional requirements to state explicitly: **no downtime on deploy**, **no lost work on control-plane restart**, **idempotent API**, **bounded blast radius** (one bad service cannot starve the others), **observable** (every decision explains itself).

## 2. Data model

```text
Service "web"                      desired state, owned by the user
 └── Deployment rev 1..N           immutable: one per change (image, env, replicas, …)
      └── Instance (container)     observed state, discovered from the node
```

A **deployment is immutable**. Changing an env var creates rev 5; it never edits rev 4. This gives you history, one-click rollback (a new deployment with an old spec), and a clean state machine per version:

```text
            ┌─────────── newer deploy queued first ───────────► SKIPPED
            │
 QUEUED ──► DEPLOYING ──► all replicas healthy ──► ACTIVE ──► newer rev promoted ──► REMOVED
                │                                    │
                ├── pull fails / crashes > 3 /        └── restarts exhausted (crash loop) ──► CRASHED
                │   not healthy before timeout
                ▼
              FAILED  (the previous ACTIVE is untouched and keeps serving)
```

Per service there is at most **one ACTIVE** (serving) and **one target** (QUEUED/DEPLOYING). See [`types.go`](../internal/engine/types.go).

**Desired vs observed.** The engine persists only desired state (`state.json`: services and deployments). Instances are **not** stored; they are rediscovered every sync by listing containers with the labels `rh.service`, `rh.deployment` and `rh.replica`. This is the single most important design decision: the database can never disagree with reality about what is running, because it does not claim to know.

## 3. Architecture

```text
 HTTP API ──► Apply(spec) ──► state.json (desired) ──► workqueue.Add("web")
                                                          │
       resync ticker (1s) ────────────────────────────────┤  level-triggered safety net
       health prober (state changes) ─────────────────────┤
       timers (backoff, drain deadlines) ─────────────────┤
                                                          ▼
                                            N workers: sync("web")
                                                          │  observe: list containers by label
                                                          │  compare with desired
                                                          │  act: create/start/restart/stop/remove
                                                          ▼
                                                 Runtime interface ──► container manager ──► shims
                                                          │
                                  route(): edge proxy backends + private DNS answers
```

Components ([`internal/engine`](../internal/engine)):

- **API** ([`api.go`](../internal/engine/api.go)): `PUT /v1/services/{name}` is idempotent: an identical spec returns the existing deployment (200), a changed one creates a deployment (201). Clients and CI can retry blindly.
- **Work queue** ([`queue.go`](../internal/engine/queue.go)): a small client-go workqueue. A key is never processed by two workers at once; repeated adds coalesce; an add during processing re-queues the key afterwards. This gives per-service serialization with cross-service parallelism, without locks held across slow operations.
- **Reconciler** ([`sync.go`](../internal/engine/sync.go)): one function, `sync(service)`, that is safe to call at any time, any number of times.
- **Runtime interface** ([`runtime.go`](../internal/engine/runtime.go)): the seam between control logic and the node. The real implementation is the container manager; tests use a fake.
- **Prober** ([`health.go`](../internal/engine/health.go)), **meter** ([`metering.go`](../internal/engine/metering.go)), **event bus** ([`events.go`](../internal/engine/events.go)), **edge proxy and DNS** ([`sync.go` `route`](../internal/engine/sync.go), [`dns.go`](../internal/engine/dns.go)).

### Level-triggered, not edge-triggered

An edge-triggered design reacts to events: "container exited → restart it". Lose one event (crash, bug, restart) and the system is wrong forever. A level-triggered design periodically asks "given everything I can observe right now, what should be true, and what must I do to get there?". Events are only hints to look sooner. Roundhouse enqueues on API calls, health changes and timers, **and** every service every second regardless. A missed event costs at most one resync interval. This is the core idea of Kubernetes controllers, and the answer to half of all "what if…" follow-up questions.

## 4. The sync function

Pseudocode of [`sync`](../internal/engine/sync.go), about 60 lines of the real thing:

```text
sync(service):
  observed  = runtime.List() where label rh.service == service
  if service deleted: retire all observed; when none remain, forget the service; return

  mark older QUEUED/DEPLOYING deployments SKIPPED        # only the latest intent matters

  if target (QUEUED/DEPLOYING) exists:  rollout(target)
  if active exists:                     maintain(active)

  for containers of any other deployment:
    if within that deployment's drain window: requeue at the deadline
    else: retire (SIGTERM → grace → kill, then remove), in the background

  route(): edge proxy backends and DNS answers = healthy instances of ACTIVE
  return "look at me again in X" (backoff due, drain deadline, health poll)

rollout(t):
  QUEUED → DEPLOYING (record start time)
  ensure the image (pull; failure → FAILED "image pull failed: …")
  if now − start > deployTimeout → FAILED "not healthy within Ns"
  for each replica r in 0..replicas-1:
    none?    → create + start
    exited?  → count failures; > 3 → FAILED with the last output lines; else restart after backoff
  if every replica is healthy:
    t → ACTIVE; previous ACTIVE → REMOVED with drainUntil = now + drain
    (route() will now point at t; the old containers survive the drain window)

maintain(a):
  for each replica: missing → recreate; exited → restart policy:
    never → leave it;  on-failure and exit 0 → leave it
    restarts ≥ max and it died quickly → a becomes CRASHED (stop restarting)
    else restart after backoff(restarts): 1s, 2s, 4s … 30s
```

### Why promotion order gives zero downtime

1. New instances start **alongside** the old ones.
2. Traffic is not touched until **all** new replicas pass their health check.
3. Promotion flips `ACTIVE` in one state change; `route()` swaps the proxy's backend list atomically. New connections go to the new deployment. Existing connections are not interrupted.
4. Old instances leave the backend set immediately but keep running for `drainSeconds`, so in-flight requests finish.
5. Only then do they get `SIGTERM`, then `SIGKILL` after the grace period.

Swap steps 3 and 5 and you have downtime. Skip step 2 and you send traffic to a process that is still booting. And note what this ordering requires: **two versions running at once**. That is fine for stateless services and fatal for a database on a single volume, which is why services with a volume use a recreate strategy instead (stop old, then start new) and accept a few seconds of downtime. The integration test hammers the edge during a rollout and requires **zero** failed requests; locally it measured 1,617 requests, 0 failures across two back-to-back rollouts.

The application has a part to play too: it should stop accepting and finish in-flight requests on `SIGTERM`. `examples/hello` does. An app that ignores SIGTERM is killed after the grace period, which is safe but slower.

### Readiness vs liveness

Roundhouse's health checks are **readiness** checks: they gate promotion and routing. An instance that becomes unhealthy while ACTIVE is removed from routing (after three consecutive failures, to avoid flapping) but not restarted. Restarting on failed checks (**liveness**) is a separate, dangerous policy: when a shared dependency (the database) goes down, every replica fails its check at once, and a liveness policy restarts the whole fleet, turning a partial outage into a total one. Be ready to argue this.

## 5. Failure modes

The interviewer's favourite part. Each row is tested in [`engine_test.go`](../internal/engine/engine_test.go) or the [integration suite](../integration/integration_test.go) unless marked.

| What fails                                               | What happens                                                                                                                                                                                                                                                                                                                                                                        |
| -------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| New image does not exist                                 | Pull fails → FAILED with the registry's error; previous ACTIVE serves                                                                                                                                                                                                                                                                                                               |
| App crashes on boot                                      | Restarted with backoff; after >3 failures FAILED, last 20 log lines stored on the deployment                                                                                                                                                                                                                                                                                        |
| App boots but never passes its health check              | FAILED after `deployTimeoutSeconds`; previous ACTIVE serves                                                                                                                                                                                                                                                                                                                         |
| Three deploys pushed in quick succession                 | Only the newest rolls out; the others are SKIPPED                                                                                                                                                                                                                                                                                                                                   |
| ACTIVE replica crashes                                   | Restarted after backoff; routing drops it until healthy again                                                                                                                                                                                                                                                                                                                       |
| ACTIVE replica crash-loops                               | CRASHED after `maxRestarts` quick deaths; the exited container is kept for inspection                                                                                                                                                                                                                                                                                               |
| Someone `rh rm -f`s a replica                            | Next sync sees it missing and recreates it                                                                                                                                                                                                                                                                                                                                          |
| **The engine process dies mid-rollout**                  | Containers keep running (they belong to shims). On restart the engine reloads desired state, lists containers by label, adopts them (creates nothing), re-probes health, and continues the rollout from where observation says it is                                                                                                                                                |
| A shim is SIGKILLed                                      | The container's process dies with it or is orphaned; `List` reports "supervisor lost", and the restart policy replaces it                                                                                                                                                                                                                                                           |
| Container exits in the window before its shim records it | Reported as still running until the shim finishes writing, never as a fake exit (a real race found during development)                                                                                                                                                                                                                                                              |
| Host reboots                                             | Shims and containers are gone; overlay mounts are gone but upper dirs survive. The engine restarts containers; `launch` re-mounts the overlay. (Not in the automated tests.)                                                                                                                                                                                                        |
| Service with a volume is redeployed                      | **Recreate, not rolling:** every old instance is stopped and removed before the new one starts, because two processes on one data directory corrupt it. Brief downtime; if the new version fails, the old one is brought back. A real bug found while building this: an early version overlapped two Postgres instances and corrupted the database ([module 11](11-capital-lab.md)) |
| Public port already taken by another service             | Rejected at `Apply` time, before anything is created                                                                                                                                                                                                                                                                                                                                |
| Disk full                                                | Pull/unpack fails → deployment FAILED; blob writes are temp-file-plus-rename, so nothing half-written is ever used                                                                                                                                                                                                                                                                  |
| state.json corrupted                                     | Engine refuses to start (better than acting on garbage). Writes are atomic (temp + fsync + rename), so a crash cannot produce this; a disk failure could                                                                                                                                                                                                                            |

## 6. Observability

- **Events** are the user-facing story of a deployment: queued, deploying, pulling, replica started, healthy, live (with the time it took), draining, failed (why). `rh deploy` streams them; `GET /v1/events?follow=1` is NDJSON.
- **Logs** of all replicas are merged into one stream per service, following new instances as they appear (restarts, new deployments).
- **Metrics** in Prometheus format: deployments created/promoted/failed, restarts, probe results, reconcile time, work-queue depth, edge connections by result, per-container CPU and memory.
- **Failed deployments explain themselves** after their containers are gone: the reason and the last output lines are stored on the deployment.

## 7. Testing a control plane

The control logic is tested against [`fakeRuntime`](../internal/engine/engine_test.go): an in-memory node where tests crash containers, fail health checks and fail pulls on demand. 17 scenarios run in about 7 seconds under the race detector. The integration suite then checks that the real runtime behaves the way the fake assumes. Without this seam, every scenario would need root, a kernel and seconds of real time, and the rare interleavings would never be tested.

## 8. What production adds

Be ready to say what you would change before running this for paying users:

- **Durable, replicated desired state** (etcd, Postgres, FoundationDB) instead of a local JSON file, with optimistic concurrency (a version per service) so two control-plane replicas cannot overwrite each other.
- **Multi-node:** a scheduler and a per-node agent (module 9). The per-node part of Roundhouse's engine is roughly what an agent does.
- **Leases and fencing** so a partitioned node that "comes back" cannot resurrect instances that were already rescheduled elsewhere.
- **Surge and unavailability budgets** (`maxSurge`, `maxUnavailable`) for services too large to double during a deploy.
- **Pre-stop hooks** and connection-aware draining at the proxy (wait for active connections to reach zero, with a cap).
- **Admission control and quotas** per tenant, and priority classes for eviction under memory pressure.
- **A separate data path:** the edge proxy should not live in the control-plane process. In Roundhouse, restarting the daemon briefly interrupts public traffic (running containers and their private networking are unaffected).

## 9. A 45-minute talk track

1. (5 min) Clarify requirements, write the table in section 1.
2. (5 min) Data model: services, immutable deployments, instances; draw the state machine.
3. (10 min) Architecture: API → desired state → work queue → reconciler → node runtime → shims. Explain level-triggering and "observed state from labels".
4. (10 min) Walk a deploy through `rollout`, then a failed deploy, then a crash loop. Use the promotion-order argument for zero downtime.
5. (10 min) Failure modes table, especially "the control plane dies mid-rollout".
6. (5 min) Scale: many nodes, the scheduler, where the state lives, the edge.

Practise it against the [interview prep](10-interview-prep.md) questions.

## Lab 8

```sh
sudo -E rh daemon --http 127.0.0.1:7070 &
sudo -E rh deploy -f examples/web.json                 # watch the timeline
sudo -E rh deploy -f examples/web.json -e VERSION=v2   # rolling update
sudo -E rh deploy -f examples/web.json -e VERSION=v3 --cmd 'sh -c exit${IFS}3'   # crash on boot → FAILED
sudo -E rh deploy -f examples/web.json -e VERSION=v4 --health /nope --timeout 10 # never healthy → FAILED
sudo -E rh svc status web                              # history and reasons
sudo -E rh svc rollback web                            # new rev with an old spec
# Kill the control plane mid-rollout and bring it back:
sudo -E rh deploy -f examples/web.json -e VERSION=v5 --wait=false; sudo pkill -x rh
sudo -E rh daemon --http 127.0.0.1:7070 &
sudo -E rh svc status web                              # nothing was recreated; the rollout finished
curl -s localhost:7070/metrics | grep '^rh_'
```

## Check yourself

1. Why are deployments immutable, and what does that buy you?
2. Why does the engine not store instances?
3. Explain why the promotion order gives zero downtime, and one way to break it.
4. The engine crashes after starting two of three new replicas. Walk through the restart.
5. Readiness or liveness for a platform's default health check? Defend your answer.
