package state

import (
	"testing"
	"time"
)

// A rate-limited round retries, and each retry posts a NEW review request. crq
// overwrote CommandID and forgot the old one, so nothing knew those comments
// existed — which is why a throttled PR collects a column of identical requests
// that tidying could never find.
func TestRecordPostedRemembersEveryCommandCrqWrote(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	r := &Round{Repo: "o/r", PR: 1, Head: "aaaaaaaa1", Phase: PhaseQueued}

	fire := func(id int64) {
		t.Helper()
		if r.Phase != PhaseQueued {
			if err := r.AwaitRetry(now, "rate limited", now); err != nil {
				t.Fatal(err)
			}
			r.Phase = PhaseQueued
		}
		if err := r.Reserve("tok", "host", now); err != nil {
			t.Fatal(err)
		}
		if err := r.Fire(id, now); err != nil {
			t.Fatal(err)
		}
		r.RecordPosted("coderabbitai", id, now)
	}
	fire(100)
	fire(200)
	fire(300)

	if len(r.PostedCommands) != 3 || r.PostedCommands[0].ID != 100 || r.PostedCommands[2].ID != 300 {
		t.Fatalf("posted = %v, want every request this round wrote", r.PostedCommands)
	}
	if r.PostedCommands[0].Bot != "coderabbitai" {
		t.Errorf("bot = %q, want the reviewer the request was addressed to", r.PostedCommands[0].Bot)
	}
	if r.CommandID != 300 {
		t.Errorf("CommandID = %d, want the newest", r.CommandID)
	}
	// Re-recording the same id is not a second comment.
	fire(300)
	if len(r.PostedCommands) != 3 {
		t.Errorf("posted = %v, want no change when the command is unchanged", r.PostedCommands)
	}

	// Bounded: a PR that retries all day must not grow its round without limit.
	for i := int64(1000); i < 1100; i++ {
		fire(i)
	}
	if len(r.PostedCommands) > 50 {
		t.Errorf("posted grew to %d entries", len(r.PostedCommands))
	}
}

// A round records an ADOPTED command in exactly the same CommandID, so only the
// posted list can tell the two apart — and getting that wrong means deleting a
// person's request to review.
func TestFireAloneClaimsNoAuthorship(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	r := &Round{Repo: "o/r", PR: 1, Head: "aaaaaaaa1", Phase: PhaseQueued}
	if err := r.Fire(77, now); err != nil {
		t.Fatal(err)
	}
	r.SetCoCommand("chatgpt-codex-connector", 78, now)
	if len(r.PostedCommands) != 0 {
		t.Fatalf("posted = %v, want nothing: crq wrote neither comment", r.PostedCommands)
	}
}
