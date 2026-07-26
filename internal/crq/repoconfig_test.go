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
