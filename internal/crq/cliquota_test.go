package crq

import (
	"context"
	"testing"
	"time"
)

func blockedReport(wait string) PreflightReport {
	return PreflightReport{
		Status:      "rate_limited",
		Error:       "Rate limit exceeded",
		ErrorType:   "rate_limit",
		Recoverable: true,
		RetryAfter:  wait,
	}
}

func cliQuotaService(t *testing.T, now time.Time) (*Service, StateStore) {
	t.Helper()
	cfg := firingConfig()
	cfg.GateRepo = "kristofferR/crq-state"
	cfg.Scope = []string{"kristofferR"}
	cfg.RateLimitFallback = 15 * time.Minute
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }
	return svc, store
}

// The local CLI spends the same account quota the queue serializes, so a local
// block is evidence about that quota — obtained with no probe comment and no
// GitHub round trip.
func TestRecordCLIQuotaAppliesTheBlock(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	got, err := svc.RecordCLIQuota(context.Background(), blockedReport("32 minutes"), "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Applied {
		t.Fatalf("the block must be recorded: %+v", got)
	}
	st, _, _ := store.Load(context.Background())
	want := now.Add(32 * time.Minute)
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(want) {
		t.Errorf("BlockedUntil = %v, want %s", st.Account.BlockedUntil, want)
	}
	if st.Account.Source != "coderabbit-cli" {
		t.Errorf("Source = %q, want coderabbit-cli", st.Account.Source)
	}
}

// The CLI can be signed in to a different CodeRabbit organisation than the one
// crq queues for. Applying that block would stall reviews for an account that is
// not limited at all, so an unknown or foreign org must be refused — and refused
// with a reason, not silently.
func TestRecordCLIQuotaRefusesAnotherAccount(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	for _, org := range []string{"someone-else", ""} {
		svc, store := cliQuotaService(t, now)
		got, err := svc.RecordCLIQuota(context.Background(), blockedReport("32 minutes"), org)
		if err != nil {
			t.Fatal(err)
		}
		if got.Applied {
			t.Errorf("org %q must not apply to this account", org)
		}
		if got.Reason == "" {
			t.Errorf("org %q must be refused with a reason", org)
		}
		st, _, _ := store.Load(context.Background())
		if st.Account.BlockedUntil != nil {
			t.Errorf("org %q left a block behind: %s", org, st.Account.BlockedUntil)
		}
	}
}

// A window read from a PR comment is authoritative about the whole account; a
// local reading may be a narrower limit. Extending is safe, shortening is not.
func TestRecordCLIQuotaNeverShortensAStandingBlock(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)
	longer := now.Add(2 * time.Hour)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Account.BlockedUntil = &longer
		st.Account.Source = "calibrate"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.RecordCLIQuota(context.Background(), blockedReport("5 minutes"), "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if got.Applied {
		t.Error("a shorter local window must not replace a longer standing block")
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(longer) {
		t.Errorf("BlockedUntil = %v, want the longer standing block %s", st.Account.BlockedUntil, longer)
	}
}

// "Blocked, but I can't tell you for how long" must not read as "not blocked":
// treating an unreadable window as clear is what let the daemon re-fire every
// couple of minutes against a limit measured in tens of minutes.
func TestRecordCLIQuotaFallsBackWhenTheWindowIsUnreadable(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	got, err := svc.RecordCLIQuota(context.Background(), blockedReport("soon"), "kristofferR")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Applied {
		t.Fatalf("an unreadable window must still block: %+v", got)
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(now.Add(15*time.Minute)) {
		t.Errorf("BlockedUntil = %v, want the conservative fallback", st.Account.BlockedUntil)
	}
}

// Anything that is not an account block leaves the shared quota alone.
func TestRecordCLIQuotaIgnoresOtherFailures(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, store := cliQuotaService(t, now)

	report := blockedReport("32 minutes")
	report.ErrorType = "auth"
	if got, err := svc.RecordCLIQuota(context.Background(), report, "kristofferR"); err != nil {
		t.Fatal(err)
	} else if got.Applied {
		t.Error("a non-quota failure must not touch the account block")
	}
	st, _, _ := store.Load(context.Background())
	if st.Account.BlockedUntil != nil {
		t.Errorf("BlockedUntil = %s, want none", st.Account.BlockedUntil)
	}
}
