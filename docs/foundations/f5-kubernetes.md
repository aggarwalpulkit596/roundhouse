# F5: Kubernetes in two days

> **Goal (day 12 of the [20-day plan](../00-study-plan.md)):** know Kubernetes well enough to use its vocabulary in an interview, compare it with the engine you are studying, and argue when a platform should or should not build on it. You are not trying to become a Kubernetes administrator.

Infrastructure interviews use Kubernetes terms as shared language ("like a readiness probe", "like the scheduler's filter and score phases") even at companies that do not run Kubernetes for customer workloads. You need the concepts, a few hours of hands-on use, and the architecture.

## 1. The objects you must know

| Object                  | What it is                                                                  | Roundhouse equivalent                      |
| ----------------------- | --------------------------------------------------------------------------- | ------------------------------------------ |
| Pod                     | One or more containers sharing a network namespace (and optionally volumes) | an instance (one container)                |
| Deployment              | Desired state for a stateless app: image, replicas, rollout strategy        | a service + its deployments                |
| ReplicaSet              | One version of a Deployment's pods (a new one per rollout)                  | a deployment revision                      |
| Service                 | A stable virtual IP and DNS name that load-balances to matching pods        | private DNS + edge proxy                   |
| Ingress / Gateway       | HTTP routing from outside into Services                                     | edge proxy (`publicPort`)                  |
| ConfigMap / Secret      | Configuration and secrets injected as env vars or files                     | `env` (with `${{svc.VAR}}` references)     |
| PersistentVolume(Claim) | Storage that outlives pods                                                  | `volume`                                   |
| StatefulSet             | Pods with stable identity and storage (databases)                           | a service with a volume (recreate deploys) |
| Job / CronJob           | Run to completion, once or on a schedule                                    | exercise 11                                |
| Node                    | A machine running the kubelet                                               | a machine running `rh daemon`              |
| Namespace (k8s)         | A name scope for objects (not a Linux namespace!)                           | none                                       |

Probes: **readiness** (may this pod get traffic?), **liveness** (should it be restarted?), **startup** (is it still booting?). Module 8 explains why Roundhouse only does readiness.

Resources: **requests** (reserved; used for scheduling; map to cgroup CPU weight) and **limits** (enforced ceilings; map to `cpu.max` and `memory.max`).

## 2. The architecture

```text
kubectl ──► kube-apiserver ──► etcd (all desired and observed state, Raft-replicated)
                 ▲  ▲  ▲
                 │  │  └─ controllers (kube-controller-manager): Deployment, ReplicaSet, Node, …
                 │  │       each a level-triggered loop: watch → compare → act
                 │  └──── kube-scheduler: picks a node for unscheduled pods (filter, then score)
                 │
      kubelet (on every node) ── CRI ──► containerd ──► shim ──► runc
                                 CNI ──► network plugin (bridge, Calico, Cilium…)
      kube-proxy (on every node): Service virtual IPs → iptables/IPVS/eBPF rules
```

Everything is a **controller reconciling desired state**, which is exactly the pattern in [`internal/engine/sync.go`](../../internal/engine/sync.go). The kubelet is the node agent, like the per-node part of Roundhouse's engine.

## 3. Hands-on (half a day)

Use [kind](https://kind.sigs.k8s.io/) (Kubernetes in Docker) on your lab machine:

```sh
go install sigs.k8s.io/kind@latest && kind create cluster
curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/$(dpkg --print-architecture)/kubectl" && sudo install kubectl /usr/local/bin/

kubectl create deployment web --image=nginx --replicas=3
kubectl expose deployment web --port=80
kubectl get pods -o wide
kubectl run -it --rm client --image=busybox -- wget -qO- http://web     # Service DNS
kubectl set image deployment/web nginx=nginx:alpine                      # rolling update
kubectl rollout status deployment/web && kubectl rollout history deployment/web
kubectl rollout undo deployment/web                                      # rollback
kubectl get rs                                                           # a ReplicaSet per version
kubectl delete pod -l app=web --wait=false; kubectl get pods -w          # self-healing
```

Then compare each step with Roundhouse's `rh deploy`, `rh svc status`, `rh svc rollback` and private DNS.

## 4. For depth (optional, one extra day)

Kelsey Hightower's [_Kubernetes The Hard Way_](https://github.com/kelseyhightower/kubernetes-the-hard-way) builds a cluster component by component. It is the Kubernetes equivalent of what Roundhouse does for containers.

## Resources

- [Kubernetes Basics tutorial](https://kubernetes.io/docs/tutorials/kubernetes-basics/) and the [Concepts](https://kubernetes.io/docs/concepts/) pages for Pods, Deployments, Services and probes.
- Brendan Burns, Joe Beda, Kelsey Hightower and Lachlan Evenson, _Kubernetes: Up and Running_.
- The [controller pattern](https://kubernetes.io/docs/concepts/architecture/controller/) page (short and essential).

## Check yourself

1. Walk through what happens after `kubectl apply` of a Deployment until containers run, naming every component.
2. What is the difference between a Deployment and a ReplicaSet, and why does a rollout create a new ReplicaSet?
3. Requests vs limits: which ones does the scheduler use, and which one does the kernel enforce?
4. Readiness vs liveness: which one can turn a database outage into a total outage?
5. Give two reasons a PaaS might not run customer workloads on Kubernetes, and two reasons it might.
