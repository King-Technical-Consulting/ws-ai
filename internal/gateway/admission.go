package gateway

import (
	"container/heap"
	"context"
	"sync"
)

// Admission: an endpoint that declares max_concurrency serves that many
// requests at once (a llama-server --parallel N). Past that, llama.cpp would
// queue the rest itself, first come, first served, where the gateway cannot
// reorder anything. So the gateway holds the overflow itself, in a priority
// queue per endpoint: when a slot frees, the waiting request with the highest
// priority takes it, and among equals the one that waited longest. A request
// already running is never preempted. An endpoint with no declared
// max_concurrency has no queue and admits everything at once.

// Priorities for Metadata-derived scheduling; higher goes first.
const (
	PriorityDefault = 0
	PriorityMember  = 10
	PriorityOwner   = 100
)

type waiter struct {
	prio    int
	seq     uint64
	slots   int
	ch      chan struct{}
	idx     int
	granted bool
}

type waitQueue []*waiter

func (q waitQueue) Len() int { return len(q) }
func (q waitQueue) Less(i, j int) bool {
	if q[i].prio != q[j].prio {
		return q[i].prio > q[j].prio
	}
	return q[i].seq < q[j].seq
}
func (q waitQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i]; q[i].idx = i; q[j].idx = j }
func (q *waitQueue) Push(x any)   { w := x.(*waiter); w.idx = len(*q); *q = append(*q, w) }
func (q *waitQueue) Pop() any {
	old := *q
	n := len(old)
	w := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return w
}

func (t *Throughput) queuedLocked(id string) int {
	if q := t.queue[id]; q != nil {
		return q.Len()
	}
	return 0
}

// grantLocked hands free slots to the best waiters. t.mu must be held.
func (t *Throughput) grantLocked(id string) {
	q := t.queue[id]
	for q != nil && q.Len() > 0 {
		top := (*q)[0]
		if t.busy[id] >= top.slots {
			return
		}
		heap.Pop(q)
		top.granted = true
		t.busy[id]++
		close(top.ch)
	}
	if q != nil && q.Len() == 0 {
		delete(t.queue, id)
	}
}

func (t *Throughput) releaseFn(id string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.busy[id] > 0 {
				t.busy[id]--
			}
			if t.busy[id] == 0 {
				delete(t.busy, id)
			}
			t.grantLocked(id)
			t.mu.Unlock()
		})
	}
}

// Acquire takes a slot on an endpoint that serves slots requests at once
// (slots <= 0 means unlimited: it never waits). If none is free it waits in
// the priority queue until one is granted or ctx ends. The returned function
// releases the slot and is safe to call more than once.
func (t *Throughput) Acquire(ctx context.Context, id string, slots, prio int) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	if slots <= 0 || (t.busy[id] < slots && t.queuedLocked(id) == 0) {
		t.busy[id]++
		t.mu.Unlock()
		return t.releaseFn(id), nil
	}
	q := t.queue[id]
	if q == nil {
		q = &waitQueue{}
		t.queue[id] = q
	}
	t.seq++
	w := &waiter{prio: prio, seq: t.seq, slots: slots, ch: make(chan struct{})}
	heap.Push(q, w)
	t.mu.Unlock()

	select {
	case <-w.ch:
		return t.releaseFn(id), nil
	case <-ctx.Done():
		t.mu.Lock()
		if w.granted {
			// The slot arrived as the context ended: give it back.
			t.mu.Unlock()
			t.releaseFn(id)()
			return nil, ctx.Err()
		}
		heap.Remove(q, w.idx)
		if q.Len() == 0 {
			delete(t.queue, id)
		}
		t.mu.Unlock()
		return nil, ctx.Err()
	}
}
