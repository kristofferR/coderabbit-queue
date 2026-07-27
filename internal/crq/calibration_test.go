package crq

import (
	"context"
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// The calibration probe is the one writer that replaced the whole quota, so it
// was also the one that could SHORTEN a standing block. That matters more now
// that a PR's own rate-limit notice records one: a probe whose reply carries no
// parseable reset would erase the window CodeRabbit had just stated, and Pump
// would fire inside it.
func TestCalibrationNeverShortensAStandingBlock(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CalibrationPR = 77
	cfg.GateRepo = "o/state"
	cfg.CalibrationTTL = time.Minute
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	// A calibration reply that says nothing about a reset — the parse miss.
	reply := ghapi.IssueComment{ID: 5, Body: "auto-generated reply by CodeRabbit\nnothing parseable here",
		CreatedAt: now.Add(-10 * time.Second), UpdatedAt: now.Add(-10 * time.Second)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey(cfg.GateRepo, 77)] = []ghapi.IssueComment{reply}

	// A PR's own notice already told crq the account is blocked for 40 minutes.
	until := now.Add(40 * time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &until
		st.Account.RLCommentID = 900
		u := now.Add(-time.Minute)
		st.Account.RLCommentUpdated = &u
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.BlockedUntil == nil {
		t.Fatal("the probe erased a block CodeRabbit had stated; Pump would fire inside the window")
	}
	if !got.Account.BlockedUntil.Equal(until) {
		t.Errorf("blocked until %s, want the standing %s", got.Account.BlockedUntil, until)
	}

	// A LONGER window from the probe is new information and still wins.
	longer := now.Add(2 * time.Hour)
	gh.comments[fakeKey(cfg.GateRepo, 77)] = []ghapi.IssueComment{{
		ID: 6, Body: "auto-generated reply by CodeRabbit\n> **Next review available in:** **120 minutes**",
		CreatedAt: now, UpdatedAt: now,
		User: reply.User,
	}}
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.CheckedAt = nil // force a re-read
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.RefreshQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.BlockedUntil == nil || got.Account.BlockedUntil.Before(longer.Add(-2*time.Minute)) {
		t.Errorf("blocked until %v, want the probe's longer window near %s", got.Account.BlockedUntil, longer)
	}
}
