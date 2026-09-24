package engine

import (
	"sync"
	"time"
)

// Event is one entry in the engine's activity feed (what Railway shows in a
// deployment's timeline).
type Event struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	Service    string    `json:"service,omitempty"`
	Deployment string    `json:"deployment,omitempty"`
	Instance   string    `json:"instance,omitempty"`
	Type       string    `json:"type"`
	Message    string    `json:"message"`
}

// eventBus keeps a ring buffer for late readers and fans out to live
// subscribers. Slow subscribers drop events rather than block the engine.
type eventBus struct {
	mu   sync.Mutex
	seq  uint64
	ring []Event
	max  int
	subs map[chan Event]struct{}
}

func newEventBus(max int) *eventBus {
	return &eventBus{max: max, subs: map[chan Event]struct{}{}}
}

func (b *eventBus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b.ring = append(b.ring, e)
	if len(b.ring) > b.max {
		b.ring = b.ring[len(b.ring)-b.max:]
	}
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Since returns buffered events after seq, optionally for one service.
func (b *eventBus) Since(seq uint64, service string) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Event
	for _, e := range b.ring {
		if e.Seq > seq && (service == "" || e.Service == service) {
			out = append(out, e)
		}
	}
	return out
}

func (b *eventBus) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}
