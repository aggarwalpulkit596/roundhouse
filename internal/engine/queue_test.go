package engine

import (
	"testing"
	"time"
)

func TestQueueCoalescesDuplicates(t *testing.T) {
	q := newWorkQueue()
	q.Add("a")
	q.Add("a")
	q.Add("b")
	if q.Len() != 2 {
		t.Fatalf("len = %d, want 2", q.Len())
	}
}

func TestQueueRequeuesKeyChangedWhileProcessing(t *testing.T) {
	q := newWorkQueue()
	q.Add("a")
	k, _ := q.Get()
	q.Add("a") // an event arrives mid-sync
	if q.Len() != 0 {
		t.Fatal("a key being processed must not be handed to a second worker")
	}
	q.Done(k)
	if q.Len() != 1 {
		t.Fatal("the change during processing was lost")
	}
}

func TestQueueAddAfter(t *testing.T) {
	q := newWorkQueue()
	q.AddAfter("a", 20*time.Millisecond)
	if q.Len() != 0 {
		t.Fatal("added too early")
	}
	time.Sleep(60 * time.Millisecond)
	if q.Len() != 1 {
		t.Fatal("not added after delay")
	}
}

func TestQueueShutdownUnblocksGet(t *testing.T) {
	q := newWorkQueue()
	done := make(chan bool)
	go func() {
		_, ok := q.Get()
		done <- ok
	}()
	time.Sleep(10 * time.Millisecond)
	q.Shutdown()
	if <-done {
		t.Fatal("Get should report shutdown")
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	e := &Engine{cfg: Config{BackoffBase: time.Second}}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := e.backoff(i); got != w*time.Second {
			t.Errorf("backoff(%d) = %s, want %s", i, got, w*time.Second)
		}
	}
}
