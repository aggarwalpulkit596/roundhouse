// Package network gives containers their own network stack, connected to
// the host through a Linux bridge — the same topology as Docker's default
// "bridge" network and the CNI bridge plugin:
//
//	           host netns                               container netns
//	┌───────────────────────────────┐              ┌─────────────────────┐
//	│  eth0 ── iptables MASQUERADE  │              │                     │
//	│                               │   veth pair  │  eth0 10.88.0.5/16  │
//	│  rh0 (bridge) 10.88.0.1/16 ───┼── rh-<id> ═══┼═ (renamed peer)     │
//	│                               │              │  default via .0.1   │
//	└───────────────────────────────┘              └─────────────────────┘
//
// A veth pair is a virtual cable: packets written to one end come out the
// other. One end stays on the host bridge; the other is moved into the
// container's network namespace (identified by its init pid).
package network

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Manager owns the bridge and the address pool.
type Manager struct {
	// StateDir holds ipam.json.
	StateDir string
	// Bridge is the host bridge name.
	Bridge string
	// Subnet is the container address pool; the first host address is the
	// gateway (the bridge's own address).
	Subnet *net.IPNet
}

// DefaultSubnet is Roundhouse's container network.
const DefaultSubnet = "10.88.0.0/16"

// New returns a manager with defaults for anything unset.
func New(stateDir, bridge, subnet string) (*Manager, error) {
	if bridge == "" {
		bridge = "rh0"
	}
	if subnet == "" {
		subnet = DefaultSubnet
	}
	_, n, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	return &Manager{StateDir: stateDir, Bridge: bridge, Subnet: n}, nil
}

// Gateway is the bridge address, the containers' default route.
func (m *Manager) Gateway() net.IP { return nthIP(m.Subnet, 1) }

// Setup creates the bridge, enables forwarding and installs NAT. It is
// idempotent and cheap, so callers run it before every attach.
func (m *Manager) Setup() error {
	br, err := netlink.LinkByName(m.Bridge)
	if err != nil {
		la := netlink.NewLinkAttrs()
		la.Name = m.Bridge
		if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: la}); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("create bridge %s: %w", m.Bridge, err)
		}
		if br, err = netlink.LinkByName(m.Bridge); err != nil {
			return err
		}
	}
	ones, _ := m.Subnet.Mask.Size()
	gw := &netlink.Addr{IPNet: &net.IPNet{IP: m.Gateway(), Mask: net.CIDRMask(ones, 32)}}
	if err := netlink.AddrAdd(br, gw); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("address bridge: %w", err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return err
	}
	// The host must route between the bridge and its uplink.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	return m.ensureNAT()
}

// ensureNAT installs the three rules a bridge network needs. -C checks for
// an existing rule so repeated Setup calls do not duplicate them.
func (m *Manager) ensureNAT() error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil // no iptables: containers still reach each other and the host
	}
	sub := m.Subnet.String()
	rules := [][]string{
		// Rewrite the source of container traffic leaving the host so replies
		// come back to the host, which un-NATs them to the container.
		{"-t", "nat", "POSTROUTING", "-s", sub, "!", "-o", m.Bridge, "-j", "MASQUERADE"},
		{"-t", "filter", "FORWARD", "-i", m.Bridge, "-j", "ACCEPT"},
		{"-t", "filter", "FORWARD", "-o", m.Bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, r := range rules {
		table, chain, spec := r[1], r[2], r[3:]
		check := append([]string{"-w", "-t", table, "-C", chain}, spec...)
		if exec.Command("iptables", check...).Run() == nil {
			continue
		}
		add := append([]string{"-w", "-t", table, "-A", chain}, spec...)
		if out, err := exec.Command("iptables", add...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %s: %v: %s", strings.Join(add, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// HostVethName is the host end of a container's veth pair. Interface names
// are limited to 15 bytes (IFNAMSIZ-1).
func HostVethName(id string) string {
	if len(id) > 11 {
		id = id[:11]
	}
	return "rh-" + id
}

// Attach wires the network namespace of pid to the bridge with address ip.
// It runs from the parent while the container init is blocked, before the
// user program starts, so the program never sees a half-configured network.
func (m *Manager) Attach(id string, pid int, ip net.IP) error {
	br, err := netlink.LinkByName(m.Bridge)
	if err != nil {
		return fmt.Errorf("bridge %s: %w (did Setup run?)", m.Bridge, err)
	}
	hostName := HostVethName(id)
	peerName := "rhp" + hostName[3:]
	if old, err := netlink.LinkByName(hostName); err == nil {
		_ = netlink.LinkDel(old) // left over from a previous run of this container
	}
	la := netlink.NewLinkAttrs()
	la.Name = hostName
	la.MasterIndex = br.Attrs().Index
	la.MTU = br.Attrs().MTU
	veth := &netlink.Veth{LinkAttrs: la, PeerName: peerName}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create veth: %w", err)
	}
	cleanup := func() { _ = netlink.LinkDel(veth) }
	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		cleanup()
		return err
	}
	// Moving the link into the namespace makes it disappear from the host.
	if err := netlink.LinkSetNsPid(peer, pid); err != nil {
		cleanup()
		return fmt.Errorf("move veth into netns: %w", err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		cleanup()
		return err
	}

	// Configure the inside through a netlink socket opened in the container's
	// namespace. No thread switching (setns) is needed with a handle.
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		cleanup()
		return err
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		cleanup()
		return err
	}
	defer h.Close()

	inner, err := h.LinkByName(peerName)
	if err != nil {
		cleanup()
		return err
	}
	if err := h.LinkSetName(inner, "eth0"); err != nil {
		cleanup()
		return fmt.Errorf("rename eth0: %w", err)
	}
	ones, _ := m.Subnet.Mask.Size()
	if err := h.AddrAdd(inner, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(ones, 32)}}); err != nil {
		cleanup()
		return fmt.Errorf("address eth0: %w", err)
	}
	if err := h.LinkSetUp(inner); err != nil {
		cleanup()
		return err
	}
	if lo, err := h.LinkByName("lo"); err == nil {
		_ = h.LinkSetUp(lo)
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: inner.Attrs().Index, Gw: m.Gateway()}); err != nil {
		cleanup()
		return fmt.Errorf("default route: %w", err)
	}
	return nil
}

