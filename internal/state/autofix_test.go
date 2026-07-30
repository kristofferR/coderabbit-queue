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
func TestAutofixHealthSurfacesAFailingDispatcher(t *testing.T) {
	now := time.Date(2026, 7, 26, 23, 0, 0, 0, time.UTC)
	st := New()
	st.Account.Scope = "owner"

	if st.Autofix.Unhealthy() {
		t.Fatal("a fleet that has never dispatched is not unhealthy")
	}
	// One failure is a transient.
	st.NoteDispatch("cachyos", false, "checkout failed", now)
	if st.Autofix.Unhealthy() {
		t.Error("one failure must not raise the alarm")
	}
	for i := 0; i < AutofixUnhealthyAfter; i++ {
		st.NoteDispatch("cachyos", false, "refusing to fetch into branch", now)
	}
	if !st.Autofix.Unhealthy() {
		t.Fatalf("after %d failures the dispatcher is not working", AutofixUnhealthyAfter)
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
	if !st.Autofix.Unhealthy() {
		t.Error("clearing Warn cleared the dispatch alarm")
	}

	// One success is recovery.
	st.NoteDispatch("cachyos", true, "", now.Add(time.Minute))
	if st.Autofix.Unhealthy() {
		t.Error("a started session must clear the alarm")
	}
	if st.Autofix.LastSuccessAt == nil {
		t.Error("recovery must be recorded")
	}
}

func TestAutofixHealthStreaksAreIndependentPerHost(t *testing.T) {
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	st := New()
	for i := 0; i < AutofixUnhealthyAfter; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		st.NoteDispatch("broken", false, "cannot start", at)
		st.NoteDispatch("healthy", true, "", at)
	}
	if got := st.AutofixByHost["broken"].ConsecutiveFailures; got != AutofixUnhealthyAfter {
		t.Fatalf("broken host failures = %d, want %d", got, AutofixUnhealthyAfter)
	}
	if !st.Autofix.Unhealthy() || st.Autofix.Host != "broken" {
		t.Fatalf("fleet summary = %+v, want the broken host retained", st.Autofix)
	}

	loaded := State{AutofixByHost: st.AutofixByHost}
	loaded.Normalize(now)
	if loaded.Autofix == nil || loaded.Autofix.Host != "broken" {
		t.Fatalf("normalized summary = %+v, want it rebuilt from per-host records", loaded.Autofix)
	}
}

func TestAutofixHealthExpiresRetiredHosts(t *testing.T) {
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	st := New()
	for i := 0; i < AutofixUnhealthyAfter; i++ {
		st.NoteDispatch("retired", false, "cannot start", now)
	}
	st.NoteDispatch("active", true, "", now.Add(AutofixHealthTTL))

	if _, ok := st.AutofixByHost["retired"]; ok {
		t.Fatal("inactive retired host remained in dispatch health")
	}
	if st.Autofix == nil || st.Autofix.Host != "active" || st.Autofix.Unhealthy() {
		t.Fatalf("fleet summary = %+v, want the active healthy host", st.Autofix)
	}

	lastFailureAt := now
	loaded := State{AutofixByHost: map[string]AutofixHealth{
		"retired": {
			Host:                "retired",
			ConsecutiveFailures: AutofixUnhealthyAfter,
			LastFailureAt:       &lastFailureAt,
		},
	}}
	loaded.Normalize(now.Add(AutofixHealthTTL))
	if loaded.Autofix != nil || len(loaded.AutofixByHost) != 0 {
		t.Fatalf("normalization retained expired dispatch health: autofix=%+v by_host=%+v", loaded.Autofix, loaded.AutofixByHost)
	}
}
