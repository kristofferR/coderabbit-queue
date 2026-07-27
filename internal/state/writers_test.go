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
	if err := r.Complete(); err != nil {
		t.Fatal(err)
	}
	if err := r.Reopen(); err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseQueued {
		t.Fatalf("phase = %s, want the round requeued", r.Phase)
	}
	if r.LastAttemptAt != nil {
		t.Errorf("LastAttemptAt = %v, want it untouched by a reopen", r.LastAttemptAt)
	}
}
