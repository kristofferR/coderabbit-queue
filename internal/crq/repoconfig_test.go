package crq

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// The override has to reach a real decision, not just the config value: a
// per-repo setting that every caller reads and no decision honours is worse
// than none, because it reads as configured.
func TestRepoOverrideReachesTheDecision(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.FeedbackBots = cfg.RequiredBots
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Fleet default: Codex gates.
	if got := svc.cfgFor(st, "o/plain"); !containsBot(got.RequiredBots, dialect.CodexBotLogin) {
		t.Fatalf("fleet RequiredBots = %v, want codex gating", got.RequiredBots)
	}

	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetRepoOverride("o/quiet", RepoReviewers{
			CoBots: []string{}, SetCoBots: true,
			Required: []string{"coderabbitai[bot]"}, SetRequired: true,
			UpdatedAt: &now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	quiet := svc.cfgFor(st, "o/quiet")
	if containsBot(quiet.RequiredBots, dialect.CodexBotLogin) {
		t.Errorf("RequiredBots = %v, want codex not gating this repo", quiet.RequiredBots)
	}
	// The policy the engine actually decides from must carry it.
	for _, cp := range quiet.policy().CoReviewerPolicies() {
		if dialect.IsCodexBot(cp.Login) {
			t.Errorf("policy still drives codex on o/quiet: %+v", cp)
		}
	}
	if !containsBot(quiet.policy().RequiredBots, "coderabbitai[bot]") {
		t.Errorf("policy RequiredBots = %v, want the primary still gating", quiet.policy().RequiredBots)
	}
	// And the other repository is untouched by its neighbour's choice.
	if !containsBot(svc.cfgFor(st, "o/plain").RequiredBots, dialect.CodexBotLogin) {
		t.Error("one repository's override must not change another's")
	}
}

func TestClearReviewersComparesAgainstFleetPolicy(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	repo, pr := "owner/repo", 19
	now := time.Now().UTC()
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("required-bots", "coderabbitai[bot],"+dialect.CodexBotLogin)
		st.SetRepoOverride(repo, RepoReviewers{
			Required:    []string{"coderabbitai[bot]"},
			SetRequired: true,
			UpdatedAt:   &now,
		})
		st.PutRound(Round{Repo: repo, PR: pr, Head: "abcdef123", Phase: PhaseCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(cfg, gh, store, nil).ClearReviewers(ctx, repo); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if round := st.Round(repo, pr); round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %+v, want clearing to the fleet reviewer set to reopen it", round)
	}
}

// The override must be able to ADD a reviewer, not only subtract one. Resolving
// choices against the fleet's enabled list made "which bots for which project"
// a one-way filter: with CRQ_COBOTS=codex, asking for bugbot was rejected as
// unknown even though it is a registered reviewer.
func TestOverrideCanEnableABotTheFleetDoesNot(t *testing.T) {
	fleet := isolatedConfig(t, map[string]string{"CRQ_COBOTS": "codex"})
	if len(fleet.CoBots) != 1 {
		t.Fatalf("fleet CoBots = %+v, want only codex", fleet.CoBots)
	}

	got := fleet.ForRepo(RepoReviewers{
		CoBots: []string{"bugbot"}, SetCoBots: true,
		Required: []string{"bugbot"}, SetRequired: true,
	})
	found := false
	for _, cb := range got.CoBots {
		if cb.Name == "bugbot" {
			found = true
			if cb.Command == "" {
				t.Error("an enabled bot with no trigger command can never be asked to review")
			}
		}
	}
	if !found {
		t.Fatalf("CoBots = %+v, want bugbot enabled from the registry", got.CoBots)
	}
	// Required implies enabled, as the fleet parse already does — a bot that
	// gates but is never triggered waits forever.
	only := fleet.ForRepo(RepoReviewers{
		CoBots: []string{}, SetCoBots: true,
		Required: []string{"macroscope"}, SetRequired: true,
	})
	if len(only.CoBots) != 1 || only.CoBots[0].Name != "macroscope" {
		t.Errorf("CoBots = %+v, want the required bot enabled too", only.CoBots)
	}
}

func TestOverrideRecomputesImplicitTriggerAfterRemovingRequiredness(t *testing.T) {
	fleet := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot],codex",
	})
	got := fleet.ForRepo(RepoReviewers{
		CoBots:      []string{"codex"},
		SetCoBots:   true,
		Required:    []string{"coderabbitai[bot]"},
		SetRequired: true,
	})
	if len(got.CoBots) != 1 {
		t.Fatalf("CoBots = %+v, want Codex still enabled", got.CoBots)
	}
	if got.CoBots[0].Required || got.CoBots[0].Trigger != engine.TriggerNever {
		t.Fatalf("Codex = %+v, want optional with its implicit never trigger", got.CoBots[0])
	}

	explicit := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":              "codex",
		"CRQ_REQUIRED_BOTS":       "coderabbitai[bot],codex",
		"CRQ_COBOT_CODEX_TRIGGER": "always",
	})
	got = explicit.ForRepo(RepoReviewers{
		CoBots:      []string{"codex"},
		SetCoBots:   true,
		Required:    []string{"coderabbitai[bot]"},
		SetRequired: true,
	})
	if got.CoBots[0].Trigger != engine.TriggerAlways {
		t.Fatalf("explicit Codex trigger = %q, want always preserved", got.CoBots[0].Trigger)
	}
}

