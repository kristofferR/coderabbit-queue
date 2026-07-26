package state

import (
	"testing"
	"time"
)

// A rate-limited round retries, and each retry posts a NEW review request. crq
// overwrote CommandID and forgot the old one, so nothing knew those comments
// existed — which is why a throttled PR collects a column of identical requests
// that tidying could never find.
func TestFireRemembersTheCommandItSuperseded(t *testing.T) {
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
	}
	fire(100)
	fire(200)
	fire(300)

	if len(r.SpentCommands) != 2 || r.SpentCommands[0] != 100 || r.SpentCommands[1] != 200 {
		t.Fatalf("spent = %v, want the two superseded commands", r.SpentCommands)
	}
	if r.CommandID != 300 {
		t.Errorf("CommandID = %d, want the newest", r.CommandID)
	}
	// Re-firing the same id is not a supersede.
	fire(300)
	if len(r.SpentCommands) != 2 {
		t.Errorf("spent = %v, want no change when the command is unchanged", r.SpentCommands)
	}

	// Bounded: a PR that retries all day must not grow its round without limit.
	for i := int64(1000); i < 1100; i++ {
		fire(i)
	}
	if len(r.SpentCommands) > 50 {
		t.Errorf("spent grew to %d entries", len(r.SpentCommands))
	}
}
