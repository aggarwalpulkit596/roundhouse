# Module 5: Networking

> **Goal:** trace a packet from a container to the internet, from a user to a container, and from one service to another by name, and know what each hop costs.

## The building blocks

| Primitive         | What it is                                                                                  |
| ----------------- | ------------------------------------------------------------------------------------------- |
| Network namespace | A complete, separate network stack: interfaces, routes, iptables rules, sockets, port space |
| veth pair         | A virtual cable: two interfaces; a packet sent into one comes out of the other              |
| Bridge            | A virtual L2 switch in the host. Interfaces "plugged in" can talk to each other             |
| IPAM              | Something that hands out addresses without collisions                                       |
| NAT (MASQUERADE)  | Rewrites container source addresses to the host's on the way out, and back on the way in    |
| conntrack         | The kernel table that remembers each NATed flow so replies can be translated back           |

Roundhouse's default "bridge" network is the same topology as Docker's `docker0` and the CNI `bridge` plugin:

```text
            host network namespace                               container netns
┌───────────────────────────────────────────────┐          ┌──────────────────────┐
│ eth0 (uplink) ◄── iptables nat POSTROUTING    │          │                      │
│                   -s 10.88.0.0/16 ! -o rh0    │          │  eth0 10.88.0.5/16   │
│                   -j MASQUERADE               │  veth    │  lo   127.0.0.1      │
│ rh0 (bridge) 10.88.0.1/16 ──── rh-<id> ═══════╪══════════╪═ (peer renamed eth0) │
│      ▲                                        │          │  default via 10.88.0.1│
│      └── private DNS :53, edge proxy dials here │        └──────────────────────┘
└───────────────────────────────────────────────┘
```

## How Roundhouse wires a container ([`internal/network/network.go`](../internal/network/network.go))

1. **`Setup`** (idempotent): create bridge `rh0`, give it `10.88.0.1/16`, bring it up, set `net.ipv4.ip_forward=1`, and install three iptables rules (MASQUERADE for egress, FORWARD accept for the bridge, FORWARD accept for established replies).
2. **`Allocate`**: take the next free address from a JSON file under `flock`. It continues _after_ the last allocation instead of reusing the lowest free address. A just-released IP may still sit in other containers' ARP caches or in conntrack entries, and handing it out immediately is how traffic ends up at the wrong container.
3. **`Attach`**, called by the runtime's `BeforeRelease` hook while the container's init is still blocked:
   - create a veth pair `rh-<id>` / `rhp<id>` with the host end enslaved to the bridge;
   - move the peer into the network namespace of the init PID (`LinkSetNsPid`). It disappears from the host;
   - open a netlink socket **inside** that namespace (`netlink.NewHandleAt`) and there rename the peer to `eth0`, assign the address, bring up `eth0` and `lo`, and add a default route via the bridge.
4. **`Detach`/`Release`** on removal. When a network namespace dies (its last process exits), the kernel deletes the veth pair automatically.

Everything uses the netlink API through `vishvananda/netlink` (the library CNI plugins use) rather than shelling out to `ip`, which is not even installed on every host.

**Why configure from the parent, before the app starts?** So the app never sees a half-configured network: a process that checks connectivity at boot would otherwise race the runtime.

## Getting traffic in: publishing ports

There are two ways to get a host port to a container port:

| Approach                                                        | How                                                        | Pros                                                                                          | Cons                                                                                   |
| --------------------------------------------------------------- | ---------------------------------------------------------- | --------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| **iptables DNAT** (kube-proxy iptables mode, Docker by default) | `PREROUTING -p tcp --dport 8080 -j DNAT --to 10.88.0.5:80` | kernel fast path, preserves the client IP                                                     | `localhost` needs extra rules (`route_localnet`), hard to observe, rule churn at scale |
| **Userland proxy** (docker-proxy, Roundhouse)                   | accept on the host, dial the container, copy bytes         | trivial to reason about, works for localhost, per-connection metrics, easy to switch backends | an extra copy and a goroutine per connection; the app sees the proxy's IP              |

Roundhouse uses a userland proxy ([`proxy.go`](../internal/network/proxy.go)) because the same component doubles as the **edge router**: its backend list is an atomic pointer, and replacing it is how a deploy moves traffic. Existing connections keep their backend; only new connections see the new set. That is "drain" at L4. At scale, platforms do this in an edge proxy fleet (Envoy, their own Rust/Go proxies) or with eBPF in the kernel, not in each host daemon.

