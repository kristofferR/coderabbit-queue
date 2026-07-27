package state

import (
	"strings"
	"testing"
	"time"
)

// The failure this exists for looked like success from outside: the watcher ran,
// the queue moved, PRs reported findings — and every dispatch died on a wedged
// git mirror, in a log line nobody read. It has to reach a surface someone looks
// at, and survive the unrelated progress that clears Warn.
func TestDrainHealthSurfacesAFailingDispatcher(t *testing.T) {
	now := time.Date(2026, 7, 26, 23, 0, 0, 0, time.UTC)
	st := New()
	st.Account.Scope = "owner"

	if st.Drain.Unhealthy() {
		t.Fatal("a fleet that has never dispatched is not unhealthy")
	}
	// One failure is a transient.
	st.NoteDispatch("cachyos", false, "checkout failed", now)
	if st.Drain.Unhealthy() {
		t.Error("one failure must not raise the alarm")
	}
	for i := 0; i < DrainUnhealthyAfter; i++ {
		st.NoteDispatch("cachyos", false, "refusing to fetch into branch", now)
	}
	if !st.Drain.Unhealthy() {
		t.Fatalf("after %d failures the dispatcher is not working", DrainUnhealthyAfter)
	}

	// The two surfaces a person actually looks at.
	if line := StatusLine(st, StoreConfig{}); !strings.Contains(line, "dispatch failing") {
		t.Errorf("status line = %q, want the failing dispatcher named", line)
	}
	dash := RenderDashboard(st, StoreConfig{Scope: []string{"owner"}})
	if !strings.Contains(dash, "fix sessions are not starting") || !strings.Contains(dash, "cachyos") {
		t.Errorf("dashboard does not report the failure:\n%s", dash)
	}

	// Unrelated progress clears Warn; it must NOT clear this, or the failure
	// disappears again the moment something else succeeds.
	st.Warn = ""
	if !st.Drain.Unhealthy() {
		t.Error("clearing Warn cleared the dispatch alarm")
	}

	// One success is recovery.
	st.NoteDispatch("cachyos", true, "", now.Add(time.Minute))
	if st.Drain.Unhealthy() {
		t.Error("a started session must clear the alarm")
	}
	if st.Drain.LastSuccessAt == nil {
		t.Error("recovery must be recorded")
	}
}

func TestDrainHealthStreaksAreIndependentPerHost(t *testing.T) {
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	st := New()
	for i := 0; i < DrainUnhealthyAfter; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		st.NoteDispatch("broken", false, "cannot start", at)
		st.NoteDispatch("healthy", true, "", at)
	}
	if got := st.Drains["broken"].ConsecutiveFailures; got != DrainUnhealthyAfter {
		t.Fatalf("broken host failures = %d, want %d", got, DrainUnhealthyAfter)
	}
	if !st.Drain.Unhealthy() || st.Drain.Host != "broken" {
		t.Fatalf("fleet summary = %+v, want the broken host retained", st.Drain)
	}

	loaded := State{Drains: st.Drains}
	loaded.Normalize(now)
	if loaded.Drain == nil || loaded.Drain.Host != "broken" {
		t.Fatalf("normalized summary = %+v, want it rebuilt from per-host records", loaded.Drain)
	}
}

func TestDrainHealthExpiresRetiredHosts(t *testing.T) {
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	st := New()
	for i := 0; i < DrainUnhealthyAfter; i++ {
		st.NoteDispatch("retired", false, "cannot start", now)
	}
	st.NoteDispatch("active", true, "", now.Add(DrainHealthTTL))

	if _, ok := st.Drains["retired"]; ok {
		t.Fatal("inactive retired host remained in dispatch health")
	}
	if st.Drain == nil || st.Drain.Host != "active" || st.Drain.Unhealthy() {
		t.Fatalf("fleet summary = %+v, want the active healthy host", st.Drain)
	}

	lastFailureAt := now
	loaded := State{Drains: map[string]DrainHealth{
		"retired": {
			Host:                "retired",
			ConsecutiveFailures: DrainUnhealthyAfter,
			LastFailureAt:       &lastFailureAt,
		},
	}}
	loaded.Normalize(now.Add(DrainHealthTTL))
	if loaded.Drain != nil || len(loaded.Drains) != 0 {
		t.Fatalf("normalization retained expired dispatch health: drain=%+v drains=%+v", loaded.Drain, loaded.Drains)
	}
}
