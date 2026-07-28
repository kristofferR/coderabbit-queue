package state

import (
	"testing"
	"time"
)

// Sharing a state ref stops an older binary ERASING a new field. It does not
// make that binary act on it: it loads the field as unknown JSON, writes it back
// untouched, and keeps deciding from its own fleet-wide configuration. So the
// fleet has to be able to say who is driving and what they understand.
func TestLaggingWritersNamesWhoWillIgnoreTheOverride(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	st.Leader = &LeaderLease{Owner: "host=old-mac pid=7", Token: "t", ExpiresAt: now.Add(time.Minute)}

	// A leader that has never announced a capability is exactly the old binary.
	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 1 || got[0] != "host=old-mac pid=7" {
		t.Fatalf("lagging = %v, want the leader named", got)
	}

	// Once it writes with the capability, it is no longer lagging.
	st.NoteWriter("host=old-mac pid=7", CapsRepoOverrides, now)
	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 0 {
		t.Errorf("lagging = %v, want none once the leader understands", got)
	}

	// A stale stamp does not count: the host may have been downgraded since.
	if got := st.LaggingWriters(CapsRepoOverrides, now.Add(2*time.Hour)); len(got) != 0 {
		// The lease has expired by then, so nobody is acting.
		t.Errorf("lagging = %v, want none when no lease is live", got)
	}
	st.Leader = &LeaderLease{Owner: "host=old-mac pid=7", Token: "t", ExpiresAt: now.Add(2*time.Hour + time.Minute)}
	if got := st.LaggingWriters(CapsRepoOverrides, now.Add(2*time.Hour)); len(got) != 1 {
		t.Errorf("lagging = %v, want the leader named again once its stamp is stale", got)
	}

	// Writers are bounded: a host silent for a day is not part of the fleet.
	st.NoteWriter("other", CapsRepoOverrides, now)
	st.NoteWriter("fresh", CapsRepoOverrides, now.Add(25*time.Hour))
	if _, ok := st.Writers["other"]; ok {
		t.Errorf("writers = %v, want the day-old host pruned", st.Writers)
	}
}

// The leader records itself as "host=<name> pid=<n>" while capabilities are
// keyed by host alone. Comparing those directly reported every current-version
// daemon as needing an upgrade — the warning would have been pure noise.
func TestLaggingWritersMatchesTheLeadersProcessIdentity(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	st.Leader = &LeaderLease{Owner: "host=cachyos pid=1234", ExpiresAt: now.Add(time.Minute)}
	st.NoteWriter("host=cachyos pid=1234", CapsRepoOverrides, now)

	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 0 {
		t.Errorf("lagging = %v, want none — the leader IS the capable writer", got)
	}

	// A new CLI on the same machine must not vouch for an old daemon: that is
	// the ordinary upgrade, and per-host keying would hide exactly it.
	st.NoteWriter("host=cachyos pid=9999", CapsRepoOverrides, now)
	st.Leader = &LeaderLease{Owner: "host=cachyos pid=1", ExpiresAt: now.Add(time.Minute)}
	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 1 {
		t.Errorf("lagging = %v, want the un-upgraded daemon named", got)
	}
}

