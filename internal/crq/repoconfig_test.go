package crq

import (
	"context"
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
	primaryPresent(fleet.ForRepo(RepoReviewers{CoBots: []string{"codex"}, SetCoBots: true}), "override naming only codex")
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
	if err := svc.enqueueBatch(ctx, []queueCandidate{{Repo: repo, PR: auto, Head: gotHead}}); err != nil {
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
