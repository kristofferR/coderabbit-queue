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
	if ok, _ := round.HeartbeatDispatch("tok-a", now.Add(9*time.Minute)); !ok {
		t.Error("the owner must be able to heartbeat")
	}
	if ok, taken := round.HeartbeatDispatch("tok-b", now.Add(9*time.Minute)); ok || !taken {
		t.Errorf("another token got ok=%v taken=%v, want refused and told the claim is live", ok, taken)
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

// A round being fixed right now must not also be fired: the session is replacing
// the code the review would be about, and its push moves the head minutes later.
// The rest of the queue keeps moving, because the round leaves the queue instead
// of being refused at the front of it.
func TestAClaimedRoundLeavesTheFireQueue(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := &State{}
	fixing, err := s.NewRound("o/r", 1, "aaaaaaaa1", now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.NewRound("o/r", 2, "bbbbbbbb2", now)
	if err != nil {
		t.Fatal(err)
	}
	s.PutRound(*other)
	if ok, why := fixing.ClaimDispatch("host", "tok", now, 3); !ok {
		t.Fatalf("setup: %s", why)
	}
	s.PutRound(*fixing)

	if fixing.FireEligible(now) {
		t.Error("a round a fix session holds is fire-eligible; the review would be of a head about to move")
	}
	if fixing.Phase != PhaseAwaitingRetry || fixing.RetryAt == nil || !fixing.RetryAt.After(now) {
		t.Fatalf("claim was not mirrored into the retry gate older binaries honor: %#v", fixing)
	}
	if next := s.NextEligible(now); next == nil || next.PR != 2 {
		t.Errorf("next eligible = %#v, want the PR nobody is fixing — the queue must keep moving", next)
	}
	for _, e := range s.Queue(now, 0) {
		if e.PR == fixing.PR {
			t.Errorf("held dispatch appears in the rendered queue: %#v", e)
		}
	}
	// A claim nobody heartbeats expires, so this can never park a round for good.
	if !fixing.FireEligible(now.Add(2 * DispatchTTL)) {
		t.Error("an expired claim still holds the round out of the queue")
	}
}

func TestDispatchHoldRestoresTheOriginalQueueState(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cooldown := now.Add(30 * time.Minute)
	r := Round{Repo: "o/r", PR: 1, Head: "aaaaaaaa1", Phase: PhaseAwaitingRetry, RetryAt: &cooldown}

	if ok, why := r.ClaimDispatch("host", "tok", now, 3); !ok {
		t.Fatal(why)
	}
	firstHold := *r.RetryAt
	if ok, taken := r.HeartbeatDispatch("tok", now.Add(DispatchTTL/2)); !ok || taken {
		t.Fatalf("heartbeat = ok %v taken %v", ok, taken)
	}
	if !r.RetryAt.After(firstHold) {
		t.Errorf("retry compatibility hold was not extended: %s <= %s", r.RetryAt, firstHold)
	}
	if !r.ReleaseDispatch("tok") {
		t.Fatal("release failed")
	}
	if r.Phase != PhaseAwaitingRetry || r.RetryAt == nil || !r.RetryAt.Equal(cooldown) {
		t.Errorf("release restored phase=%s retry=%v, want awaiting_retry at %s", r.Phase, r.RetryAt, cooldown)
	}
}

func TestArchivedDispatchCanBeHeartbeatedWithoutRevivingItsRound(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	r := Round{Repo: "o/r", PR: 1, Head: "aaaaaaaa1", Phase: PhaseQueued}
	if ok, why := r.ClaimDispatch("host", "tok", now, 3); !ok {
		t.Fatal(why)
	}
	r.Abandon("superseded")
	st := State{Archive: []Round{r}}

	later := now.Add(DispatchTTL / 2)
	if ok, taken := st.HeartbeatArchivedDispatch("o/r", 1, "tok", later); !ok || taken {
		t.Fatalf("archived heartbeat = ok %v taken %v", ok, taken)
	}
	if got := st.Archive[0].Dispatch.Heartbeat; !got.Equal(later) {
		t.Errorf("heartbeat = %s, want %s", got, later)
	}
	if !st.ReleaseArchivedDispatch("o/r", 1, "tok") {
		t.Fatal("archived release failed")
	}
	if st.Archive[0].Phase != PhaseAbandoned {
		t.Errorf("release revived archived round into %s", st.Archive[0].Phase)
	}
}

// A session's own push moves the head, so crq supersedes the round and the fresh
// one carries no claim. Reading that as "somebody took this round" killed the
// session between pushing and resolving — every single time it succeeded.
func TestHeartbeatSeparatesSupersededFromStolen(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	// Superseded: the claim is gone, and nobody else holds one.
	fresh := &Round{Repo: "o/r", PR: 1, Head: "bbbbbbbb2"}
	if ok, taken := fresh.HeartbeatDispatch("tok-a", now); ok || taken {
		t.Errorf("superseded round gave ok=%v taken=%v, want a benign miss", ok, taken)
	}

	// Stolen: somebody else holds a LIVE claim.
	stolen := &Round{Repo: "o/r", PR: 1, Head: "aaaaaaaa1"}
	if ok, _ := stolen.ClaimDispatch("other", "tok-b", now, 3); !ok {
		t.Fatal("setup: the other watcher should have claimed it")
	}
	if ok, taken := stolen.HeartbeatDispatch("tok-a", now); ok || !taken {
		t.Errorf("stolen round gave ok=%v taken=%v, want taken", ok, taken)
	}

	// A claim that expired is not somebody else working: nothing is running.
	if ok, taken := stolen.HeartbeatDispatch("tok-a", now.Add(2*DispatchTTL)); ok || taken {
		t.Errorf("expired claim gave ok=%v taken=%v, want a benign miss", ok, taken)
	}
}