// A primary that is itself a registry bot keeps its silenced entry through an
// override: that entry is where its wording and check-run hooks come from, so
// dropping it costs the PRIMARY its evidence and the round waits for a bot
// whose clean result crq can no longer read.
func TestOverrideKeepsTheSilencedPrimaryEntry(t *testing.T) {
	fleet := isolatedConfig(t, map[string]string{
		// Bugbot's login is cursor[bot]; naming that as the primary is what makes
		// the primary a registry bot.
		"CRQ_BOT":        "cursor[bot]",
		"CRQ_COBOTS":     "codex,bugbot",
		"CRQ_REVIEW_CMD": "bugbot run",
	})
	primaryPresent := func(cfg Config, when string) {
		t.Helper()
		for _, cb := range cfg.CoBots {
			if sameBot(cb.Login, cfg.Bot) {
				return
			}
		}
		t.Errorf("%s: CoBots = %+v lost the primary's registry entry", when, cfg.CoBots)
	}
	primaryPresent(fleet, "fleet default")
	got := fleet.ForRepo(RepoReviewers{
		CoBots: []string{"codex"}, SetCoBots: true,
		Required: []string{dialect.CodexBotLogin}, SetRequired: true,
	})
	primaryPresent(got, "override naming only codex")
	for _, cp := range got.policy().CoReviewerPolicies() {
		if sameBot(cp.Login, got.Bot) {
			t.Errorf("primary %q leaked into the dynamic co-review policies", got.Bot)
		}
	}
}

