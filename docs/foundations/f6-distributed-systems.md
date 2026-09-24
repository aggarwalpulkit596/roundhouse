# F6: Distributed systems for control planes

> **Goal:** the distributed-systems ideas that come up whenever a control plane spans more than one machine: failure detection, consensus, leases and fencing, idempotency, consistency, and backpressure. Days 12 and 15 of the [20-day plan](../00-study-plan.md), then again during interview prep.

A single-node engine can crash and recover (module 8). A fleet of nodes adds partial failure: some things fail while others keep going, and you cannot tell "slow" from "dead". Senior infrastructure interviews always go here.

## 1. You cannot tell slow from dead

A node stops answering heartbeats. It may have crashed, or the network between you may be partitioned, or it may be paused in a long garbage collection. **You cannot know.** Everything below exists because of this fact.

- **Heartbeats and timeouts** turn silence into a decision ("presumed dead after 40s"). Too short: false positives and churn. Too long: slow recovery.
- The presumed-dead node may still be running your workload. If you reschedule it elsewhere, you now have **two copies**. For a stateless web server that is harmless. For anything holding a volume or a lock, it is the database corruption you saw in [module 11](../11-capital-lab.md), across machines.

## 2. Leases and fencing

- A **lease** is a lock with an expiry: "node-14 owns replica 3 until 12:00:40". The owner must renew it; if it cannot reach the lease store, it must **stop acting** when the lease expires, even though nobody told it to.
- **Fencing tokens:** every new lease grant carries a higher number. Storage (or the next component) rejects writes carrying an older number, so a node that wakes up from a long pause with a stale lease is rejected instead of trusted. (Martin Kleppmann's "How to do distributed locking" explains why locks without fencing are unsafe.)

## 3. Consensus: agreeing on one truth

Desired state (what should run where) must survive machine failures and not diverge. **Consensus** algorithms (Raft, Paxos) let a group of machines agree on an ordered log of changes as long as a **majority** is up: 3 nodes tolerate 1 failure, 5 tolerate 2. etcd (Kubernetes' store) and Consul use Raft.

You will not implement Raft in an interview. You should be able to say:

- A leader is elected; writes go through it and are committed once a majority acknowledges.
- A minority partition cannot make progress (it cannot elect a leader), which is exactly what prevents split-brain.
- Reads can be stale unless they go through the leader or use a read-index/lease.

Play with the [Raft visualization](https://raft.github.io/) for half an hour; it sticks.

## 4. Idempotency and retries

Networks lose responses, not just requests. The client cannot tell whether its request was applied, so it retries, so every mutating operation must be **safe to repeat**:

- **Idempotent APIs:** `PUT /services/web` with the full spec (Roundhouse returns the existing deployment for an identical spec), or an **idempotency key** on `POST`.
- **At-least-once delivery + idempotent processing = effectively once.** That is how Capital Lab's payment flow and Roundhouse's usage metering avoid double charging.
- **Retries need backoff and jitter**, or all clients retry at the same moment and overload the recovering server (a "retry storm").

## 5. Level-triggered reconciliation (again)

The most important control-plane idea, repeated because it is the answer to so many questions: store **desired** state durably, **observe** actual state from the source of truth (the nodes), and continually **reconcile** the difference. Missed events, crashes and restarts then cost time, not correctness. [Module 8](../08-provisioning-engine.md), section 3.

## 6. Consistency and caches

- **Strong consistency:** every read sees the latest write (at a cost in latency and availability).
- **Eventual consistency:** replicas converge if writes stop. DNS is the classic example: a changed record reaches everyone eventually, bounded (roughly) by its TTL.
- **Read-your-writes** and **monotonic reads** are useful middle grounds for user-facing APIs ("I just deployed; show me my deployment").
- **CAP**, in one honest sentence: during a network partition you must choose between answering (possibly stale or divergent) and staying consistent (refusing some requests).

## 7. Backpressure and overload

A system that accepts more work than it can do falls over. Bounded queues, rate limits, load shedding (reject early with 429/503), and concurrency limits (like the pull worker pool and the work queue) keep it upright. For a platform, the classic overload is the **thundering herd**: a base image changes and 50,000 services redeploy at once. Answers: rate-limit rollouts globally, stagger by jitter, prioritize.

## Lab F6

1. Spend 30 minutes with the [Raft visualization](https://raft.github.io/): kill the leader, partition the cluster, and watch what happens to writes on each side.
2. In Roundhouse, send the same `rh deploy -f examples/web.json` three times in a row. How many deployments are created? Why?
3. Kill `rh daemon` in the middle of a rollout, restart it, and write down exactly why nothing is lost ([module 8](../08-provisioning-engine.md), "Failure modes").
4. Design on paper: two `rh daemon` processes manage the same node by accident. What breaks first? How would a lease prevent it?

## Resources

- Martin Kleppmann, _Designing Data-Intensive Applications_: chapters 5 (replication), 8 (the trouble with distributed systems) and 9 (consistency and consensus). The most useful book for this role after your Linux books.
- Google, [_Site Reliability Engineering_](https://sre.google/sre-book/table-of-contents/) (free online): chapters on load balancing, handling overload, cascading failures and distributed consensus.
- Diego Ongaro and John Ousterhout, [_In Search of an Understandable Consensus Algorithm_](https://raft.github.io/raft.pdf) (the Raft paper; readable).
- Martin Kleppmann, [_How to do distributed locking_](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html).

## Check yourself

1. A node misses heartbeats for 60 seconds. What do you do with its workloads, and what can go wrong?
2. What is a fencing token, and what failure does it prevent that a plain lease does not?
3. Why can a 3-node Raft cluster survive one failure but not two? What happens to a 2-node minority during a partition?
4. Make `POST /deployments` safe to retry. Give two designs.
5. A popular base image is patched, and 50,000 services want to redeploy. Design the rollout.
