# F2: Networking fundamentals

> **Goal:** enough TCP/IP, DNS and HTTP to understand how packets reach a container, how services find each other, and what a load balancer does. Day 3 of the [20-day plan](../00-study-plan.md).

In Docker Compose, services reach each other by name and `ports: "8080:80"` makes one reachable from your browser. This module explains the machinery under both.

## 1. The layers that matter

| Layer            | Unit             | Addresses               | Examples                                |
| ---------------- | ---------------- | ----------------------- | --------------------------------------- |
| Link (L2)        | frame            | MAC `02:42:ac:11:00:02` | Ethernet, Wi-Fi, a Linux bridge, veth   |
| Network (L3)     | packet           | IP `10.88.0.5`          | IPv4, IPv6, routing, NAT                |
| Transport (L4)   | segment/datagram | port `:8080`            | TCP (reliable stream), UDP (datagrams)  |
| Application (L7) | message          | URL, hostname           | HTTP, DNS, TLS, gRPC, Postgres protocol |

"L4 load balancer" means it routes by IP and port without looking inside; "L7" means it reads HTTP (paths, headers).

## 2. IP addresses, subnets and routing

- An IPv4 address is 32 bits. A **subnet** in CIDR notation, `10.88.0.0/16`, means "the first 16 bits are fixed": 65,536 addresses from 10.88.0.0 to 10.88.255.255.
- **Private ranges** (not routed on the internet): `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`. Docker uses `172.17.0.0/16`; Roundhouse uses `10.88.0.0/16`.
- Every host has a **routing table**: for each destination, which interface to send through and, if not local, which **gateway** (next router) to hand the packet to. The **default route** (`0.0.0.0/0`) catches everything else.

```sh
ip addr            # your interfaces and their addresses
ip route           # the routing table: look for "default via …"
ip route get 1.1.1.1   # which route a packet to 1.1.1.1 would use
```

## 3. Switching, bridges and ARP

Within one L2 network, hosts find each other's MAC addresses with **ARP** ("who has 10.88.0.5? tell 10.88.0.1"). A **switch** forwards frames to the right port by MAC. A Linux **bridge** is a software switch inside the kernel. Docker's `docker0` and Roundhouse's `rh0` are bridges; each container is plugged into one via a **veth pair** (a virtual cable). See [module 5](../05-networking.md).

```sh
ip neigh          # the ARP (neighbour) cache
bridge link       # interfaces attached to bridges (after running a container)
```

## 4. NAT

Private addresses cannot go out to the internet as they are. **NAT** (network address translation) rewrites them:

- **Source NAT / masquerade:** a container's outbound packet leaves with the **host's** address as its source; the kernel remembers the mapping in the **conntrack** table and rewrites the replies back. This is how containers reach the internet.
- **Destination NAT / port forwarding:** a packet arriving at `host:8080` is rewritten to `10.88.0.5:80`. This is one way to implement `ports: "8080:80"`. (The other is a proxy program; module 5 compares them.)

On Linux, NAT is configured with **iptables** (or its successor **nftables**) in the `nat` table.

```sh
sudo iptables -t nat -S          # NAT rules (Docker adds several)
sudo conntrack -L | head         # tracked connections
```

## 5. TCP and UDP

**TCP** gives an ordered, reliable byte stream over an unreliable network:

