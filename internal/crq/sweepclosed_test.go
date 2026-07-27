package crq

import (
	"context"
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// A merged pull request must leave the queue even when the round in front of it
// cannot fire.
//
// The pump examines NextEligible — the FRONT — and nothing else, so an
// account-blocked round there hid four merged PRs in the rendered queue for an
// afternoon: every pump reported the blocked round again rather than looking
// past it. The sweep is what reaches the rest.
func TestClosedRoundsLeaveTheQueueBehindABlockedOne(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	now := time.Now().UTC()

	// The front: open, and blocked by the account quota.
	var front ghapi.Pull
	front.State, front.Number, front.Head.SHA = "open", 1, "aaaaaaaa1"
	gh.pulls[fakeKey("owner/front", 1)] = front
	// Behind it: merged, so its round is dead work.
	var merged ghapi.Pull
	merged.State, merged.Number, merged.Head.SHA = "closed", 2, "bbbbbbbb1"
	gh.pulls[fakeKey("owner/behind", 2)] = merged

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/front", 1, "aaaaaaaa1", PhaseQueued, now.Add(-time.Hour), 0)
	seedRound(t, store, cfg, "owner/behind", 2, "bbbbbbbb1", PhaseQueued, now.Add(-time.Minute), 0)
	blocked := now.Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blocked
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// One pump is enough: the sweep runs before the front is chosen.
	if _, err := svc.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Abandoning archives the round, so it leaves Rounds entirely; an entry that
	// is still there must at least not be active.
	if r := st.Round("owner/behind", 2); r != nil && r.Active() {
		t.Errorf("the merged round is still in the queue: %+v", r)
	}
	if r := st.Round("owner/front", 1); r == nil || !r.Active() {
		t.Errorf("the blocked round was dropped too: %+v", r)
	}
}
