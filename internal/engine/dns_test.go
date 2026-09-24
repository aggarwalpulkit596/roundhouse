package engine

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestDNSAnswersServiceRecords(t *testing.T) {
	e := &Engine{cfg: Config{Domain: "rh.internal"}, dnsIPs: map[string][]string{"web": {"10.88.0.5", "10.88.0.6"}}}
	s := &DNSServer{Engine: e, TTL: 5}

	resp := s.Answer(BuildQuery(0xbeef, "web.rh.internal.", 1))
	if resp == nil {
		t.Fatal("in-zone query was not answered")
	}
	if id := binary.BigEndian.Uint16(resp[0:2]); id != 0xbeef {
		t.Fatalf("id = %x", id)
	}
	flags := binary.BigEndian.Uint16(resp[2:4])
	if flags&0x8000 == 0 || flags&0x0400 == 0 || flags&0x000f != 0 {
		t.Fatalf("want QR, AA and NOERROR; flags = %016b", flags)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 2 {
		t.Fatalf("answers = %d, want 2", an)
	}
	// The last 4 bytes are the second A record's address.
	if ip := net.IP(resp[len(resp)-4:]); !ip.Equal(net.ParseIP("10.88.0.6")) {
		t.Fatalf("last answer = %s", ip)
	}
}

func TestDNSUnknownServiceIsNXDOMAIN(t *testing.T) {
	e := &Engine{cfg: Config{Domain: "rh.internal"}, dnsIPs: map[string][]string{}}
	s := &DNSServer{Engine: e}
	resp := s.Answer(BuildQuery(1, "nope.rh.internal", 1))
	if rcode := binary.BigEndian.Uint16(resp[2:4]) & 0xf; rcode != 3 {
		t.Fatalf("rcode = %d, want NXDOMAIN (3)", rcode)
	}
}

func TestDNSForwardsOtherZones(t *testing.T) {
	e := &Engine{cfg: Config{Domain: "rh.internal"}}
	s := &DNSServer{Engine: e}
	if s.Answer(BuildQuery(1, "example.com", 1)) != nil {
		t.Fatal("out-of-zone query must be forwarded, not answered")
	}
	if s.Answer([]byte{1, 2, 3}) != nil {
		t.Fatal("truncated packet must not be answered")
	}
}