- **Three-way handshake** to open: SYN → SYN-ACK → ACK.
- Every byte is acknowledged; losses are retransmitted.
- **Four-way close** (FIN/ACK each way). Either side can close its sending half while still receiving: **half-close**. Proxies must handle it (Roundhouse's does).
- A **socket** is identified by (source IP, source port, destination IP, destination port). A server **listens** on a port; each client connection gets its own socket.

**UDP** sends independent datagrams: no connection, no ordering, no retries. DNS uses UDP by default.

```sh
ss -tlnp               # listening TCP sockets and their processes
ss -tnp                # established connections
nc -l 9000 &  echo hello | nc -q1 127.0.0.1 9000     # a TCP server and client
sudo tcpdump -i lo -n port 9000                      # watch the handshake (in another terminal)
```

## 6. DNS

**DNS** turns names into addresses. Your machine asks a **resolver** (listed in `/etc/resolv.conf`), which walks from the root servers down to the authoritative server for the name, and caches the answer for its **TTL** (time to live).

- **Record types:** `A` (IPv4), `AAAA` (IPv6), `CNAME` (alias), `SRV`, `TXT`.
- **Service discovery:** Docker Compose runs an embedded DNS server so `db` resolves to the database container's IP. Kubernetes answers `db.default.svc.cluster.local`. Roundhouse answers `db.rh.internal` ([`internal/engine/dns.go`](../../internal/engine/dns.go)). Railway answers `db.railway.internal`.
- **Caching problems:** clients may cache longer than the TTL, so "DNS load balancing" is imperfect.

```sh
cat /etc/resolv.conf
dig example.com                 # full answer with TTL
dig +trace example.com          # the walk from the root
dig @1.1.1.1 example.com AAAA
```

## 7. HTTP, TLS and proxies

- **HTTP** is a request/response protocol over TCP: method, path, headers, body; status codes (2xx success, 3xx redirect, 4xx client error, 5xx server error).
- **HTTP/1.1 keep-alive** reuses one TCP connection for many requests. **HTTP/2** multiplexes many requests on one connection.
- **TLS** encrypts the TCP stream and proves the server's identity with a certificate. **TLS termination** means a proxy decrypts traffic before forwarding it.
- A **reverse proxy / load balancer** (nginx, Envoy, HAProxy, Railway's edge) accepts client connections and forwards them to one of several **backends**. **Health checks** decide which backends are eligible. **Connection draining** lets in-flight requests finish before a backend is removed. That is the heart of zero-downtime deploys ([module 8](../08-provisioning-engine.md)).

```sh
curl -v https://example.com 2>&1 | head -30     # see the TLS handshake and headers
curl -sI https://example.com                    # headers only
```

## Lab F2

1. Draw your machine's network: interfaces, addresses, default gateway. Then run `docker run -d -p 8080:80 nginx` and redraw it: what appeared (`ip link`, `ip addr`, `sudo iptables -t nat -S`)?
2. Capture a TCP handshake and an HTTP request with `sudo tcpdump -i any -n port 8080` while running `curl localhost:8080`.
3. With Docker Compose, run two services and resolve one from the other (`docker compose exec web getent hosts db`). Then find the DNS server Docker gave the container (`cat /etc/resolv.conf` inside it).
4. Use `dig` to find the TTL of a popular site, query it twice, and watch the TTL count down (cached).
5. Write down, in order, everything that happens between typing `curl https://example.com` and seeing the HTML. Then check it with `curl -v`.

## Resources

- Julia Evans, _Networking! ACK!_ and _How DNS Works_ ([wizardzines.com](https://wizardzines.com/)).
- Ilya Grigorik, [_High Performance Browser Networking_](https://hpbn.co/) (free online): TCP, TLS and HTTP/2 chapters.
- Brian "Beej" Hall, [_Beej's Guide to Network Programming_](https://beej.us/guide/bgnet/): sockets from a programmer's view.
- Kurose and Ross, _Computer Networking: A Top-Down Approach_, when you want the full textbook.

## Check yourself

1. How many addresses are in a /24? Is `10.88.3.7` in `10.88.0.0/16`?
2. How does a container with a private IP get a reply from a server on the internet? Name the two kernel features involved.
3. What is TCP half-close, and why must a proxy care?
4. How does `db` resolve to an IP inside Docker Compose? What could go wrong when `db` is replaced by a new container?
5. What is the difference between an L4 and an L7 load balancer? Which one can route `/api` to a different backend?