// Detach removes the host end of the veth pair (deleting one end deletes
// both). When a network namespace dies the kernel does this automatically.
func (m *Manager) Detach(id string) {
	if l, err := netlink.LinkByName(HostVethName(id)); err == nil {
		_ = netlink.LinkDel(l)
	}
}

// ---------------------------------------------------------------------------
// IPAM: a tiny host-local allocator, like the CNI host-local plugin. State
// is a JSON file guarded by flock so concurrent `rh run`s never collide.

type ipamState struct {
	Allocations map[string]string `json:"allocations"` // ip -> container id
	Last        string            `json:"last"`
}

// Allocate reserves an address for id, returning the existing one if id
// already has an address.
func (m *Manager) Allocate(id string) (net.IP, error) {
	var out net.IP
	err := m.withIPAM(func(st *ipamState) error {
		for ip, owner := range st.Allocations {
			if owner == id {
				out = net.ParseIP(ip).To4()
				return nil
			}
		}
		size := hostCount(m.Subnet)
		// Continue after the last allocation rather than reusing the lowest
		// free address: a just-freed IP may still be in peers' ARP/conntrack
		// caches.
		start := uint32(2)
		if last := net.ParseIP(st.Last); last != nil && m.Subnet.Contains(last) {
			start = ipOffset(m.Subnet, last) + 1
		}
		for i := uint32(0); i < size; i++ {
			off := 2 + (start-2+i)%(size-2)
			ip := nthIP(m.Subnet, off)
			if _, used := st.Allocations[ip.String()]; !used {
				st.Allocations[ip.String()] = id
				st.Last = ip.String()
				out = ip
				return nil
			}
		}
		return fmt.Errorf("address pool %s exhausted", m.Subnet)
	})
	return out, err
}

// Release frees id's address.
func (m *Manager) Release(id string) error {
	return m.withIPAM(func(st *ipamState) error {
		for ip, owner := range st.Allocations {
			if owner == id {
				delete(st.Allocations, ip)
			}
		}
		return nil
	})
}

// Allocations returns ip -> owner, sorted by ip, for display.
func (m *Manager) Allocations() ([][2]string, error) {
	var out [][2]string
	err := m.withIPAM(func(st *ipamState) error {
		for ip, id := range st.Allocations {
			out = append(out, [2]string{ip, id})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out, err
}

func (m *Manager) withIPAM(fn func(*ipamState) error) error {
	path := filepath.Join(m.StateDir, "ipam.json")
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	st := ipamState{Allocations: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("corrupt %s: %w", path, err)
		}
		if st.Allocations == nil {
			st.Allocations = map[string]string{}
		}
	}
	if err := fn(&st); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func nthIP(n *net.IPNet, off uint32) net.IP {
	base := binary.BigEndian.Uint32(n.IP.To4())
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, base+off)
	return ip
}

func ipOffset(n *net.IPNet, ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip.To4()) - binary.BigEndian.Uint32(n.IP.To4())
}

// hostCount is the number of addresses in the subnet including network and
// broadcast; usable offsets are 2..size-2 (1 is the gateway).
func hostCount(n *net.IPNet) uint32 {
	ones, bits := n.Mask.Size()
	return 1<<uint(bits-ones) - 1
}

// LoopbackUp brings up lo inside the network namespace of pid, which is all
// a --net none container gets.
func LoopbackUp(pid int) error {
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return err
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	lo, err := h.LinkByName("lo")
	if err != nil {
		return err
	}
	return h.LinkSetUp(lo)
}
