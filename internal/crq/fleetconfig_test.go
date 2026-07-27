package crq

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
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
		// A slug GitHub can never return is a rule covering nothing, whether it
		// is missing the slash or holds a character no repository name can.
		for _, slug := range []string{"owner-repo", "owner/re po", "owner/re\tpo", "owner/repo?"} {
			if err := svc.SetFleetConfig(ctx, key, slug); err == nil {
				t.Errorf("%s accepted the malformed repository slug %q", key, slug)
			}
		}
	}
	// A scope entry is an owner login, not a slug: autoreview hands each one to
	// EachOpenPR as a user or organisation name, so a repository typed here — or
	// anything else GitHub cannot resolve — fails every pass, on every host at
	// once, because the value is fleet-wide.
	for _, owner := range []string{"owner/repo", "own er", "owner?", ".."} {
		if err := svc.SetFleetConfig(ctx, "scope", owner); err == nil {
			t.Errorf("scope accepted the malformed owner %q", owner)
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

func TestSeedingValidatesEveryMissingValueBeforeWriting(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"not-a-repository": true}
	store := NewMemoryStore(cfg)

	seeded, err := NewService(cfg, newFakeGitHub(), store, nil).SeedFleetConfig(ctx)
	if err == nil || !strings.Contains(err.Error(), "cannot seed repos") {
		t.Fatalf("seed = %v, %v; want the malformed host repository rejected", seeded, err)
	}
	st, _, loadErr := store.Load(ctx)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(st.FleetConfig) != 0 {
		t.Fatalf("failed seed partially wrote fleet policy: %v", st.FleetConfig)
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

// Which co-reviewers run, how crq drives them, and whether an account-blocked
// round degrades to them are decisions, not machine facts. Two hosts answering
// them differently is the same divergence as one excluding a repository the
// other reviews: both behave correctly, and the PRs disagree.
func TestFleetOwnsTheCoReviewerPolicy(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":              "codex",
		"CRQ_REQUIRED_BOTS":       "coderabbitai[bot]",
		"CRQ_COBOT_BUGBOT_CMD":    "bugbot run",
		"CRQ_COBOT_BUGBOT_GRACE":  "10m",
		"CRQ_COBOT_CODEX_TRIGGER": "always",
		"CRQ_RL_CO_DEGRADE":       "0",
	})
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	for _, set := range []struct{ key, value string }{
		{"cobots", "bugbot"},
		{"cobot-bugbot-trigger", "always"},
		{"cobot-bugbot-cmd", "bugbot run now"},
		{"cobot-bugbot-grace", "30m"},
		{"rate-limit-co-degrade", "1"},
	} {
		if err := svc.SetFleetConfig(ctx, set.key, set.value); err != nil {
			t.Fatalf("set %s: %v", set.key, err)
		}
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := svc.fleetCfg(st)

	if len(got.CoBots) != 1 || got.CoBots[0].Name != "bugbot" {
		t.Fatalf("CoBots = %+v, want only the fleet's bugbot", got.CoBots)
	}
	bugbot := got.CoBots[0]
	if bugbot.Trigger != engine.TriggerAlways || bugbot.Command != "bugbot run now" || bugbot.SelfHealGrace != 30*time.Minute {
		t.Errorf("bugbot = %+v, want the fleet's trigger, command and grace", bugbot)
	}
	if !got.RateLimitCoDegrade {
		t.Error("the fleet turned co-reviewer degradation on and this host stayed off")
	}
	// The derived views have to follow, or which co-reviewer runs depends on
	// which view crq happens to ask.
	var seen bool
	for _, r := range got.Reviewers {
		if !sameBot(r.Login, bugbot.Login) {
			continue
		}
		seen = true
		if r.Trigger != engine.TriggerAlways || r.Command != "bugbot run now" {
			t.Errorf("reviewer %+v disagrees with the fleet's co-reviewer policy", r)
		}
	}
	if !seen {
		t.Errorf("Reviewers = %+v, want the fleet's co-reviewer among them", got.Reviewers)
	}
}

// A value only some hosts can read breaks the fleet from the inside, so the
// co-reviewer settings are refused where they are set like every other one.
func TestFleetRefusesCoReviewerPolicyItCannotRead(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	for _, bad := range []struct{ key, value string }{
		{"cobots", "codex,nosuchbot"},
		{"cobot-codex-trigger", "sometimes"},
		{"cobot-codex-grace", "-5m"},
		{"rate-limit-co-degrade", "maybe"},
	} {
		if err := svc.SetFleetConfig(ctx, bad.key, bad.value); err == nil {
			t.Errorf("%s accepted %q", bad.key, bad.value)
		}
	}
	// Explicitly none is a real answer, not a malformed one.
	if err := svc.SetFleetConfig(ctx, "cobots", ""); err != nil {
		t.Errorf("cobots refused an empty set: %v", err)
	}
}

// Enabling a co-reviewer fleet-wide is a reviewer change like requiring one:
// the heads already reviewed were reviewed without it.
func TestFleetCoBotChangeReopensCompletedRounds(t *testing.T) {
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
	if err := svc.SetFleetConfig(ctx, "cobots", "codex,bugbot"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	round := st.Round(repo, pr)
	if round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %+v, want the completed round reopened for the new co-reviewer", round)
	}
	if !containsBot(round.ForceCoReviewers, dialect.BugbotLogin) {
		t.Errorf("ForceCoReviewers = %v, want the newly enabled self-heal bot nudged once", round.ForceCoReviewers)
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
		st.NoteWriter("old-daemon", CapsFleetPolicy-1, now)
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

func TestEquivalentFleetScopeKeepsAccountQuota(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"Acme", "Foo"}
	store := NewMemoryStore(cfg)
	blocked := time.Date(2026, 7, 27, 13, 0, 0, 0, time.UTC)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blocked
		st.Account.Scope = "Acme,Foo"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := NewService(cfg, newFakeGitHub(), store, nil).SetFleetConfig(ctx, "scope", "foo,acme"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Account.BlockedUntil == nil || !st.Account.BlockedUntil.Equal(blocked) {
		t.Fatalf("equivalent scope cleared the account block: %+v", st.Account)
	}
}

func TestFleetInflightTimeoutOverridesTheHost(t *testing.T) {
	cfg := firingConfig()
	cfg.InflightTimeout = 15 * time.Minute
	st := DefaultState(cfg)
	st.SetFleetValue("inflight-timeout", "45m")

	got := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil).fleetCfg(st)
	if got.InflightTimeout != 45*time.Minute {
		t.Fatalf("inflight timeout = %s, want fleet 45m", got.InflightTimeout)
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

// A reservation is a fire's commit point, but the review command reaches GitHub
// after it. Excluding the repository in that window cannot take the post back:
// retiring the round would only destroy the record of the quota the account is
// about to be charged for, on a repository crq is no longer meant to touch.
func TestFleetExcludeRefusesToRaceAClaimedTriggerPost(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "owner/repo", PR: 7, Head: "abcdef123", Phase: PhaseReserved, Token: "tok"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewService(cfg, newFakeGitHub(), store, nil)
	err := svc.SetFleetConfig(ctx, "exclude", "owner/repo")
	if err == nil || !strings.Contains(err.Error(), "already being posted") {
		t.Fatalf("exclude during a claimed trigger post = %v, want a refusal", err)
	}
	st, _, _ := store.Load(ctx)
	if value, ok := st.FleetValue("exclude"); ok {
		t.Errorf("exclude = %q, want the refused change not written", value)
	}
	if round := st.Round("owner/repo", 7); round == nil || round.Phase != PhaseReserved {
		t.Fatalf("round = %+v, want the reserved round left alone", round)
	}

	// A co-reviewer claim is the same promise, and both clear once the post is
	// recorded — then the exclusion goes through.
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round("owner/repo", 7)
		r.Phase = PhaseQueued
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetFleetConfig(ctx, "exclude", "owner/repo"); err != nil {
		t.Fatalf("exclude after the post finished = %v, want it recorded", err)
	}
}

// Whose findings crq reads decides whether a head comes back as `fix` or as
// clean, which makes it fleet policy: one queue driver reporting findings the
// next considers absent is the same divergence as one host excluding a
// repository the other reviews.
func TestFleetOwnsTheFeedbackBots(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
		"CRQ_FEEDBACK_BOTS": "coderabbitai[bot]",
	})
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, feedbackBotsKey, "coderabbitai[bot],"+dialect.CodexBotLogin); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	got := svc.fleetCfg(st)
	if !containsBot(got.FeedbackBots, dialect.CodexBotLogin) {
		t.Errorf("FeedbackBots = %v, want the fleet's list to win over this host's", got.FeedbackBots)
	}
	// And `crq doctor` names the variable still set locally, or the host looks
	// healthy while it reads a different set.
	diverged, err := svc.FleetDivergence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(diverged, "\n"), "CRQ_FEEDBACK_BOTS") {
		t.Errorf("divergence = %v, want the overridden host variable named", diverged)
	}

	// Recording the empty value is the fleet saying "derive it", which has to
	// recompute the surfaced set rather than leave this host's explicit one.
	if err := svc.SetFleetConfig(ctx, feedbackBotsKey, ""); err != nil {
		t.Fatal(err)
	}
	st, _, _ = store.Load(ctx)
	got = svc.fleetCfg(st)
	if got.FeedbackBotsExplicit || !containsBot(got.FeedbackBots, dialect.CodexBotLogin) {
		t.Fatalf("FeedbackBots = %v (explicit %v), want the derived set including the enabled co-reviewer",
			got.FeedbackBots, got.FeedbackBotsExplicit)
	}
}

// Seeding writes THIS host's answers, so comparing the reviewer policy it
// records against the policy this host was already using can only ever say
// "nothing changed" — while the host that completed the head may have been
// running an entirely different set. That divergence is what seeding exists to
// end, so a seeded reviewer key reconciles as if the fleet had no reviewers.
func TestSeedingReviewerPolicyReopensCompletedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_REPOS":                "",
		"CRQ_EXCLUDE":              "",
		"CRQ_COBOTS":               "codex,bugbot",
		"CRQ_REQUIRED_BOTS":        "coderabbitai[bot]",
		"CRQ_COBOT_BUGBOT_CMD":     "bugbot run",
		"CRQ_COBOT_BUGBOT_TRIGGER": "selfheal",
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

	if _, err := NewService(cfg, gh, store, nil).SeedFleetConfig(ctx); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	round := st.Round(repo, pr)
	if round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %+v, want the completed round reopened by the seed", round)
	}
	if !containsBot(round.ForceCoReviewers, dialect.BugbotLogin) {
		t.Errorf("ForceCoReviewers = %v, want the seeded self-heal bot nudged once", round.ForceCoReviewers)
	}
}

