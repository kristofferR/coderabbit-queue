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
	st.Leader = &LeaderLease{Owner: "old-mac", Token: "t", ExpiresAt: now.Add(time.Minute)}

	// A leader that has never announced a capability is exactly the old binary.
	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 1 || got[0] != "old-mac" {
		t.Fatalf("lagging = %v, want the leader named", got)
	}

	// Once it writes with the capability, it is no longer lagging.
	st.NoteWriter("old-mac", CapsRepoOverrides, now)
	if got := st.LaggingWriters(CapsRepoOverrides, now); len(got) != 0 {
		t.Errorf("lagging = %v, want none once the leader understands", got)
	}

	// A stale stamp does not count: the host may have been downgraded since.
	if got := st.LaggingWriters(CapsRepoOverrides, now.Add(2*time.Hour)); len(got) != 0 {
		// The lease has expired by then, so nobody is acting.
		t.Errorf("lagging = %v, want none when no lease is live", got)
	}
	st.Leader = &LeaderLease{Owner: "old-mac", Token: "t", ExpiresAt: now.Add(2*time.Hour + time.Minute)}
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
