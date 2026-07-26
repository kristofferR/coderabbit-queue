package crq

import (
	"sync"
	"testing"
	"time"
)

// A session used to run inline, so one twenty-minute fix blocked every other PR
// for twenty minutes. The pool exists to keep the decision loop moving.
func TestDispatchPoolDoesNotBlockTheCaller(t *testing.T) {
	pool := newDispatchPool(2)
	release := make(chan struct{})

	start := time.Now()
	for i := 0; i < 2; i++ {
		if ok, why := pool.start(func() { <-release }); !ok {
			t.Fatalf("session %d refused: %s", i, why)
		}
	}
	// Both slots are busy: the caller is told so immediately rather than waiting.
	ok, why := pool.start(func() {})
	if ok {
		t.Error("a third session ran with only two slots")
	}
	if why == "" {
		t.Error("a refusal must say why, or the PR looks handled")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("starting sessions blocked for %s; the pass must not wait on them", elapsed)
	}

	// A finished session frees its slot.
	close(release)
	pool.wait()
	if ok, why := pool.start(func() {}); !ok {
		t.Errorf("slot not released after the session finished: %s", why)
	}
	pool.wait()
}

// --once must not return while its sessions are still writing.
func TestDispatchPoolWaitsForRunningSessions(t *testing.T) {
	pool := newDispatchPool(3)
	var mu sync.Mutex
	done := 0
	for i := 0; i < 3; i++ {
		pool.start(func() {
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			done++
			mu.Unlock()
		})
	}
	pool.wait()
	mu.Lock()
	defer mu.Unlock()
	if done != 3 {
		t.Errorf("wait returned with %d/3 sessions finished", done)
	}
}