// Four defects that all end the same way: a round waiting on a reviewer crq
// never asks, or never waiting at all.
func TestOverrideNeverGatesOnAReviewerItCannotTrigger(t *testing.T) {
	t.Run("required alone enables the bot", func(t *testing.T) {
		// `reviewers set repo --required bugbot` with no --bots: SetRequired is
		// true and SetCoBots is false, so nothing enabled Bugbot and it became an
		// unknown required reviewer with no command.
		fleet := isolatedConfig(t, map[string]string{"CRQ_COBOTS": "codex"})
		got := fleet.ForRepo(RepoReviewers{Required: []string{"bugbot"}, SetRequired: true})
		for _, r := range got.Reviewers {
			if dialect.NormalizeBotName(r.Login) != "cursor" {
				continue
			}
			if !r.Required {
				t.Error("bugbot must gate here")
			}
			if r.Command == "" || r.Trigger == engine.TriggerNever {
				t.Errorf("bugbot = %+v: required but never triggered, so the round waits forever", r)
			}
			return
		}
		t.Errorf("reviewers = %+v, want bugbot enabled by requiring it", got.Reviewers)
	})

	t.Run("a promoted co-reviewer gets a trigger", func(t *testing.T) {
		// Codex's fleet entry is trigger=never while it is optional. Retaining
		// that entry and only flipping Required leaves the engine waiting for
		// evidence no command was ever posted for.
		fleet := isolatedConfig(t, map[string]string{"CRQ_COBOTS": "codex,bugbot"})
		for _, cb := range fleet.CoBots {
			if cb.Name == "codex" && cb.Trigger != engine.TriggerNever {
				t.Skip("codex is already triggered by default here; nothing to promote")
			}
		}
		got := fleet.ForRepo(RepoReviewers{
			CoBots: []string{"codex"}, SetCoBots: true,
			Required: []string{"codex"}, SetRequired: true,
		})
		for _, cb := range got.CoBots {
			if cb.Name == "codex" && cb.Trigger == engine.TriggerNever {
				t.Errorf("codex = %+v: required with no trigger", cb)
			}
		}
	})

	t.Run("a newly required self-heal bot gets an immediate trigger", func(t *testing.T) {
		// Bugbot normally self-heals only after activity proves it missed a head.
		// On a completed head reopened by this override it has no activity yet, so
		// preserving selfheal would complete the round without ever asking it.
		fleet := isolatedConfig(t, map[string]string{"CRQ_COBOTS": "bugbot"})
		got := fleet.ForRepo(RepoReviewers{
			CoBots: []string{"bugbot"}, SetCoBots: true,
			Required: []string{"bugbot"}, SetRequired: true,
		})
		for _, cb := range got.CoBots {
			if cb.Name != "bugbot" {
				continue
			}
			if cb.Trigger != engine.TriggerSelfHeal {
				t.Errorf("bugbot = %+v: the repository's steady-state policy must stay selfheal", cb)
			}
			forced := forcedCoReviewers(nil, fleet, got)
			if len(forced) != 1 || !sameBot(forced[0], cb.Login) {
				t.Fatalf("forced reviewers = %v, want newly required bugbot", forced)
			}
			round := Round{
				Repo: "o/r", PR: 3, Head: "aaaaaaaa1", Phase: PhaseQueued,
				ForceCoReviewers: forced,
			}
			obs := engine.Observation{
				Head: "aaaaaaaa1", Open: true,
				Reviews: []engine.ReviewSeen{{
					Bot: got.Bot, Commit: "aaaaaaaa1",
				}},
			}
			decision := engine.DecideFire(engine.Global{SlotFree: true}, round, obs, time.Now().UTC(), got.policy())
			if decision.Verdict != engine.FireCoOnly || len(decision.PostCo) != 1 ||
				!sameBot(decision.PostCo[0], cb.Login) {
				t.Errorf("decision = %+v, want one immediate bugbot trigger", decision)
			}
			return
		}
		t.Fatalf("CoBots = %+v, want bugbot enabled", got.CoBots)
	})

	t.Run("a newly enabled optional self-heal bot gets an immediate trigger", func(t *testing.T) {
		fleet := isolatedConfig(t, map[string]string{
			"CRQ_COBOTS":                    "",
			"CRQ_COBOT_CODEX_REQUIRED":      "false",
			"CRQ_COBOT_BUGBOT_REQUIRED":     "false",
			"CRQ_COBOT_MACROSCOPE_REQUIRED": "false",
		})
		got := fleet.ForRepo(RepoReviewers{CoBots: []string{"bugbot"}, SetCoBots: true})
		for _, cb := range got.CoBots {
			if cb.Name != "bugbot" {
				continue
			}
			if cb.Required {
				t.Fatalf("bugbot = %+v, want an optional reviewer", cb)
			}
			forced := forcedCoReviewers(nil, fleet, got)
			if len(forced) != 1 || !sameBot(forced[0], cb.Login) {
				t.Fatalf("forced reviewers = %v, want newly enabled bugbot", forced)
			}
			round := Round{
				Repo: "o/r", PR: 4, Head: "aaaaaaaa1", Phase: PhaseQueued,
				ForceCoReviewers: forced,
			}
			obs := engine.Observation{
				Head: "aaaaaaaa1", Open: true,
				Reviews: []engine.ReviewSeen{{
					Bot: got.Bot, Commit: "aaaaaaaa1",
				}},
			}
			decision := engine.DecideFire(engine.Global{SlotFree: true}, round, obs, time.Now().UTC(), got.policy())
			if decision.Verdict != engine.FireCoOnly || len(decision.PostCo) != 1 ||
				!sameBot(decision.PostCo[0], cb.Login) {
				t.Errorf("decision = %+v, want one immediate optional bugbot trigger", decision)
			}
			return
		}
		t.Fatalf("CoBots = %+v, want bugbot enabled", got.CoBots)
	})

	t.Run("an explicit feedback set survives", func(t *testing.T) {
		// CRQ_FEEDBACK_BOTS is the one list an operator may widen beyond who
		// reviews. Deriving over it would silently stop surfacing those findings
		// the moment the repository got any override at all.
		fleet := isolatedConfig(t, map[string]string{"CRQ_FEEDBACK_BOTS": "sonar[bot]"})
		got := fleet.ForRepo(RepoReviewers{CoBots: []string{"codex"}, SetCoBots: true})
		if len(got.FeedbackBots) != 1 || got.FeedbackBots[0] != "sonar[bot]" {
			t.Errorf("FeedbackBots = %v, want the explicit setting kept", got.FeedbackBots)
		}
	})
}

