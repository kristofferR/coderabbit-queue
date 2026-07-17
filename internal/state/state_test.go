package state

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func newFired(t *testing.T, s *State) Round {
	t.Helper()
	r, err := s.NewRound("owner/repo", 7, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.Fire(101, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.PutRound(*r)
	return *r
}

func TestHappyPathTransitions(t *testing.T) {
	s := New()
	r := newFired(t, &s)
	if r.Phase != PhaseFired || r.Attempts != 1 || r.CommandID != 101 {
		t.Fatalf("after fire: %+v", r)
	}
	if err := r.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	if err := r.Acknowledge(); err != nil {
		t.Fatalf("acknowledge must be idempotent: %v", err)
	}
	if err := r.Complete(); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseCompleted {
		t.Fatalf("phase = %s", r.Phase)
	}
}

// TestFiredHeadCannotRefire encodes the #448 invariant: once a head has
// fired, no transition path leads back to another Fire without an explicit
// retry window or a new head.
func TestFiredHeadCannotRefire(t *testing.T) {
	s := New()
	r := newFired(t, &s)

	var te *TransitionError
	if err := r.Fire(102, t0.Add(time.Minute)); !errors.As(err, &te) {
		t.Fatalf("double fire must be illegal, got %v", err)
	}
	if err := r.Reserve("tok2", "host", t0.Add(time.Minute)); !errors.As(err, &te) {
		t.Fatalf("re-reserve of a fired round must be illegal, got %v", err)
	}
	if r.FireEligible(t0.Add(time.Hour)) {
		t.Fatal("a fired round is never fire-eligible")
	}

	// The rate-limited path parks the round; it stays ineligible until the
	// window passes, and its history survives.
	retryAt := t0.Add(15 * time.Minute)
	if err := r.AwaitRetry(retryAt, "account rate limited", t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.FireEligible(retryAt.Add(-time.Second)) {
		t.Fatal("must not be eligible before RetryAt")
	}
	if !r.FireEligible(retryAt) {
		t.Fatal("must be eligible once RetryAt passes")
	}
	if r.Attempts != 1 || r.CommandID != 101 || r.Head != "abcdef123" {
		t.Fatalf("retry must keep fire history: %+v", r)
	}
	// Re-reserving for the retry keeps counting attempts.
	if err := r.Reserve("tok3", "host", retryAt); err != nil {
		t.Fatal(err)
	}
	if err := r.Fire(103, retryAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", r.Attempts)
	}
}

// TestDedupeCompletesFromQueued covers the "bot already reviewed the head"
// path: a queued round is marked complete without recording a fictitious fire,
// leaving it as the dedup marker.
func TestDedupeCompletesFromQueued(t *testing.T) {
	s := New()
	r, err := s.NewRound("owner/repo", 10, "abcdef123", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Dedupe(t0); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseCompleted || r.FiredAt != nil || r.Attempts != 0 {
		t.Fatalf("dedupe must complete without a fire: %+v", r)
	}
	// A fired round cannot be deduped — it goes through Complete.
	fired := newFired(t, &s)
	var te *TransitionError
	if err := fired.Dedupe(t0); !errors.As(err, &te) {
		t.Fatalf("dedupe of a fired round must be illegal, got %v", err)
	}
}

func TestIllegalCompletions(t *testing.T) {
	s := New()
	r, err := s.NewRound("owner/repo", 8, "cafebabe1", t0)
	if err != nil {
		t.Fatal(err)
	}
	var te *TransitionError
	if err := r.Complete(); !errors.As(err, &te) {
		t.Fatalf("completing a queued round must be illegal, got %v", err)
	}
	if err := r.Acknowledge(); !errors.As(err, &te) {
		t.Fatalf("acknowledging a queued round must be illegal, got %v", err)
	}
}

func TestReleaseToQueueKeepsAdoptionCutoff(t *testing.T) {
	s := New()
	r, _ := s.NewRound("owner/repo", 9, "abc123def", t0)
	if err := r.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseToQueue("post failed", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseQueued || r.Attempts != 1 {
		t.Fatalf("after release: %+v", r)
	}
	if r.LastAttemptAt == nil || !r.LastAttemptAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("adoption cutoff must advance: %+v", r.LastAttemptAt)
	}
}

func TestOneRoundPerPR(t *testing.T) {
	s := New()
	if _, err := s.NewRound("Owner/Repo", 7, "abcdef123", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewRound("owner/repo", 7, "00fedcba9", t0); err == nil {
		t.Fatal("second round for the same PR must be refused (case-insensitive key)")
	}
	r, err := s.Supersede("owner/repo", 7, "00fedcba9", t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if r.Head != "00fedcba9" || r.Phase != PhaseQueued || r.Seq != 2 {
		t.Fatalf("superseded round: %+v", r)
	}
	if len(s.Archive) != 1 || s.Archive[0].Phase != PhaseAbandoned || s.Archive[0].Head != "abcdef123" {
		t.Fatalf("old round must be archived abandoned: %+v", s.Archive)
	}
}

func TestSlotRoundStaleness(t *testing.T) {
	s := New()
	r := newFired(t, &s)
	s.FireSlot = &FireSlot{Key: Key(r.Repo, r.PR), Token: "tok", Since: t0}
	if s.SlotRound() == nil {
		t.Fatal("slot round must resolve")
	}
	s.FireSlot.Token = "stolen"
	if s.SlotRound() != nil {
		t.Fatal("token mismatch must read as stale")
	}
	s.Normalize(t0)
	if s.FireSlot != nil {
		t.Fatal("Normalize must clear a stale slot")
	}
}

func TestNextEligibleOrdersBySeq(t *testing.T) {
	s := New()
	a, _ := s.NewRound("owner/repo", 1, "aaaaaaaa1", t0)
	s.PutRound(*a)
	b, _ := s.NewRound("owner/repo", 2, "bbbbbbbb2", t0)
	s.PutRound(*b)
	// Round a parks awaiting retry; b becomes the eligible head of queue.
	ra := s.Round("owner/repo", 1)
	if err := ra.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := ra.Fire(1, t0); err != nil {
		t.Fatal(err)
	}
	if err := ra.AwaitRetry(t0.Add(10*time.Minute), "rate limited", t0); err != nil {
		t.Fatal(err)
	}
	s.PutRound(*ra)

	if got := s.NextEligible(t0.Add(time.Minute)); got == nil || got.PR != 2 {
		t.Fatalf("expected PR 2 eligible, got %+v", got)
	}
	// Once the window passes, the older round wins again by Seq.
	if got := s.NextEligible(t0.Add(11 * time.Minute)); got == nil || got.PR != 1 {
		t.Fatalf("expected PR 1 eligible after retry window, got %+v", got)
	}
}

func TestArchiveBounded(t *testing.T) {
	s := New()
	for i := 0; i < ArchiveMax+10; i++ {
		if _, err := s.NewRound("owner/repo", i, "abcdef123", t0); err != nil {
			t.Fatal(err)
		}
		s.EndRound("owner/repo", i, "closed")
	}
	if len(s.Archive) != ArchiveMax {
		t.Fatalf("archive = %d, want %d", len(s.Archive), ArchiveMax)
	}
}