// The first `crq config set` of a key is the same adoption a seed performs, and
// the pre-change baseline it is reconciled against comes from THIS host — so
// recording the value this machine was already using looks like no change at
// all, while the host that completed the head may have been running another set
// entirely.
func TestFirstFleetReviewerValueReopensCompletedRounds(t *testing.T) {
	ctx := context.Background()
	required := "coderabbitai[bot]," + dialect.CodexBotLogin
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": required,
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

	// The same value this host already had: only its absence from the fleet
	// makes it a change, and that is the change that has to be reconciled.
	if err := NewService(cfg, gh, store, nil).SetFleetConfig(ctx, "required-bots", required); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if round := st.Round(repo, pr); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %+v, want the completed round reopened by the adoption", round)
	}
}

// Adoption erases the reviewer baseline because the fleet may never have agreed
// on WHO reviews. A co-reviewer's self-heal grace is not that question: it is
// how long an already-configured bot is given, every host was driving the same
// set before and after, and reconciliation compares membership. Erasing the
// baseline for it reopened every completed round in the fleet and forced a
// self-heal trigger post on every open PR, for a timing value.
func TestFirstFleetCoBotTimingValueLeavesCompletedRoundsAlone(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":               "codex,bugbot",
		"CRQ_REQUIRED_BOTS":        "coderabbitai[bot]",
		"CRQ_COBOT_BUGBOT_CMD":     "bugbot run",
		"CRQ_COBOT_BUGBOT_TRIGGER": "selfheal",
	})
	repo, pr := "owner/repo", 7
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		// Who reviews is already the fleet's answer; only the timing is not.
		st.SetFleetValue("cobots", "codex,bugbot")
		st.SetFleetValue("required-bots", "coderabbitai[bot]")
		st.PutRound(Round{Repo: repo, PR: pr, Head: "abcdef123", Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := NewService(cfg, gh, store, nil).SetFleetConfig(ctx, "cobot-bugbot-grace", "10m"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	round := st.Round(repo, pr)
	if round == nil || round.Phase != PhaseCompleted {
		t.Fatalf("round = %+v, want the completed round left alone by a timing-only adoption", round)
	}
	if len(round.ForceCoReviewers) != 0 {
		t.Errorf("ForceCoReviewers = %v, want no trigger forced by a timing-only adoption", round.ForceCoReviewers)
	}
}

// Adopting an exclusion this host already applied is a change for every host
// that did not, so it still has to pass the claimed-trigger refusal.
func TestFirstFleetExcludeStillRefusesAClaimedTriggerPost(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.ExcludeRepos = map[string]bool{"owner/repo": true}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "owner/repo", PR: 7, Head: "abcdef123", Phase: PhaseReserved, Token: "tok"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := NewService(cfg, newFakeGitHub(), store, nil).SetFleetConfig(ctx, "exclude", "owner/repo")
	if err == nil || !strings.Contains(err.Error(), "already being posted") {
		t.Fatalf("adopting an exclusion during a claimed trigger post = %v, want a refusal", err)
	}
}

// The recorded quota belongs to whichever account the fleet was scanning, which
// this host's own scope says nothing about — so adopting the scope is judged
// against the quota's own recorded one.
func TestFirstFleetScopeDropsQuotaFromAnotherAccount(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"acme"}
	store := NewMemoryStore(cfg)
	blocked := time.Date(2026, 7, 27, 13, 0, 0, 0, time.UTC)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blocked
		st.Account.Scope = "other-org"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := NewService(cfg, newFakeGitHub(), store, nil).SetFleetConfig(ctx, "scope", "acme"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if st.Account.BlockedUntil != nil || st.Account.Scope != "acme" {
		t.Fatalf("account = %+v, want another account's block dropped", st.Account)
	}
}

// Dropping a recorded key this binary cannot read is the remedy doctor names,
// but it is not this binary's to make while the crq that wrote it is driving the
// queue: the removal may need cleanup only that version knows how to do.
func TestUnsettingAnUnknownKeyWaitsForTheNewerDriver(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("some-future-knob", "7")
		st.Leader = &LeaderLease{Owner: "new-daemon", ExpiresAt: now.Add(time.Minute)}
		st.NoteWriter("new-daemon", WriterCaps+1, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	if _, err := svc.UnsetFleetConfig(ctx, "some-future-knob"); err == nil || !strings.Contains(err.Error(), "newer queue drivers") {
		t.Fatalf("unset of an uninterpretable key = %v, want a refusal naming the newer driver", err)
	}
	// A setting this binary does understand is still its own to drop: it can
	// reconcile that removal itself.
	if err := svc.SetFleetConfig(ctx, "min-interval", "5m"); err != nil {
		t.Fatal(err)
	}
	if dropped, err := svc.UnsetFleetConfig(ctx, "min-interval"); err != nil || !dropped {
		t.Fatalf("unset of a known key = %v %v, want it dropped", dropped, err)
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

func TestFleetDivergenceIncludesAnExplicitlyEmptyHostValue(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.ExcludeRepos = map[string]bool{}
	cfg.ExplicitFleetEnv = map[string]bool{"CRQ_EXCLUDE": true}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("exclude", "owner/repo")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := NewService(cfg, newFakeGitHub(), store, nil).FleetDivergence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], `CRQ_EXCLUDE is set to ""`) {
		t.Fatalf("divergence = %v, want the explicitly empty host value", got)
	}
}

// A host copy that agrees with the fleet is the same variable waiting to be
// fallen back on, and `crq config unset` is what makes it one: the setting goes
// back to whatever each host says, and by then there is no recorded value left
// for this report to compare against. A setting the fleet does not record is not
// that — it is simply this host's to answer, and saying so would name every
// variable on a machine that has yet to adopt anything.
func TestFleetDivergenceNamesAHostCopyThatAgrees(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.MinInterval = 5 * time.Minute
	cfg.ExplicitFleetEnv = map[string]bool{"CRQ_MIN_INTERVAL": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	got, err := svc.FleetDivergence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("divergence = %v, want nothing reported for a setting the fleet leaves to this host", got)
	}

	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("min-interval", "5m")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.FleetDivergence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "CRQ_MIN_INTERVAL") {
		t.Fatalf("divergence = %v, want the matching host copy named", got)
	}
}

// A policy a host feeds from a legacy alias or a per-bot key diverges exactly as
// the canonical variable does — and the remedy names the variable that is
// actually there, since that is the one still waiting to be fallen back on.
func TestFleetDivergenceNamesTheVariableTheHostSet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
		env   string
		host  func(*Config)
	}{
		{
			name:  "legacy alias",
			key:   "rate-limit-co-degrade",
			value: "1",
			env:   "CRQ_RL_CODEX_DEGRADE",
			host:  func(cfg *Config) { cfg.RateLimitCoDegrade = false },
		},
		{
			name:  "per-bot required key",
			key:   "required-bots",
			value: "coderabbitai[bot]",
			env:   "CRQ_COBOT_BUGBOT_REQUIRED",
			host: func(cfg *Config) {
				cfg.RequiredBots = []string{"coderabbitai[bot]", dialect.BugbotLogin}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg := firingConfig()
			tc.host(&cfg)
			cfg.ExplicitFleetEnv = map[string]bool{tc.env: true}
			store := NewMemoryStore(cfg)
			if _, err := store.Update(ctx, func(st *State) error {
				st.SetFleetValue(tc.key, tc.value)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := NewService(cfg, newFakeGitHub(), store, nil).FleetDivergence(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.env) {
				t.Fatalf("divergence = %v, want it to name %s", got, tc.env)
			}
		})
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

type dashboardConfigStore struct {
	StateStore
	render StoreConfig
	syncs  int
}

func (s *dashboardConfigStore) SyncDashboard(_ context.Context, _ State, cfg StoreConfig) error {
	s.render = cfg
	s.syncs++
	return nil
}

func TestFleetMutationsSyncTheDashboard(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DashboardIssue = 1
	store := &dashboardConfigStore{StateStore: NewMemoryStore(cfg)}
	svc := NewService(cfg, newFakeGitHub(), store, &recordingLogger{})

	if err := svc.SetFleetConfig(ctx, "min-interval", "5m"); err != nil {
		t.Fatal(err)
	}
	if store.syncs != 1 {
		t.Fatalf("dashboard syncs after set = %d, want 1", store.syncs)
	}
	if dropped, err := svc.UnsetFleetConfig(ctx, "min-interval"); err != nil || !dropped {
		t.Fatalf("unset = %v, %v", dropped, err)
	}
	if store.syncs != 2 {
		t.Fatalf("dashboard syncs after unset = %d, want 2", store.syncs)
	}
	if seeded, err := svc.SeedFleetConfig(ctx); err != nil || len(seeded) == 0 {
		t.Fatalf("seed = %v, %v", seeded, err)
	}
	if store.syncs != 3 {
		t.Fatalf("dashboard syncs after seed = %d, want 3", store.syncs)
	}
}

func TestDashboardSyncReceivesTheEffectiveFleetReviewers(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	cfg.DashboardIssue = 1
	inner := NewMemoryStore(cfg)
	store := &dashboardConfigStore{StateStore: inner}
	if _, err := inner.Update(ctx, func(st *State) error {
		st.SetFleetValue("required-bots", "coderabbitai[bot],"+dialect.CodexBotLogin)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := inner.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, &recordingLogger{})
	svc.sync(ctx, st)
	if !strings.Contains(store.render.CoReviewers, "codex (required") {
		t.Fatalf("dashboard co-reviewers = %q, want fleet-required codex", store.render.CoReviewers)
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
		st.NoteWriter("old-daemon", CapsFleetPolicy-1, now)
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

func TestFleetReviewerChangeDoesNotFailOnAnInaccessibleHistoricalRepository(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: "owner/live", Number: 7}}
	gh.listPullErrs["owner/gone"] = &ghapi.APIError{Status: http.StatusForbidden, Body: "resource not accessible"}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "owner/gone", PR: 6, Head: "aaaaaaa11", Phase: PhaseCompleted})
		st.PutRound(Round{Repo: "owner/live", PR: 7, Head: "bbbbbbb22", Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)
	if err := svc.SetFleetConfig(ctx, "required-bots", "coderabbitai[bot],"+dialect.CodexBotLogin); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("owner/live", 7); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("live round = %+v, want it reopened", round)
	}
	if round := st.Round("owner/gone", 6); round == nil || !round.ReviewersChanged || round.Phase != PhaseCompleted {
		t.Fatalf("inaccessible historical round = %+v, want a marked completed round", round)
	}
}

// A co-reviewer's command, trigger mode or self-heal grace moves no membership,
// so reconciliation compares the same reviewer sets before and after and never
// reads the open-PR map. Building one anyway spent a REST lookup on every
// repository ever recorded in Rounds — and made a purely local timing update
// fail whenever one of those historical lookups was throttled.
func TestFleetCoBotTimingChangeScansNoPullRequests(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex,bugbot",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	gh := newFakeGitHub()
	gh.listPullErrs["owner/repo"] = &ghapi.RateLimitError{Kind: "secondary"}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "owner/repo", PR: 7, Head: "bbbbbbb22", Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)
	for _, key := range []string{"cobot-bugbot-grace", "cobot-bugbot-cmd", "cobot-bugbot-trigger"} {
		value := map[string]string{
			"cobot-bugbot-grace":   "12m",
			"cobot-bugbot-cmd":     "bugbot run",
			"cobot-bugbot-trigger": "selfheal",
		}[key]
		if err := svc.SetFleetConfig(ctx, key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
		if _, err := svc.UnsetFleetConfig(ctx, key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

// An idempotent membership command scans nothing either. Re-recording the value
// the fleet already holds, or unsetting a key it never held, reconciles nothing
// — and a fleet whose round history names many repositories would otherwise
// spend an open-PR lookup on each of them and fail outright when one was
// throttled, for a command with nothing to change.
func TestIdempotentFleetReviewerCommandScansNoPullRequests(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "owner/repo", PR: 7, Head: "bbbbbbb22", Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)
	if err := svc.SetFleetConfig(ctx, "required-bots", "coderabbitai[bot]"); err != nil {
		t.Fatalf("recording required-bots: %v", err)
	}
	// Only now does the lookup fail, so reaching it at all is the failure.
	gh.listPullErrs["owner/repo"] = &ghapi.RateLimitError{Kind: "secondary"}
	if err := svc.SetFleetConfig(ctx, "required-bots", "coderabbitai[bot]"); err != nil {
		t.Fatalf("repeating the recorded value: %v", err)
	}
	if dropped, err := svc.UnsetFleetConfig(ctx, "cobots"); err != nil || dropped {
		t.Fatalf("unsetting a key the fleet never recorded = (%v, %v), want (false, nil)", dropped, err)
	}
}

func TestFleetReviewerChangePropagatesGitHubThrottling(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	gh := newFakeGitHub()
	gh.listPullErrs["owner/repo"] = &ghapi.RateLimitError{Kind: "secondary"}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.PutRound(Round{Repo: "owner/repo", PR: 7, Head: "bbbbbbb22", Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)
	err := svc.SetFleetConfig(ctx, "required-bots", "coderabbitai[bot],"+dialect.CodexBotLogin)
	if !ghapi.IsThrottled(err) {
		t.Fatalf("reviewer change error = %v, want GitHub throttling propagated", err)
	}
}

func TestInaccessibleRepoLookupClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "not found", err: ghapi.ErrNotFound, want: true},
		{name: "forbidden", err: &ghapi.APIError{Status: http.StatusForbidden}, want: true},
		{name: "unprocessable", err: &ghapi.APIError{Status: http.StatusUnprocessableEntity}, want: false},
		{name: "primary throttle", err: &ghapi.RateLimitError{Kind: "primary"}, want: false},
		{name: "secondary throttle", err: &ghapi.RateLimitError{Kind: "secondary"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inaccessibleRepoLookup(tt.err); got != tt.want {
				t.Fatalf("inaccessibleRepoLookup(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// A key only a newer crq knows is the one divergence this host cannot act on:
// applyFleet ignores it, and reporting only the settings this binary understands
// would have `crq config` and `crq doctor` both call the host fully in step
// while it silently drops part of the shared policy.
func TestFleetConfigReportsSettingsOnlyANewerCrqKnows(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.ExplicitFleetEnv = map[string]bool{}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("some-future-knob", "7")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	items, err := svc.FleetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reported *FleetSetting
	for i, item := range items {
		if item.Key == "some-future-knob" {
			reported = &items[i]
		}
	}
	if reported == nil || !reported.Unknown || reported.Error == "" {
		t.Fatalf("crq config must report the recorded key it ignores, got %+v", items)
	}
	diverged, err := svc.FleetDivergence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diverged) != 1 || !strings.Contains(diverged[0], "some-future-knob") {
		t.Fatalf("doctor divergence = %v, want the ignored setting named", diverged)
	}

	// And the remedy doctor names has to work: refusing to unset a key this
	// binary does not know would leave it unremovable from every older host.
	if dropped, err := svc.UnsetFleetConfig(ctx, "some-future-knob"); err != nil || !dropped {
		t.Fatalf("unset = %v %v, want the recorded key dropped", dropped, err)
	}
	if _, err := svc.UnsetFleetConfig(ctx, "never-recorded"); err == nil {
		t.Fatal("a key that is neither known nor recorded is still a typo")
	}
}
