package state

import (
	"testing"
	"time"
)

// The claim is what keeps dispatch from being dangerous: two watchers must not
// both spawn a session for one PR, a dead session must not hold a round for
// ever, and a fix that keeps not working must stop rather than loop.
func TestDispatchClaim(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	round := &Round{Repo: "o/r", PR: 1, Head: "aaaaaaaa1"}

	if ok, why := round.ClaimDispatch("host-a", "tok-a", now, 3); !ok {
		t.Fatalf("first claim refused: %s", why)
	}
	if ok, why := round.ClaimDispatch("host-b", "tok-b", now, 3); ok {
		t.Error("a second watcher took a live claim")
	} else if why == "" {
		t.Error("a refusal must say why")
	}

	// A heartbeat keeps a long session's claim alive.
	if !round.HeartbeatDispatch("tok-a", now.Add(9*time.Minute)) {
		t.Error("the owner must be able to heartbeat")
	}
	if round.HeartbeatDispatch("tok-b", now.Add(9*time.Minute)) {
		t.Error("a heartbeat from another token must be refused")
	}
	if ok, _ := round.ClaimDispatch("host-b", "tok-b", now.Add(10*time.Minute), 3); ok {
		t.Error("a heartbeated claim must not be stolen")
	}

	// A dead session's claim is taken over once its heartbeat goes stale, and the
	// attempt it already spent is not forgotten.
	stale := now.Add(9*time.Minute + DispatchTTL)
	ok, why := round.ClaimDispatch("host-b", "tok-b", stale, 3)
	if !ok {
		t.Fatalf("a stale claim must be takeable: %s", why)
	}
	if round.Dispatch.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 — the dead session's attempt still happened", round.Dispatch.Attempts)
	}

	// Releasing frees the round but keeps the count.
	if !round.ReleaseDispatch("tok-b") {
		t.Fatal("the owner must be able to release")
	}
	if round.DispatchHeld(stale) {
		t.Error("a released claim must not read as held")
	}
	if ok, _ := round.ClaimDispatch("host-c", "tok-c", stale, 3); !ok {
		t.Fatal("a released round must be claimable")
	}
	if round.Dispatch.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", round.Dispatch.Attempts)
	}

	// And the bound holds: a fix that keeps not working stops.
	round.ReleaseDispatch("tok-c")
	if ok, why := round.ClaimDispatch("host-c", "tok-d", stale, 3); ok {
		t.Error("the attempt bound must stop a fourth dispatch")
	} else if why == "" {
		t.Error("the bound's refusal must say why")
	}

	// A new head is a fresh start: the previous attempt achieved something.
	fresh := &Round{Repo: "o/r", PR: 1, Head: "bbbbbbbb2"}
	if ok, _ := fresh.ClaimDispatch("host-c", "tok-e", stale, 3); !ok {
		t.Error("a new head must be dispatchable again")
	}
}