// Changing who has to review a head must not strand the PR. A completed round
// is the "this head was reviewed" dedup marker, so adding a required reviewer
// leaves convergence reporting it pending while enqueue keeps skipping the head:
// no eligible round exists to trigger it, and next waits for a push that has no
// reason to come.
func TestChangingRequirementsReopensACompletedRound(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	repo, pr, head := "o/r", 3, "aaaaaaaa1"
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, time.Now().UTC(), 11)

	if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %#v, want it requeued so the new reviewer can be asked", round)
	}
	if round.Head != head {
		t.Errorf("head = %q, want the same head %q — what changed is who must answer", round.Head, head)
	}

	// An override that does not change the required set leaves rounds alone: a
	// finished round must not be reopened for nothing.
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round(repo, pr)
		if err := r.Reserve("t", "h", time.Now().UTC()); err != nil {
			return err
		}
		if err := r.Fire(12, time.Now().UTC()); err != nil {
			return err
		}
		if err := r.Complete(); err != nil {
			return err
		}
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	st, _, _ = store.Load(ctx)
	if round := st.Round(repo, pr); round == nil || round.Phase != PhaseCompleted {
		t.Errorf("round = %#v, want it left completed when nothing changed", round)
	}
}

func TestSettingIdenticalReviewersPreservesOverrideIdentity(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	first := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return first }

	if _, err := svc.SetReviewers(ctx, "o/r", []string{"codex"}, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	before, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	original, ok := before.RepoOverride("o/r")
	if !ok || original.UpdatedAt == nil {
		t.Fatalf("override = %#v, want a timestamped override", original)
	}

	svc.now = func() time.Time { return first.Add(time.Hour) }
	if _, err := svc.SetReviewers(ctx, "o/r", []string{"codex"}, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	after, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, ok := after.RepoOverride("o/r")
	if !ok || unchanged.UpdatedAt == nil || !unchanged.UpdatedAt.Equal(*original.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v, want the original identity %v", unchanged.UpdatedAt, original.UpdatedAt)
	}
	if after.Rev != before.Rev {
		t.Fatalf("state revision = %d, want unchanged %d", after.Rev, before.Rev)
	}
}

// Optional co-reviewers can become convergence gates after participation
// evidence appears. Enabling one therefore needs the same active round as
// changing the statically required set, so its trigger and bounded wait run.
func TestChangingEnabledCoReviewersReopensACompletedRound(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{"CRQ_COBOTS": ""})
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	repo, pr, head := "o/r", 4, "aaaaaaaa1"
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, time.Now().UTC(), 11)

	if _, err := svc.SetReviewers(ctx, repo, []string{"bugbot"}, nil); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, pr)
	if round == nil || round.Phase != PhaseQueued {
		t.Fatalf("round = %#v, want it requeued so the newly enabled co-reviewer can run", round)
	}
}

func TestChangingReviewersForcesExistingActiveRounds(t *testing.T) {
	for _, phase := range []Phase{PhaseQueued, PhaseFired, PhaseReviewing, PhaseAwaitingRetry} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			cfg := isolatedConfig(t, map[string]string{"CRQ_COBOTS": ""})
			store := NewMemoryStore(cfg)
			gh := newFakeGitHub()
			repo, pr, head := "o/r", 4, "aaaaaaaa1"
			gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
			svc := NewService(cfg, gh, store, nil)
			seedPhase := phase
			if phase == PhaseAwaitingRetry {
				seedPhase = PhaseReviewing
			}
			seedRound(t, store, cfg, repo, pr, head, seedPhase, time.Now().UTC(), 11)
			if phase == PhaseAwaitingRetry {
				if _, err := store.Update(ctx, func(st *State) error {
					r := st.Round(repo, pr)
					retryAt := time.Now().UTC().Add(time.Hour)
					if err := r.AwaitRetry(retryAt, "test", time.Now().UTC()); err != nil {
						return err
					}
					st.PutRound(*r)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := svc.SetReviewers(ctx, repo, []string{"bugbot"}, []string{"bugbot"}); err != nil {
				t.Fatal(err)
			}
			st, _, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			round := st.Round(repo, pr)
			if round == nil || !round.ForceCoReviewer(dialect.BugbotLogin) {
				t.Fatalf("round = %#v, want newly required bugbot forced on the active round", round)
			}
			if round.Phase != phase {
				t.Fatalf("phase = %s, want active round to stay %s", round.Phase, phase)
			}
		})
	}
}

