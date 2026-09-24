package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"
)

// DNSServer answers <service>.<domain> with the IPs of that service's
// healthy instances and forwards every other query upstream. It listens on
// the bridge gateway, and containers get it as their only nameserver.
//
// This is the idea behind Railway's private networking DNS (they intercept
// lookups with eBPF; we simply own resolv.conf) and Kubernetes' CoreDNS
// "<svc>.<ns>.svc.cluster.local" records. The wire format is small enough to
// handle by hand, which is a good way to learn it.
type DNSServer struct {
	Engine   *Engine
	Upstream []string // "8.8.8.8:53"
	TTL      uint32
	conn     net.PacketConn
}

// ListenAndServe binds addr ("10.88.0.1:53") and serves until ctx is done.
func (s *DNSServer) ListenAndServe(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	s.conn = pc
	if s.TTL == 0 {
		s.TTL = 5 // short: instances move during deploys
	}
	go func() {
		<-ctx.Done()
		pc.Close()
	}()
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		go s.handle(q, from)
	}
}

func (s *DNSServer) handle(q []byte, from net.Addr) {
	resp := s.Answer(q)
	if resp == nil {
		resp = s.forward(q)
	}
	if resp != nil {
		_, _ = s.conn.WriteTo(resp, from)
	}
}

// Answer builds a response for queries in our zone, or returns nil for
// queries that should be forwarded.
func (s *DNSServer) Answer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	if binary.BigEndian.Uint16(q[4:6]) != 1 { // QDCOUNT
		return nil
	}
	name, off, ok := readName(q, 12)
	if !ok || off+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[off : off+2])
	qclass := binary.BigEndian.Uint16(q[off+2 : off+4])
	question := q[12 : off+4]
	zone := "." + s.Engine.Domain()
	lname := strings.ToLower(strings.TrimSuffix(name, "."))
	if !strings.HasSuffix(lname, zone) || qclass != 1 {
		return nil
	}
	ips := s.Engine.Resolve(lname)

	// Header: same ID, QR=1 AA=1 RD copied, RA=1, RCODE 0 or 3 (NXDOMAIN).
	h := make([]byte, 12)
	copy(h[0:2], q[0:2])
	flags := uint16(0x8400) | uint16(q[2]&0x01)<<8 | 0x0080
	var answers [][]byte
	if qtype == 1 || qtype == 255 { // A or ANY
		for _, ip := range ips {
			v4 := net.ParseIP(ip).To4()
			if v4 == nil {
				continue
			}
			rr := []byte{0xc0, 12} // pointer to the question name
			rr = binary.BigEndian.AppendUint16(rr, 1)
			rr = binary.BigEndian.AppendUint16(rr, 1)
			rr = binary.BigEndian.AppendUint32(rr, s.TTL)
			rr = binary.BigEndian.AppendUint16(rr, 4)
			rr = append(rr, v4...)
			answers = append(answers, rr)
		}
	}
	if len(ips) == 0 {
		flags |= 3 // NXDOMAIN: no such service (or nothing running)
	}
	binary.BigEndian.PutUint16(h[2:4], flags)
	binary.BigEndian.PutUint16(h[4:6], 1)
	binary.BigEndian.PutUint16(h[6:8], uint16(len(answers)))
	out := append(h, question...)
	for _, a := range answers {
		out = append(out, a...)
	}
	return out
}

func (s *DNSServer) forward(q []byte) []byte {
	for _, up := range s.Upstream {
		c, err := net.DialTimeout("udp", up, time.Second)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(q); err != nil {
			c.Close()
			continue
		}
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		c.Close()
		if err == nil && n >= 12 && buf[0] == q[0] && buf[1] == q[1] {
			return buf[:n]
		}
	}
	// SERVFAIL so the client does not wait for a timeout.
	h := make([]byte, 12)
	copy(h[0:2], q[0:2])
	binary.BigEndian.PutUint16(h[2:4], 0x8182)
	return h
}

// readName decodes a (non-compressed) QNAME starting at off.
func readName(b []byte, off int) (string, int, bool) {
	var labels []string
	for i := 0; i < 128; i++ {
		if off >= len(b) {
			return "", 0, false
		}
		l := int(b[off])
		off++
		if l == 0 {
			return strings.Join(labels, ".") + ".", off, true
		}
		if l&0xc0 != 0 || off+l > len(b) {
			return "", 0, false // questions are never compressed
		}
		labels = append(labels, string(b[off:off+l]))
		off += l
	}
	return "", 0, false
}

// BuildQuery encodes a single-question query, for tests and `rh dig`.
func BuildQuery(id uint16, name string, qtype uint16) []byte {
	q := make([]byte, 12)
	binary.BigEndian.PutUint16(q[0:2], id)
	binary.BigEndian.PutUint16(q[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(q[4:6], 1)
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0)
	q = binary.BigEndian.AppendUint16(q, qtype)
	q = binary.BigEndian.AppendUint16(q, 1)
	return q
}
