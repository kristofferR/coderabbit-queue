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
