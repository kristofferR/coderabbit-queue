package engine

import (
	"testing"
	"time"
)

// Every guard exists because removing the wrong comment costs a review. The
// expensive mistake is deleting one crq would otherwise have adopted: the next
// pump sees no command, posts another, and buys a second review of the same code.
func TestStaleCommandsKeepsWhatCrqStillReads(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	head := base.Add(30 * time.Minute)
	answered := map[string]time.Time{"coderabbitai": base.Add(40 * time.Minute)}

	in := TidyInput{
		HeadAt:     head,
		AnsweredAt: answered,
		Live:       map[int64]bool{5: true},
		Commands: []CommandComment{
			{ID: 1, Bot: "coderabbitai", CreatedAt: base},                       // stale: answered, old, not live
			{ID: 5, Bot: "coderabbitai", CreatedAt: base.Add(time.Minute)},      // live round depends on it
			{ID: 6, Bot: "codex", CreatedAt: base.Add(2 * time.Minute)},         // no evidence this bot ever acted
			{ID: 7, Bot: "coderabbitai", CreatedAt: head.Add(time.Minute)},      // newer than the head: still adoptable
			{ID: 8, Bot: "coderabbitai", CreatedAt: base.Add(50 * time.Minute)}, // after the answer AND after the head
		},
	}

	got := StaleCommands(in)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("stale = %v, want only the answered, superseded, non-live command", got)
	}

	// A retry the round explicitly replaced is spent even though it is newer
	// than the head: crq's own record that it posted a successor is stronger
	// evidence than the timestamp. This is the case that leaves a rate-limited
	// PR with a column of identical review requests.
	in.Superseded = map[int64]bool{8: true}
	got = StaleCommands(in)
	if len(got) != 2 || got[1] != 8 {
		t.Fatalf("stale = %v, want the superseded retry included", got)
	}
}

// A round that has not progressed keeps its command, whatever else is true.
func TestStaleCommandsNeverTouchesALiveRound(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	in := TidyInput{
		HeadAt:     base.Add(time.Hour),
		AnsweredAt: map[string]time.Time{"coderabbitai": base.Add(2 * time.Hour)},
		Live:       map[int64]bool{1: true, 2: true},
		Commands: []CommandComment{
			{ID: 1, Bot: "coderabbitai", CreatedAt: base},
			{ID: 2, Bot: "codex", CreatedAt: base},
		},
	}
	if got := StaleCommands(in); len(got) != 0 {
		t.Errorf("stale = %v, want nothing while the round is live", got)
	}
}

// With no head timestamp, the adoption guard cannot be evaluated, so it must not
// silently pass: an unreadable head is not permission to delete.
func TestStaleCommandsWithoutAHeadStillRequiresEvidence(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	in := TidyInput{
		AnsweredAt: map[string]time.Time{"coderabbitai": base.Add(time.Hour)},
		Commands:   []CommandComment{{ID: 1, Bot: "coderabbitai", CreatedAt: base}},
	}
	if got := StaleCommands(in); len(got) != 1 {
		t.Errorf("stale = %v, want the answered command", got)
	}
}
