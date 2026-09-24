# Module 11: Capital Lab on Roundhouse

> **Goal:** run a real application's infrastructure on the platform you built, and learn from what breaks.

Capital Lab (the parent repository) is a Kotlin/JVM app backed by PostgreSQL. Its README tells you to start the database with `docker compose up -d --wait postgres`. This module replaces Docker with Roundhouse, and ties Roundhouse's usage metering back to Capital Lab's domain: both are about recording money-relevant events exactly once.

## 1. PostgreSQL as a Roundhouse service

[`examples/capital-lab/postgres.json`](../examples/capital-lab/postgres.json) is the Compose service translated into a Roundhouse spec:

| compose.yaml                         | postgres.json                                         | What Roundhouse does with it                                                          |
| ------------------------------------ | ----------------------------------------------------- | ------------------------------------------------------------------------------------- |
| `image: postgres:17-alpine`          | `"image": "mirror.gcr.io/library/postgres:17-alpine"` | pulls via Google's Docker Hub mirror                                                  |
| `environment: POSTGRES_*`            | `"env": {…}`                                          | injected, plus `PORT` and `RH_*`                                                      |
| `ports: 127.0.0.1:55432:5432`        | `"port": 5432, "publicPort": 55432`                   | edge proxy on :55432 → the healthy instance                                           |
| `volumes: capital_lab_pg:/var/lib/…` | `"volume": {"name": "capital-lab-pg", …}`             | bind mount of `RH_ROOT/volumes/capital-lab-pg`; forces 1 replica and recreate deploys |
| `healthcheck: pg_isready`            | `"healthcheck": {"type": "tcp"}`                      | TCP probe every 2s gates promotion and routing                                        |

```sh
sudo -E rh daemon &
sudo -E rh deploy -f examples/capital-lab/postgres.json
#  … rev 1 is live (1/1 healthy) … listening on :55432

# From another container, over private DNS:
sudo -E rh run --rm --dns 10.88.0.1 -e PGPASSWORD=local_demo_only mirror.gcr.io/library/postgres:17-alpine \
  psql -h capital-lab-db.rh.internal -U capital_lab -d capital_lab -c 'select version();'

# From the host, through the edge proxy, exactly where Capital Lab expects it:
cd ..            # the capital-lab repository root
./gradlew dashboard      # LAB_JDBC_URL defaults to jdbc:postgresql://127.0.0.1:55432/capital_lab
```

Capital Lab's integration tests and dashboard need JDK 17. If your machine has none, run Gradle in a Roundhouse container with host networking and the repository mounted:

```sh
sudo -E rh run --rm --net host -v "$PWD":/src -v "$HOME/.gradle":/root/.gradle -w /src \
  mirror.gcr.io/library/eclipse-temurin:17-jdk ./gradlew --no-daemon paymentDemo
```

When this module was written, that command ran JDK 17 and Gradle inside Roundhouse correctly, but the sandbox's shared IP was being rate-limited by Maven Central (HTTP 429), so the dependency download could not finish there. On a normal connection it completes. The database part was verified end to end: tables written over private DNS, read back through the edge proxy, and preserved across redeploys.

## 2. The bug this found: two databases, one volume

The first version of the engine rolled out **every** service the same way: start the new version, wait until healthy, switch traffic, drain the old. For the stateless web examples that is exactly right. For Postgres, "start the new version alongside the old" means two `postgres` processes writing the same data directory.

It went unnoticed at first, because the new instance came up healthy: Postgres guards its data directory with `postmaster.pid`, but the PID written there belongs to another PID namespace, so the new instance concluded the lock was stale and started anyway. The next redeploy then failed with:

```text
LOG:  record with incorrect prev-link 2DB0000/0 at 0/1998540
LOG:  invalid checkpoint record
PANIC:  could not locate a valid checkpoint record at 0/1998540
```

The overlap had corrupted the write-ahead log. (The fixed engine still did the right thing with the broken data: it stopped the old instance first, failed the new deployment, and stored those log lines as the deployment's reason.)

The fix ([`sync.go`](../internal/engine/sync.go)): a service with a volume is **recreated, not rolled**. Every older instance is stopped and removed before the new one starts, maintenance of the old deployment is paused while that happens, and if the new deployment fails, the old one is recreated automatically. [`TestVolumeServiceIsRecreatedNeverOverlapped`](../internal/engine/engine_test.go) records the peak number of concurrently running instances and requires it to be exactly one, and checks the automatic fallback. After the fix: 1,000 rows, two redeploys, zero corruption.

Lessons worth telling in an interview:

- **Zero-downtime deploys assume two versions can run at once.** That assumption is false for anything with exclusive state. Platforms either accept downtime for volume-backed services (Railway volumes attach to a single replica) or use storage that supports handover (replicated databases with failover, network volumes with fencing).
- **Application-level locks do not survive namespace boundaries.** PID files, `flock` on a network filesystem, "is this port in use": each is scoped to something the container changes.
- **A test that measures the invariant** (peak concurrency on a volume) beats a test that checks the happy path.

## 3. Metering is a ledger problem

Capital Lab models money moving through a lifecycle with balanced journal entries, idempotency keys and reconciliation. Roundhouse's usage meter is the upstream half of a similar system: every sample is a money-relevant event.

| Capital Lab concept                    | Roundhouse metering equivalent                                                  |
| -------------------------------------- | ------------------------------------------------------------------------------- |
| Idempotent payment request             | A usage sample keyed by (container, sample time); re-sending cannot double bill |
| Unknown bank outcome, query to recover | Agent restart: re-prime counters rather than guess the missed interval          |
| Balanced journal                       | CPU-seconds and GB-seconds per service must equal the sum over its containers   |
| Reconciliation against the bank        | Reconciling metered usage against the cgroup's own cumulative counters          |

A good extension that joins the two projects: export Roundhouse's samples as usage events, and post them into a Capital Lab–style ledger as balanced entries (customer receivable against usage revenue), with reconciliation that flags gaps (missed samples) and overlaps (double ingestion).

## 4. Exercises

1. **Run the dashboard itself on Roundhouse.** It binds `127.0.0.1` and allows only `localhost` Host headers (DNS-rebinding protection), so the edge proxy cannot reach it on the bridge. Add an opt-in `LAB_HTTP_BIND` setting to Capital Lab that keeps the Host allowlist, write a Railfile that copies `build/install` into an `eclipse-temurin:17-jre` image, and deploy it with `LAB_JDBC_URL=jdbc:postgresql://${{capital-lab-db.RH_PRIVATE_DOMAIN}}:5432/capital_lab`.
2. **A real readiness check for Postgres.** TCP-open is not "ready to serve queries". Add an `exec` health check type that runs `pg_isready` inside the container (you have `rh exec`'s machinery).
3. **Backups.** Add `rh volume snapshot NAME` that stops writes safely (or uses `pg_basebackup` through the service), then tars the volume with a checksum.
4. **The ledger bridge** from section 3.
