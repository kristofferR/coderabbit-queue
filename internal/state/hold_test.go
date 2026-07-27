package state

import (
	"testing"
	"time"
)

// The race a hold exists to close: the skip marker stopped auto-review from
// enqueueing and `crq cancel` stopped the pump, and between the two a daemon
// fired anyway. So the hold has to be enforced where a round is CHOSEN, not at
// each caller — an exemption that must be remembered at every site gets missed
// at one of them.
func TestHeldRoundsAreNeverChosenToFire(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	for _, pr := range []int{1, 2} {
		r, err := st.NewRound("owner/repo", pr, "aaaaaaaa1", now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		st.PutRound(*r)
	}

	if next := st.NextEligible(now); next == nil || next.PR != 1 {
		t.Fatalf("next = %#v, want PR 1 before any hold", next)
	}

	st.Hold("owner/repo", 1, "waiting on a decision", "cachyos", now)
	next := st.NextEligible(now)
	if next == nil || next.PR != 1 {
		// good: 1 is skipped
	} else {
		t.Fatalf("a held PR was chosen to fire: %#v", next)
	}
	if next == nil || next.PR != 2 {
		t.Fatalf("next = %#v, want the unheld PR 2 — a hold must not stop the queue", next)
	}
	// The dashboard's queue view must agree, or a held PR reads as waiting its
	// turn when nothing will ever take it.
	for _, r := range st.QueuedRounds(now) {
		if r.PR == 1 {
			t.Error("a held round is still advertised as queued")
		}
	}

	// The dashboard and the status line read Queue, not QueuedRounds: a held
	// round showing there tells the reader it is waiting its turn when nothing
	// will ever take it.
	for _, e := range st.Queue(now, 0) {
		if e.PR == 1 {
			t.Error("a held round appears in the dashboard queue")
		}
	}

	// Case-insensitively: a hold set as Owner/Repo must match a round recorded
	// as owner/repo.
	st.Unhold("owner/repo", 1)
	st.Hold("Owner/Repo", 1, "again", "cachyos", now)
	if _, held := st.HeldPR("owner/repo", 1); !held {
		t.Error("a hold must match the round's repo spelling")
	}

	if !st.Unhold("owner/repo", 1) {
		t.Error("unhold must report that it released something")
	}
	if st.Unhold("owner/repo", 1) {
		t.Error("unholding twice must report nothing released")
	}
	if next := st.NextEligible(now); next == nil || next.PR != 1 {
		t.Fatalf("next = %#v, want PR 1 back in the queue", next)
	}
}
