package gateway

import (
	"context"
	"sync"
	"testing"
	"time"
)

func waitQueued(t *testing.T, tp *Throughput, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tp.mu.RLock()
		got := tp.queuedLocked(id)
		tp.mu.RUnlock()
		if got == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("queue for %s never reached %d", id, n)
}

func TestAcquireUnlimitedNeverWaits(t *testing.T) {
	tp := NewThroughput()
	var rel []func()
	for i := 0; i < 50; i++ {
		r, err := tp.Acquire(context.Background(), "cloud", 0, PriorityDefault)
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	if tp.InFlight("cloud") != 50 {
		t.Errorf("in flight = %d", tp.InFlight("cloud"))
	}
	for _, r := range rel {
		r()
	}
	if tp.InFlight("cloud") != 0 {
		t.Errorf("in flight after release = %d", tp.InFlight("cloud"))
	}
}

// A full endpoint admits the owner before a member who queued earlier, and
// equal priorities in arrival order. A request already running is untouched.
func TestAcquireOwnerWinsTheQueue(t *testing.T) {
	tp := NewThroughput()
	ctx := context.Background()
	hold, err := tp.Acquire(ctx, "orin", 1, PriorityMember)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	start := func(name string, prio int, want int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := tp.Acquire(ctx, "orin", 1, prio)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			r()
		}()
		waitQueued(t, tp, "orin", want)
	}
	start("member-1", PriorityMember, 1)
	start("member-2", PriorityMember, 2)
	start("owner", PriorityOwner, 3)

	if st := tp.All()["orin"]; st.Queued != 3 || st.InFlight != 1 {
		t.Errorf("stat = %+v, want 3 queued and 1 in flight", st)
	}
	hold()
	wg.Wait()
	want := []string{"owner", "member-1", "member-2"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if tp.InFlight("orin") != 0 || tp.queuedLocked("orin") != 0 {
		t.Errorf("not drained: in flight %d, queued %d", tp.InFlight("orin"), tp.queuedLocked("orin"))
	}
}

func TestAcquireCancelledWaiterLeavesTheQueue(t *testing.T) {
	tp := NewThroughput()
	hold, _ := tp.Acquire(context.Background(), "orin", 1, PriorityOwner)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := tp.Acquire(ctx, "orin", 1, PriorityMember)
		errc <- err
	}()
	waitQueued(t, tp, "orin", 1)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("a cancelled waiter must get an error")
	}
	waitQueued(t, tp, "orin", 0)
	hold()
	// The slot is free again: the next request is admitted at once.
	r, err := tp.Acquire(context.Background(), "orin", 1, PriorityMember)
	if err != nil {
		t.Fatal(err)
	}
	r()
	if tp.InFlight("orin") != 0 {
		t.Errorf("leaked a slot: %d in flight", tp.InFlight("orin"))
	}
}

func TestAcquireReleaseTwiceIsHarmless(t *testing.T) {
	tp := NewThroughput()
	a, _ := tp.Acquire(context.Background(), "x", 2, 0)
	b, _ := tp.Acquire(context.Background(), "x", 2, 0)
	a()
	a()
	if tp.InFlight("x") != 1 {
		t.Errorf("in flight = %d, want 1", tp.InFlight("x"))
	}
	b()
}