// The other acting process is whoever holds the fire slot, and a round records
// that process in ByHost. Recording a bare hostname there could never match a
// writer entry, so every `crq reviewers` call during a fire named the current
// process as lagging — telling operators to upgrade a binary that already
// understands overrides.
func TestLaggingWritersMatchesTheFireSlotOwner(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	writer := "host=cachyos pid=1234"
	st := New()
	r, err := st.NewRound("owner/repo", 7, "abcdef123", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("tok", writer, now); err != nil {
		t.Fatal(err)
	}
	st.PutRound(*r)
	st.FireSlot = &FireSlot{Key: Key("owner/repo", 7), Token: "tok", Since: now}

	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 1 || got[0] != writer {
		t.Fatalf("lagging = %v, want the un-announced slot owner named", got)
	}
	st.NoteWriter(writer, CapsRepoOverrides, now)
	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 0 {
		t.Errorf("lagging = %v, want none — the process firing IS the capable writer", got)
	}
}

// Reopening a round is not a failed attempt. Moving LastAttemptAt would raise
// the adoption floor past a newly required co-reviewer's own unanswered trigger,
// so crq would post that bot a second request for the round it is reopening to
// let it answer.
func TestReopenKeepsTheAdoptionFloor(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	r := Round{Repo: "owner/repo", PR: 7, Head: "abcdef123", Phase: PhaseFired,
		FiredAt: &now, CommandID: 11}
	floor := now.Add(-time.Hour)
	r.LastAttemptAt = &floor
	if err := r.Complete(); err != nil {
		t.Fatal(err)
	}
	if err := r.Reopen(); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseQueued {
		t.Fatalf("phase = %s, want the round requeued", r.Phase)
	}
	if r.LastAttemptAt == nil || !r.LastAttemptAt.Equal(floor) {
		t.Errorf("LastAttemptAt = %v, want it untouched by a reopen", r.LastAttemptAt)
	}
}

// The host that consumes solver settings is the autofix watcher, and it holds
// neither the leader lease nor the fire slot — so the acting set LaggingWriters
// builds cannot see it. An old watcher went on dispatching install-time model,
// fork and attempt values while the repository's page reported nobody lagging.
func TestLaggingRoleWritersNamesTheAutofixWatcher(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	st.SetHostReport(HostReport{Host: "atlas", Caps: CapsSolver - 1, Roles: []string{"autofix"}}, now)
	st.SetHostReport(HostReport{Host: "blue", Caps: CapsSolver, Roles: []string{"autofix"}}, now)
	// Running something else here, so its older binary is not this setting's
	// problem: it never starts a fix session.
	st.SetHostReport(HostReport{Host: "green", Caps: CapsSolver - 1, Roles: []string{"serve"}}, now)

	if got := st.LaggingWriters(CapsSolver, now); len(got) != 0 {
		t.Fatalf("lagging writers = %v, want none — no host is driving the queue", got)
	}
	got := st.LaggingRoleWriters(CapsSolver, now, "autofix")
	if len(got) != 1 || got[0] != "atlas" {
		t.Fatalf("lagging = %v, want only the old autofix host", got)
	}

	// A report that has aged out speaks for nobody: that machine may not be
	// running a watcher at all any more.
	if got := st.LaggingRoleWriters(CapsSolver, now.Add(2*HostReportTTL), "autofix"); len(got) != 0 {
		t.Errorf("lagging = %v, want none once every report is stale", got)
	}

	// And a machine lagging in BOTH registers is listed once, under the writer
	// identity that says which process it is.
	st.Leader = &LeaderLease{Owner: "host=atlas pid=7", ExpiresAt: now.Add(time.Minute)}
	got = st.LaggingRoleWriters(CapsSolver, now, "autofix")
	if len(got) != 1 || got[0] != "host=atlas pid=7" {
		t.Errorf("lagging = %v, want the machine named once", got)
	}
}

// One machine runs its services on two builds for as long as a rolling upgrade
// takes, and each writes the SAME record. Reading the record's own capabilities
// let whichever wrote last answer for both: a current `serve` heartbeat vouched
// for an old `autofix` watcher that ignores the very setting being saved.
func TestLaggingRoleWritersReadsEachRolesOwnCapabilities(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	st.SetHostReport(HostReport{Host: "atlas", Caps: CapsSolver - 1, Roles: []string{"autofix"}}, now)
	// The upgraded dashboard on the same machine, reporting after it.
	st.SetHostReport(HostReport{Host: "atlas", Caps: CapsSolver, Roles: []string{"serve"}}, now)

	if got := st.LaggingRoleWriters(CapsSolver, now, "autofix"); len(got) != 1 || got[0] != "atlas" {
		t.Fatalf("lagging = %v, want the machine named for its old watcher", got)
	}
	// Nothing is claimed about the role that never reported an old binary.
	if got := st.LaggingRoleWriters(CapsSolver, now, "serve"); len(got) != 0 {
		t.Errorf("lagging = %v, want none — the dashboard here is current", got)
	}
	// Upgrading the watcher clears it, without the dashboard having to write.
	st.SetHostReport(HostReport{Host: "atlas", Caps: CapsSolver, Roles: []string{"autofix"}}, now)
	if got := st.LaggingRoleWriters(CapsSolver, now, "autofix"); len(got) != 0 {
		t.Errorf("lagging = %v, want none once the watcher itself reports the capability", got)
	}
}
