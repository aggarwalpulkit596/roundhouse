package network

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
)

func TestIPAMAllocatesUniqueAddressesAndReleases(t *testing.T) {
	m, err := New(t.TempDir(), "rhtest0", "10.99.0.0/29") // 8 addresses: .2-.6 usable
	if err != nil {
		t.Fatal(err)
	}
	if gw := m.Gateway().String(); gw != "10.99.0.1" {
		t.Fatalf("gateway = %s", gw)
	}
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		ip, err := m.Allocate(fmt.Sprintf("c%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if seen[ip.String()] || ip.String() == "10.99.0.1" || ip.String() == "10.99.0.7" {
			t.Fatalf("bad or duplicate address %s", ip)
		}
		seen[ip.String()] = true
	}
	if _, err := m.Allocate("c5"); err == nil {
		t.Fatal("pool should be exhausted")
	}
	again, _ := m.Allocate("c2")
	if !seen[again.String()] {
		t.Fatal("re-allocating for the same owner should return its address")
	}
	if err := m.Release("c2"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Allocate("c6"); err != nil {
		t.Fatalf("released address should be reusable: %v", err)
	}
}

func TestIPAMIsSafeUnderConcurrency(t *testing.T) {
	m, _ := New(t.TempDir(), "rhtest0", "10.99.0.0/24")
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]string{}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			ip, err := m.Allocate(id)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if owner, dup := got[ip.String()]; dup {
				t.Errorf("%s given to both %s and %s", ip, owner, id)
			}
			got[ip.String()] = id
		}(i)
	}
	wg.Wait()
}

func echoServer(t *testing.T, tag string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				fmt.Fprintf(c, "%s:%s", tag, b)
			}()
		}
	}()
	return ln.Addr().String()
}

func roundTrip(t *testing.T, addr, msg string) string {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(msg))
	c.(*net.TCPConn).CloseWrite() // half-close must propagate through the proxy
	b, _ := io.ReadAll(c)
	return string(b)
}

func TestProxySwapsBackendsAtomically(t *testing.T) {
	a, b := echoServer(t, "a"), echoServer(t, "b")
	p, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Serve(ctx)

	if got := roundTrip(t, p.Addr().String(), "x"); got != "" {
		t.Fatalf("with no backends the connection should just close, got %q", got)
	}
	p.SetBackends([]string{a})
	if got := roundTrip(t, p.Addr().String(), "hi"); got != "a:hi" {
		t.Fatalf("got %q", got)
	}
	p.SetBackends([]string{b})
	if got := roundTrip(t, p.Addr().String(), "hi"); got != "b:hi" {
		t.Fatalf("after swap got %q", got)
	}
	// A dead backend is skipped in favour of a live one.
	p.SetBackends([]string{"127.0.0.1:1", a})
	for i := 0; i < 4; i++ {
		if got := roundTrip(t, p.Addr().String(), "r"); got != "a:r" {
			t.Fatalf("failover got %q", got)
		}
	}
}
