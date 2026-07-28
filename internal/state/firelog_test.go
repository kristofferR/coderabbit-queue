package state

import (
	"testing"
	"time"
)

// The fire log's only job is forecasting the weekly fair-use cliff, so what it
// must never do is state a confident weekly number it does not have the history
// for. These rows pin that, and the trimming that keeps the log bounded.
func TestFireLogAndFairUse(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	s := New()

	// A log younger than a week reports a FLOOR, not a weekly count.
	for i := 0; i < 10; i++ {
		s.NoteFire(now.Add(-time.Duration(i) * time.Hour))
	}
	u := s.FairUse(now, 60)
	if u.Fires != 10 || u.Complete {
		t.Fatalf("usage = %+v, want 10 fires and an incomplete week", u)
	}
	if u.Level != "ok" {
		t.Errorf("level = %q, want ok at 10 of 60", u.Level)
	}

	// Old entries fall out of the window; the count follows.
	s.NoteFire(now.Add(-20 * 24 * time.Hour))
	if got := len(s.Account.Fires); got != 10 {
		t.Errorf("log = %d entries, want the out-of-window fire dropped", got)
	}

	// Once the log reaches back a full week the count is complete.
	s.NoteFire(now.Add(-8 * 24 * time.Hour))
	if u = s.FairUse(now, 60); !u.Complete {
		t.Error("a log starting more than a week ago covers the rolling week")
	}
	if u.Fires != 10 {
		t.Errorf("fires = %d, want the 8-day-old entry outside the 7-day count", u.Fires)
	}

	// The warning band opens at 80% — early enough to act on, not so early it
	// is background noise.
	busy := New()
	for i := 0; i < 48; i++ {
		busy.NoteFire(now.Add(-time.Duration(i) * time.Hour))
	}
	if u = busy.FairUse(now, 60); u.Level != "warn" {
		t.Errorf("level = %q at 48 of 60, want warn", u.Level)
	}
	for i := 48; i < 60; i++ {
		busy.NoteFire(now.Add(-time.Duration(i) * time.Hour))
	}
	if u = busy.FairUse(now, 60); u.Level != "over" {
		t.Errorf("level = %q at 60 of 60, want over", u.Level)
	}

	// With no threshold configured it still counts, but claims nothing.
	if u = busy.FairUse(now, 0); u.Level != "ok" || u.Fires != 60 {
		t.Errorf("usage = %+v, want a count with no verdict", u)
	}

	// The cap holds whatever the window allows.
	huge := New()
	for i := 0; i < FireLogMax+50; i++ {
		huge.NoteFire(now.Add(-time.Duration(i) * time.Minute))
	}
	if got := len(huge.Account.Fires); got != FireLogMax {
		t.Errorf("log = %d entries, want it capped at %d", got, FireLogMax)
	}
}
