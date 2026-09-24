package engine

import (
	"sync"
	"time"
)

// workQueue is a minimal version of client-go's workqueue, the heart of every
// Kubernetes controller. Its guarantees are what make concurrent
// reconciliation safe:
//
//   - A key is never processed by two workers at once.
//   - Adding a key that is already queued is a no-op (events coalesce).
//   - Adding a key while it is being processed marks it dirty; it is
//     re-queued when the worker calls Done, so no change is ever lost.
type workQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []string
	dirty      map[string]bool
	processing map[string]bool
	shutdown   bool
}

func newWorkQueue() *workQueue {
	q := &workQueue{dirty: map[string]bool{}, processing: map[string]bool{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *workQueue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown || q.dirty[key] {
		return
	}
	q.dirty[key] = true
	if q.processing[key] {
		return // re-queued by Done
	}
	q.queue = append(q.queue, key)
	q.cond.Signal()
}

// AddAfter schedules a key, e.g. for a restart backoff or a drain deadline.
func (q *workQueue) AddAfter(key string, d time.Duration) {
	if d <= 0 {
		q.Add(key)
		return
	}
	time.AfterFunc(d, func() { q.Add(key) })
}

// Get blocks for the next key. ok is false after Shutdown.
func (q *workQueue) Get() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.shutdown {
		q.cond.Wait()
	}
	if q.shutdown {
		return "", false
	}
	key := q.queue[0]
	q.queue = q.queue[1:]
	delete(q.dirty, key)
	q.processing[key] = true
	return key, true
}

// Done releases a key and re-queues it if it changed during processing.
func (q *workQueue) Done(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, key)
	if q.dirty[key] && !q.shutdown {
		q.queue = append(q.queue, key)
		q.cond.Signal()
	}
}

func (q *workQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

func (q *workQueue) Shutdown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shutdown = true
	q.cond.Broadcast()
}