// Dedupe writes the "every required reviewer answered this head" marker, so a
// required reviewer added between the decision and the write must void it. The
// round is still queued when the operator's write lands — nothing to requeue —
// and dedupe posts nothing, so only this revalidation catches the race.
func TestDedupeIsRevalidatedAgainstAReviewerChange(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	repo, settled, raced, head := "o/r", 9, 10, "aaaaaaaa1"
	seedRound(t, store, cfg, repo, settled, head, PhaseQueued, now, 0)
	seedRound(t, store, cfg, repo, raced, head, PhaseQueued, now, 0)
	dedupe := engine.FireDecision{Verdict: engine.FireDedupe, Reason: "bot already reviewed head"}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := *st.Round(repo, settled)
	if _, err := svc.applyFire(ctx, svc.cfgFor(st, repo), round, engine.Observation{}, dedupe, now); err != nil {
		t.Fatal(err)
	}
	if got := roundPhase(t, store, repo, settled); got != PhaseCompleted {
		t.Fatalf("phase = %s, want an unraced dedupe to still complete the round", got)
	}

	// Now the override lands after the decision was made from svc.cfg.
	if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round = *st.Round(repo, raced)
	result, err := svc.applyFire(ctx, svc.cfg, round, engine.Observation{}, dedupe, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "lost_race" {
		t.Errorf("action = %q, want the stale dedupe reported as a lost race", result.Action)
	}
	if got := roundPhase(t, store, repo, raced); got != PhaseQueued {
		t.Fatalf("phase = %s, want the round left queued so the new reviewer is asked", got)
	}
}

// hookedStore runs a hook once, immediately before the next Update's mutation
// reaches the store — the window a fire effect's own CAS has to close, and the
// one a check made before the write cannot see into.
type hookedStore struct {
	StateStore
	hook func()
}

func (h *hookedStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	if h.hook != nil {
		hook := h.hook
		h.hook = nil
		hook()
	}
	return h.StateStore.Update(ctx, mutate)
}

// The revalidation has to happen in the write itself: an override that lands
// after any pre-flight re-read but before the dedupe commits would otherwise
// still complete the round under the reviewer set it replaced.
func TestDedupeIsRevalidatedInsideItsWrite(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	hooked := &hookedStore{StateStore: store}
	svc := NewService(cfg, newFakeGitHub(), hooked, nil)
	now := time.Now().UTC()
	repo, pr, head := "o/r", 11, "aaaaaaaa1"
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := *st.Round(repo, pr)
	hooked.hook = func() {
		if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
			t.Error(err)
		}
	}
	dedupe := engine.FireDecision{Verdict: engine.FireDedupe, Reason: "bot already reviewed head"}
	result, err := svc.applyFire(ctx, svc.cfgFor(st, repo), round, engine.Observation{}, dedupe, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "lost_race" {
		t.Errorf("action = %q, want the dedupe voided by the override that beat its write", result.Action)
	}
	if got := roundPhase(t, store, repo, pr); got != PhaseQueued {
		t.Fatalf("phase = %s, want the round left queued so the new reviewer is asked", got)
	}
}

// Every reviewers path rejects a target that is not exactly owner/name. The read
// path never contacts GitHub, so a typo would otherwise report the fleet default
// and exit 0 — a plausible answer about a project that does not exist.
func TestReviewersRejectAnIncompleteRepository(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	for _, repo := range []string{"", "owner", "owner/", "/name", "owner/name/extra"} {
		if _, err := svc.Reviewers(ctx, repo); err == nil {
			t.Errorf("Reviewers(%q) = nil error, want the malformed target refused", repo)
		}
		if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, nil); err == nil {
			t.Errorf("SetReviewers(%q) = nil error, want the malformed target refused", repo)
		}
		if _, err := svc.ClearReviewers(ctx, repo); err == nil {
			t.Errorf("ClearReviewers(%q) = nil error, want the malformed target refused", repo)
		}
	}
	if _, err := svc.Reviewers(ctx, "owner/name"); err != nil {
		t.Errorf("Reviewers(owner/name) = %v, want a well-formed target accepted", err)
	}
}

