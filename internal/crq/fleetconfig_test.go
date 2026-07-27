package crq

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// One setting, one place. A host that carries its own answer for something the
// fleet has decided is how a repository ends up excluded on one machine and
// reviewed by another, with nothing to say so.
func TestFleetPolicyOverridesTheHostsOwn(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/local": true}
	cfg.MinInterval = time.Minute
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, "repos", "owner/fleet-a,owner/fleet-b"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetFleetConfig(ctx, "min-interval", "5m"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := svc.fleetCfg(st)
	if !got.AllowRepos["owner/fleet-a"] || !got.AllowRepos["owner/fleet-b"] {
		t.Errorf("repos = %v, want the fleet's list", got.AllowRepos)
	}
	if got.AllowRepos["owner/local"] {
		t.Error("the host's own repository list survived the fleet's")
	}
	if got.MinInterval != 5*time.Minute {
		t.Errorf("min-interval = %s, want the fleet's 5m", got.MinInterval)
	}

	// A setting the fleet has no opinion on stays this host's answer.
	if got.SettleWindow != cfg.SettleWindow {
		t.Errorf("settle = %s, want the host's %s when the fleet is silent", got.SettleWindow, cfg.SettleWindow)
	}

	// And unsetting hands it back.
	if dropped, err := svc.UnsetFleetConfig(ctx, "min-interval"); err != nil || !dropped {
		t.Fatalf("unset = %v %v", dropped, err)
	}
	st, _, _ = store.Load(ctx)
	if svc.fleetCfg(st).MinInterval != time.Minute {
		t.Error("unsetting a fleet setting did not return the host's own value")
	}
}

// A value only some binaries can read would break the fleet from the inside,
// so it is refused where it is set — and if one arrives anyway, from a newer
// crq, the host keeps what it has rather than acting on half a policy.
func TestFleetRefusesWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.MinInterval = 90 * time.Second
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, "min-interval", "later"); err == nil {
		t.Error("a duration that does not parse was accepted")
	}
	if err := svc.SetFleetConfig(ctx, "min-interval", "-5m"); err == nil {
		t.Error("a negative window was accepted")
	}
	if err := svc.SetFleetConfig(ctx, "nonsense", "1"); err == nil {
		t.Error("an unknown setting was accepted")
	}
	for _, key := range []string{"repos", "exclude"} {
		if err := svc.SetFleetConfig(ctx, key, "owner-repo"); err == nil {
			t.Errorf("%s accepted a malformed repository slug", key)
		}
	}
	if err := svc.SetFleetConfig(ctx, "required-bots", ""); err == nil {
		t.Error("an empty required reviewer set was accepted")
	}

	// Planted directly, as a newer binary would leave it.
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("min-interval", "a fortnight")
		st.SetFleetValue("from-the-future", "whatever")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if got := svc.fleetCfg(st).MinInterval; got != 90*time.Second {
		t.Errorf("min-interval = %s, want this host's 90s kept when the fleet's is unreadable", got)
	}
}

// Seeding is how an existing setup adopts this without retyping it, and it must
// not let a second machine overwrite the first one's answer.
func TestSeedingDoesNotOverwriteWhatTheFleetAlreadySays(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/from-this-host": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, "repos", "owner/decided-already"); err != nil {
		t.Fatal(err)
	}
	seeded, err := svc.SeedFleetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range seeded {
		if key == "repos" {
			t.Error("seeding overwrote a setting the fleet had already recorded")
		}
	}
	st, _, _ := store.Load(ctx)
	if v, _ := st.FleetValue("repos"); v != "owner/decided-already" {
		t.Errorf("repos = %q, want the fleet's existing answer", v)
	}
	// Everything else it had no answer for is now recorded.
	if v, ok := st.FleetValue("settle"); !ok || v == "" {
		t.Errorf("settle = %q %v, want this host's value seeded", v, ok)
	}
}

func TestFleetRequiredBotsRebuildsDerivedReviewers(t *testing.T) {
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	st := DefaultState(cfg)
	st.SetFleetValue("required-bots", "coderabbitai[bot],"+dialect.CodexBotLogin)

	got := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).fleetCfg(st)
	if !containsBot(got.RequiredBots, dialect.CodexBotLogin) {
		t.Fatalf("RequiredBots = %v, want fleet-required codex", got.RequiredBots)
	}
	if len(got.CoBots) != 1 || !got.CoBots[0].Required || got.CoBots[0].Trigger != "always" {
		t.Fatalf("CoBots = %+v, want required codex with its required trigger", got.CoBots)
	}
}