Half-close matters: an HTTP/1.0 client may send its request and `shutdown(SHUT_WR)` while still waiting for the response. [`Splice`](../internal/network/proxy.go) propagates `CloseWrite` in each direction instead of closing both ends, and [`TestProxySwapsBackendsAtomically`](../internal/network/network_test.go) checks it.

## Service discovery: private DNS

Services must find each other while their instances change IPs on every deploy. The engine runs a tiny DNS server on the bridge gateway ([`internal/engine/dns.go`](../internal/engine/dns.go)) and writes `nameserver 10.88.0.1` into each deployed container's `resolv.conf`:

- `web.rh.internal` → A records for the **healthy instances of the active deployment** (all running ones if none is healthy yet: an unready answer beats NXDOMAIN).
- Any other name → forwarded to the host's upstream resolvers.
- TTL 5 seconds, because the answer changes on every deploy.

It parses the DNS wire format by hand (about 150 lines), which is a good way to learn it: a 12-byte header, a question with length-prefixed labels, answers that point back to the question name with a compression pointer (`0xc00c`).

DNS-based discovery has well-known weaknesses you should raise yourself in an interview: clients cache beyond the TTL (the JVM historically cached forever), and "DNS round-robin" is not load balancing. The alternatives are a virtual IP per service (kube-proxy's ClusterIP) or client-side load balancing via a control-plane API (gRPC xDS).

**How Railway does it.** Railway's private networking is a WireGuard mesh between hosts with eBPF programs that translate between a per-environment IPv6 private range and the backbone, firewall endpoints, and hijack DNS lookups to their internal resolver ([How private networking works](https://docs.railway.com/networking/private-networking/how-it-works)). The ideas map directly onto Roundhouse: a private address space per project, a name per service (`<service>.railway.internal`), and a resolver the platform controls. The difference is that Railway's works across hosts and regions. Roundhouse's bridge only spans one host (see [module 9](09-scale.md) for the multi-host version).

## Lab 5: build the network by hand, then compare

```sh
# Two namespaces, a bridge, NAT: Roundhouse's network in ~15 commands.
sudo ip netns add a && sudo ip netns add b
sudo ip link add br-lab type bridge && sudo ip addr add 10.99.0.1/24 dev br-lab && sudo ip link set br-lab up
for n in a b; do
  sudo ip link add veth-$n type veth peer name eth0 netns $n
  sudo ip link set veth-$n master br-lab up
  sudo ip -n $n link set lo up && sudo ip -n $n link set eth0 up
done
sudo ip -n a addr add 10.99.0.2/24 dev eth0 && sudo ip -n b addr add 10.99.0.3/24 dev eth0
sudo ip -n a route add default via 10.99.0.1 && sudo ip -n b route add default via 10.99.0.1
sudo ip netns exec a ping -c1 10.99.0.3                     # L2 through the bridge
sudo iptables -t nat -A POSTROUTING -s 10.99.0.0/24 ! -o br-lab -j MASQUERADE
sudo sysctl -w net.ipv4.ip_forward=1
sudo ip netns exec a ping -c1 1.1.1.1                       # out through NAT
sudo conntrack -L 2>/dev/null | grep 10.99.0.2              # the NAT flow the kernel remembers
# Clean up
sudo ip netns del a; sudo ip netns del b; sudo ip link del br-lab
sudo iptables -t nat -D POSTROUTING -s 10.99.0.0/24 ! -o br-lab -j MASQUERADE

# Now look at what Roundhouse created:
sudo -E rh run -d --name n1 mirror.gcr.io/library/busybox:latest httpd -f -p 80
ip link show master rh0
sudo iptables -t nat -S POSTROUTING | grep 10.88
cat $RH_ROOT/network/ipam.json
sudo -E rh exec n1 ip route
sudo -E rh rm -f n1

# Private DNS while a service rolls
sudo -E rh daemon &
sudo -E rh deploy -f examples/web.json
sudo -E rh run --rm --dns 10.88.0.1 mirror.gcr.io/library/busybox:latest nslookup web.rh.internal 10.88.0.1
```

## Check yourself

1. Trace an outbound TCP SYN from a container to `1.1.1.1` and its SYN-ACK back: every interface and table it passes.
2. What breaks if two containers are given the same IP? How does IPAM prevent it across concurrent `rh run`s?
3. Userland proxy or DNAT for a platform's edge? Argue both sides.
4. Why is DNS round-robin not load balancing? What do you do about clients that cache DNS forever?
5. Extend this design to many hosts. What changes about addressing, routing and discovery? (Hints: an overlay such as VXLAN or WireGuard, a routed /24 per host, a control plane that distributes endpoints.)