// A closed PR is not a dead PR. Requirements that change while it is shut must
// still reach it if it is reopened at the same head, or its completed round is
// the dedup marker that hides the reviewer the operator added.
func TestAReopenedPRPicksUpRequirementsChangedWhileItWasClosed(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	repo, manual, auto, head := "o/r", 7, 8, "aaaaaaaa1"
	svc := NewService(cfg, gh, store, nil)
	for _, pr := range []int{manual, auto} {
		seedRound(t, store, cfg, repo, pr, head, PhaseCompleted, time.Now().UTC(), 11)
	}

	// Both PRs are closed while the required set changes: no round is requeued.
	if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []int{manual, auto} {
		round := st.Round(repo, pr)
		if round == nil || round.Phase != PhaseCompleted {
			t.Fatalf("round = %#v, want a closed PR left alone", round)
		}
		if !round.ReviewersChanged {
			t.Fatalf("round = %#v, want the change recorded for a reopen", round)
		}
	}

	// Both come back at the same head — the manual path first.
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head + "bcdef1234"
	gh.pulls[fakeKey(repo, manual)] = pull
	gh.pulls[fakeKey(repo, auto)] = pull
	result, err := svc.Enqueue(ctx, repo, manual)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Queued || result.Deduped {
		t.Fatalf("enqueue = %#v, want the reopened round queued rather than deduped", result)
	}

	// And the autoreview path, which never calls Enqueue.
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	need, gotHead, err := svc.needsReview(ctx, st, repo, auto, true)
	if err != nil {
		t.Fatal(err)
	}
	if !need || gotHead != head {
		t.Fatalf("needsReview = %v %q, want the reopened PR enqueued at %q", need, gotHead, head)
	}
	if err := svc.enqueueBatch(ctx, []queueCandidate{{Repo: repo, PR: auto, Head: gotHead}}, svc.fleetCfg(st).FleetRevision); err != nil {
		t.Fatal(err)
	}

	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []int{manual, auto} {
		round := st.Round(repo, pr)
		if round == nil || round.Phase != PhaseQueued || round.Head != head {
			t.Fatalf("round = %#v, want it queued at the same head so the new reviewer is asked", round)
		}
		if round.ReviewersChanged {
			t.Errorf("round = %#v, want the mark cleared — it answers under the current requirements now", round)
		}
	}
}

// Rounds are never deleted, so a repository's merged and closed PRs stay behind
// as completed dedup markers. Requeueing those on a reviewer change would put
// every dead round ahead of real work — Pump observes and drops them one per
// tick — and no closed PR can be stranded by a reviewer it will never get.
func TestChangingRequirementsLeavesClosedPRsAlone(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	repo, open, merged := "o/r", 4, 5
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: open}}
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, open, "aaaaaaaa1", PhaseCompleted, time.Now().UTC(), 11)
	seedRound(t, store, cfg, repo, merged, "bbbbbbbb2", PhaseCompleted, time.Now().UTC(), 12)

	if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(repo, open); round == nil || round.Phase != PhaseQueued {
		t.Errorf("open round = %#v, want it requeued so the new reviewer can be asked", round)
	}
	if round := st.Round(repo, merged); round == nil || round.Phase != PhaseCompleted {
		t.Errorf("merged round = %#v, want it left completed — a closed PR cannot be stranded", round)
	}
}

// The other half of TestOverrideKeepsTheSilencedPrimaryEntry: when the fleet
// configuration never carried an entry for a registry-bot primary (not enabled,
// not fleet-required), a repository that gates on it has to gain one. Skipping
// it as "not a co-reviewer here" leaves observation without that bot's wording
// and check-run hooks, so a check-only clean result is never fetched and the
// primary this repository waits for stays pending until the round times out.
func TestOverrideAddsTheSilencedPrimaryEntryTheFleetLacked(t *testing.T) {
	fleet := isolatedConfig(t, map[string]string{
		// Bugbot's login is cursor[bot]: naming it primary makes the primary a
		// registry bot, and neither CRQ_COBOTS nor CRQ_REQUIRED_BOTS mentions it.
		"CRQ_BOT":             "cursor[bot]",
		"CRQ_REVIEW_CMD":      "bugbot run",
		"CRQ_COBOTS":          "codex",
		"CRQ_REQUIRED_BOTS":   dialect.CodexBotLogin,
		"CRQ_COBOT_CODEX_CMD": "@codex review",
	})
	for _, cb := range fleet.CoBots {
		if sameBot(cb.Login, fleet.Bot) {
			t.Fatalf("fleet CoBots = %+v already carry the primary; this test needs the gap", fleet.CoBots)
		}
	}

	got := fleet.ForRepo(RepoReviewers{
		Required: []string{"cursor[bot]", dialect.CodexBotLogin}, SetRequired: true,
	})
	var primary *CoBotConfig
	for i, cb := range got.CoBots {
		if sameBot(cb.Login, got.Bot) {
			primary = &got.CoBots[i]
		}
	}
	if primary == nil {
		t.Fatalf("CoBots = %+v, want the primary's registry entry so its evidence can be read", got.CoBots)
	}
	// Silenced: it is asked as the primary, and asking the same bot twice is the
	// bug the fleet parse silences it for.
	if primary.Trigger != engine.TriggerNever {
		t.Errorf("primary entry = %+v, want trigger never — it is triggered as the primary", *primary)
	}
	if !got.coChecksRelevant() {
		t.Error("the primary's check runs must be fetched; a check-only clean result is its whole answer")
	}
	// And it is still exactly one reviewer, account-metered, not a second free one.
	if p, ok := got.Primary(); !ok || !sameBot(p.Login, got.Bot) {
		t.Errorf("Primary() = %+v/%v, want the configured primary", p, ok)
	}
	metered := 0
	for _, r := range got.Reviewers {
		if r.Metered() {
			metered++
		}
	}
	if metered != 1 {
		t.Errorf("reviewers = %+v, want exactly one metered entry", got.Reviewers)
	}
}

