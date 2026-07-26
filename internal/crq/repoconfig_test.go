package crq

import (
	"context"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
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