func TestFleetMutationsAreInertInDryRun(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("min-interval", "5m")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg.DryRun = true
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, "min-interval", "10m"); err != nil {
		t.Fatal(err)
	}
	if dropped, err := svc.UnsetFleetConfig(ctx, "min-interval"); err != nil || !dropped {
		t.Fatalf("dry-run unset = %v, %v; want the report without a write", dropped, err)
	}
	if seeded, err := svc.SeedFleetConfig(ctx); err != nil || len(seeded) == 0 {
		t.Fatalf("dry-run seed = %v, %v; want missing keys reported", seeded, err)
	}
	st, _, _ := store.Load(ctx)
	if value, _ := st.FleetValue("min-interval"); value != "5m" || len(st.FleetConfig) != 1 {
		t.Fatalf("dry run changed fleet state: %v", st.FleetConfig)
	}
}

func TestFleetDryRunStillChecksDriverCompatibility(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{Owner: "old-daemon", ExpiresAt: now.Add(time.Minute)}
		st.NoteWriter("old-daemon", CapsRepoOverrides, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg.DryRun = true
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	err := svc.SetFleetConfig(ctx, "min-interval", "5m")
	if err == nil || !strings.Contains(err.Error(), "lack fleet-policy support") {
		t.Fatalf("dry-run set with a lagging driver = %v, want a capability refusal", err)
	}
}

func TestFleetScopeChangeInvalidatesAccountQuota(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	blocked := time.Date(2026, 7, 27, 13, 0, 0, 0, time.UTC)
	remaining := 3
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blocked
		st.Account.Remaining = &remaining
		st.Account.CheckedAt = &blocked
		st.Account.CalibAskedAt = &blocked
		st.Account.RLCommentID = 42
		st.Account.RLCommentUpdated = &blocked
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := NewService(cfg, newFakeGitHub(), store, nil).SetFleetConfig(ctx, "scope", "other-org"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if st.Account.Scope != "other-org" {
		t.Fatalf("account scope = %q, want other-org", st.Account.Scope)
	}
	if st.Account.BlockedUntil != nil || st.Account.Remaining != nil || st.Account.CheckedAt != nil ||
		st.Account.CalibAskedAt != nil || st.Account.RLCommentID != 0 || st.Account.RLCommentUpdated != nil {
		t.Fatalf("old account quota survived the scope change: %+v", st.Account)
	}
}

func TestFleetExcludeRetiresQueuedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{
			Repo: "owner/repo", PR: 7, Head: "abcdef123",
			Phase: PhaseQueued, EnqueuedAt: time.Now().UTC(),
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := NewService(cfg, newFakeGitHub(), store, nil).SetFleetConfig(ctx, "exclude", "owner/repo"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if round := st.Round("owner/repo", 7); round != nil {
		t.Fatalf("excluded round remains fire-eligible: %+v", round)
	}
	if len(st.Archive) != 1 || st.Archive[0].Phase != PhaseAbandoned {
		t.Fatalf("excluded round was not archived as abandoned: %+v", st.Archive)
	}
}

func TestFleetDivergenceIncludesConfigFileValues(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.MinInterval = time.Minute
	cfg.ExplicitFleetEnv = map[string]bool{"CRQ_MIN_INTERVAL": true}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("min-interval", "5m")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := NewService(cfg, newFakeGitHub(), store, nil).FleetDivergence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "CRQ_MIN_INTERVAL") {
		t.Fatalf("divergence = %v, want the explicitly loaded config-file value", got)
	}
}

func TestFleetRevisionInvalidatesAStaleDecision(t *testing.T) {
	cfg := firingConfig()
	st := DefaultState(cfg)
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	snapshot := svc.cfgFor(st, "owner/repo")
	st.SetFleetValue("min-interval", "5m")
	if !overrideChanged(&st, "owner/repo", snapshot) {
		t.Error("a fleet policy change did not invalidate the earlier decision")
	}
}

func TestFleetSettleWindowShapesNext(t *testing.T) {
	cfg := firingConfig()
	cfg.SettleWindow = 0
	st := DefaultState(cfg)
	st.SetFleetValue("settle", "5m")
	effective := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).fleetCfg(st)
	evidence := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	got := settleUntil(FeedbackReport{LastEvidenceAt: evidence}, effective.SettleWindow)
	want := evidence.Add(5 * time.Minute)
	if got == nil || !got.Equal(want) {
		t.Fatalf("settle until = %v, want %s", got, want)
	}
}

func TestFleetPolicyRefusesALaggingQueueDriver(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{Owner: "old-daemon", ExpiresAt: now.Add(time.Minute)}
		st.NoteWriter("old-daemon", CapsRepoOverrides, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }
	if err := svc.SetFleetConfig(ctx, "min-interval", "5m"); err == nil || !strings.Contains(err.Error(), "lack fleet-policy support") {
		t.Fatalf("set with a lagging driver = %v, want a capability refusal", err)
	}
}

func TestFleetReviewerChangeReopensCompletedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	repo, pr := "owner/repo", 7
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: repo, PR: pr, Head: "abcdef123", Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)
	if err := svc.SetFleetConfig(ctx, "required-bots", "coderabbitai[bot],"+dialect.CodexBotLogin); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if round := st.Round(repo, pr); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %+v, want completed round reopened", round)
	}
}