// An operator who sets CRQ_COBOT_<NAME>_TRIGGER=never has disabled that bot's
// command everywhere: the fleet parse already lets that value win over the
// registry's required trigger. A repository requiring the bot must not be able
// to turn it back on — that would post the command the operator switched off.
func TestOverrideKeepsAnExplicitNeverTrigger(t *testing.T) {
	fleet := isolatedConfig(t, map[string]string{
		"CRQ_COBOTS":              "codex",
		"CRQ_COBOT_CODEX_TRIGGER": "never",
	})
	got := fleet.ForRepo(RepoReviewers{
		CoBots: []string{"codex"}, SetCoBots: true,
		Required: []string{"codex"}, SetRequired: true,
	})
	for _, cb := range got.CoBots {
		if cb.Name != "codex" {
			continue
		}
		if cb.Trigger != engine.TriggerNever {
			t.Errorf("codex = %+v, want the explicit never kept — the fleet parse honours it too", cb)
		}
		return
	}
	t.Fatalf("CoBots = %+v, want codex required here", got.CoBots)
}

// Completing a round writes the "this head was reviewed" dedup marker, and
// reopenForChangedReviewers deliberately leaves an in-flight round alone — it is
// already going to answer. A required reviewer added between Progress deciding
// and that write is therefore caught by neither: the marker dedupes the head
// under the set that no longer gates it, and nothing re-fires it.
func TestCompletionIsRevalidatedInsideItsWrite(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	hooked := &hookedStore{StateStore: store}
	gh := newFakeGitHub()
	repo, pr, head := "o/r", 12, "aaaaaaaa1"
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	svc := NewService(cfg, gh, hooked, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, pr, head, PhaseFired, now, 21)

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := svc.cfgFor(st, repo)
	hooked.hook = func() {
		if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
			t.Error(err)
		}
	}
	done := engine.Transition{Outcome: engine.OutComplete, Reason: "feedback complete"}
	if _, err := hooked.Update(ctx, func(st *State) error {
		return svc.applyTransition(st, st.Round(repo, pr), done, now, stale)
	}); err != nil {
		t.Fatal(err)
	}
	if got := roundPhase(t, store, repo, pr); got != PhaseFired {
		t.Fatalf("phase = %s, want the stale completion dropped so the new reviewer is still asked", got)
	}
}

