package network

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy forwards TCP connections from a host port to a changing set of
// backends. It is Roundhouse's port publisher (like docker-proxy) and its
// edge router: swapping the backend set atomically is how a deploy moves
// traffic from old instances to new ones with no dropped connections.
//
// Existing connections keep their backend; only new connections see the new
// set. That is the "drain" half of a zero-downtime deploy.
type Proxy struct {
	ln       net.Listener
	backends atomic.Pointer[[]string]
	next     atomic.Uint64
	wg       sync.WaitGroup
	active   atomic.Int64

	// OnConn, if set, is told about each proxied connection (for metrics).
	OnConn func(backend string, err error)
}

// Listen starts a proxy on addr ("0.0.0.0:8080") with no backends.
func Listen(addr string) (*Proxy, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln}
	empty := []string{}
	p.backends.Store(&empty)
	return p, nil
}

// Addr is the bound address.
func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

// SetBackends replaces the backend set ("10.88.0.5:80", ...).
func (p *Proxy) SetBackends(b []string) {
	cp := append([]string(nil), b...)
	p.backends.Store(&cp)
}

// Backends returns the current set.
func (p *Proxy) Backends() []string { return *p.backends.Load() }

// Active is the number of open proxied connections.
func (p *Proxy) Active() int64 { return p.active.Load() }

// Serve accepts until ctx is cancelled or Close is called.
func (p *Proxy) Serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		p.ln.Close()
	}()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
		}()
	}
}

// Close stops accepting and waits for open connections to finish.
func (p *Proxy) Close() error {
	err := p.ln.Close()
	p.wg.Wait()
	return err
}

func (p *Proxy) pick() []string {
	b := *p.backends.Load()
	if len(b) == 0 {
		return nil
	}
	// Round-robin start, then try the others in order if a dial fails.
	start := int(p.next.Add(1)-1) % len(b)
	out := make([]string, 0, len(b))
	for i := range b {
		out = append(out, b[(start+i)%len(b)])
	}
	return out
}

func (p *Proxy) handle(client net.Conn) {
	defer client.Close()
	var upstream net.Conn
	var backend string
	var err error = errors.New("no healthy backends")
	for _, b := range p.pick() {
		upstream, err = net.DialTimeout("tcp", b, 2*time.Second)
		if err == nil {
			backend = b
			break
		}
	}
	if p.OnConn != nil {
		p.OnConn(backend, err)
	}
	if err != nil {
		return
	}
	defer upstream.Close()
	p.active.Add(1)
	defer p.active.Add(-1)
	Splice(client, upstream)
}

// Splice copies both directions until both are done, propagating half-close
// (a client that finishes sending but still wants the response).
func Splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
