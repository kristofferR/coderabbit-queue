package crq

import (
	"strings"
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

// The queue exists for the account-metered review and nothing else. Fixing
// findings spends none of that allowance, so a PR whose findings are ready must
// get a session now — not a place in a line. Capping it at three was the same
// mistake crq already corrected for co-only rounds.
func TestDispatchIsNotQueuedByDefault(t *testing.T) {
	pool := newDispatchPool(0) // the default: no cap
	release := make(chan struct{})

	for i := 0; i < 25; i++ {
		if ok, why := pool.start(func() { <-release }); !ok {
			t.Fatalf("session %d was made to wait: %s — unmetered work must not queue", i, why)
		}
	}
	close(release)
	pool.wait()
}

// A cap is a resource valve an operator opts into, and hitting it has to say so
// in those terms rather than reading as routine.
func TestConfiguredCapSaysItIsAConfiguredCap(t *testing.T) {
	pool := newDispatchPool(1)
	release := make(chan struct{})
	if ok, _ := pool.start(func() { <-release }); !ok {
		t.Fatal("the first session should run")
	}
	ok, why := pool.start(func() {})
	if ok {
		t.Fatal("a cap of one allowed two")
	}
	if !strings.Contains(why, "CRQ_DISPATCH_CONCURRENCY") {
		t.Errorf("reason = %q, want it to name the setting that caused the wait", why)
	}
	close(release)
	pool.wait()
}