// A retry is policy-dependent too: its RetryAt and account block are computed
// from the fleet's rate-limit fallback, so a widened window landing in the same
// decision/write gap would be persisted at the old, earlier deadline — and the
// account unblocked in time for another metered review the fleet meant to defer.
func TestRetryIsRevalidatedInsideItsWrite(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	hooked := &hookedStore{StateStore: store}
	svc := NewService(cfg, newFakeGitHub(), hooked, nil)
	now := time.Now().UTC()
	repo, pr, head := "o/r", 14, "aaaaaaaa1"
	seedRound(t, store, cfg, repo, pr, head, PhaseFired, now, 22)

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := svc.cfgFor(st, repo)
	hooked.hook = func() {
		if _, err := store.Update(ctx, func(st *State) error {
			st.SetFleetValue("rate-limit-fallback", "2h")
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	until := now.Add(15 * time.Minute)
	retry := engine.Transition{
		Outcome: engine.OutRetry,
		Reason:  dialect.ReasonRateLimited,
		RetryAt: until,
		Blocked: &engine.AccountBlock{Until: until, CommentID: 99, CommentUpdated: now},
	}
	if _, err := hooked.Update(ctx, func(st *State) error {
		return svc.applyTransition(st, st.Round(repo, pr), retry, now, stale)
	}); err != nil {
		t.Fatal(err)
	}
	if got := roundPhase(t, store, repo, pr); got != PhaseFired {
		t.Fatalf("phase = %s, want the stale retry dropped so it is recomputed under the fleet's window", got)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Account.BlockedUntil != nil {
		t.Fatalf("the block computed from the old window must not be recorded, got %v", st.Account.BlockedUntil)
	}
}

// Feedback and its completion write have the same decision/write window as
// Progress. A reviewer override landing in that window must leave the in-flight
// round open so the next observation can ask the newly required reviewer.
func TestFeedbackCompletionIsRevalidatedInsideItsWrite(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	hooked := &hookedStore{StateStore: store}
	gh := newFakeGitHub()
	repo, pr, head := "o/r", 13, "aaaaaaaa1"
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	svc := NewService(cfg, gh, hooked, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, pr, head, PhaseFired, now, 21)

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := svc.cfgFor(st, repo)
	hooked.hook = func() {
		if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
			t.Error(err)
		}
	}
	svc.completeWaitRound(ctx, repo, pr, head, false, &stale)
	if got := roundPhase(t, store, repo, pr); got != PhaseFired {
		t.Fatalf("phase = %s, want stale feedback completion dropped", got)
	}
}

// The self-heal sweep claims a co-reviewer's trigger post under CAS and then
// posts it. The claim is what authorizes the post, so — as in fireCoOnly — the
// reviewer set that chose the bot has to still be the configured one when the
// claim commits, or `crq reviewers set` reports a bot disabled while crq is
// asking it for a review.
func TestSelfHealTriggerIsRevalidatedInsideItsClaim(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{cfg.Bot, dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	store := NewMemoryStore(cfg)
	hooked := &hookedStore{StateStore: store}
	gh := newFakeGitHub()
	repo, pr, head := "o/r", 13, "aaaaaaaa1"
	gh.searchPRs = []ghapi.SearchPR{{Repo: repo, Number: pr}}
	svc := NewService(cfg, gh, hooked, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, pr, head, PhaseFired, now, 22)

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := *st.Round(repo, pr)
	stale := svc.cfgFor(st, repo)
	// Nothing observed for the head yet, so an always-mode trigger wants posting.
	obs := engine.Observation{Head: head, Open: true}
	hooked.hook = func() {
		// The operator drops the co-reviewer, keeping only the primary.
		if _, err := svc.SetReviewers(ctx, repo, []string{}, []string{cfg.Bot}); err != nil {
			t.Error(err)
		}
	}
	svc.selfHealCoReviewers(ctx, stale, round, obs, now)

	for _, body := range gh.posted {
		if strings.Contains(body, "@codex") {
			t.Errorf("posted %q for a co-reviewer the operator had just removed", body)
		}
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := st.Round(repo, pr).Co(dialect.CodexBotLogin); c.ClaimedAt != nil || c.CommandID != 0 {
		t.Errorf("codex bookkeeping = %+v, want no claim for a removed reviewer", c)
	}
}

func TestSelfHealDoesNotTriggerForFleetExcludedRepository(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{cfg.Bot, dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.ExcludeRepos = map[string]bool{"o/r": true}
	store := NewMemoryStore(cfg)
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, "o/r", 13, "aaaaaaaa1", PhaseFired, now, 22)
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	svc.selfHealCoReviewers(ctx, cfg, *st.Round("o/r", 13), engine.Observation{
		Head: "aaaaaaaa1", Open: true,
	}, now)

	if len(gh.posted) != 0 {
		t.Fatalf("excluded repository received a co-review trigger: %v", gh.posted)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := st.Round("o/r", 13).Co(dialect.CodexBotLogin); c.ClaimedAt != nil || c.CommandID != 0 {
		t.Fatalf("excluded repository retained a co-review claim: %+v", c)
	}
}

// The co-review wait is the one fire verdict with no later reconciliation to
// undo it: the round is still queued when a reviewer change lands, requeuing a
// queued round is a no-op, and the stale decision then parks it in reviewing
// with commands adopted for the reviewer set that no longer gates it.
func TestCoReviewWaitIsRevalidatedInsideItsWrite(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	store := NewMemoryStore(cfg)
	hooked := &hookedStore{StateStore: store}
	svc := NewService(cfg, newFakeGitHub(), hooked, nil)
	now := time.Now().UTC()
	repo, pr, head := "o/r", 12, "aaaaaaaa2"
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := *st.Round(repo, pr)
	hooked.hook = func() {
		if _, err := svc.SetReviewers(ctx, repo, []string{"codex"}, []string{"codex"}); err != nil {
			t.Error(err)
		}
	}
	wait := engine.FireDecision{Verdict: engine.FireCoReviewWait, Reason: "awaiting co-review"}
	obs := engine.Observation{Open: true, Head: head, HeadAt: now.Add(-time.Minute)}
	result, err := svc.applyFire(ctx, svc.cfgFor(st, repo), round, obs, wait, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "lost_race" {
		t.Errorf("action = %q, want the wait voided by the override that beat its write", result.Action)
	}
	if got := roundPhase(t, store, repo, pr); got != PhaseQueued {
		t.Fatalf("phase = %s, want the round left queued so the new reviewer set decides it", got)
	}
}
