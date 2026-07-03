package crq

import (
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaultsToCodeRabbitRequiredBot(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RequiredBots) != 1 || cfg.RequiredBots[0] != "coderabbitai[bot]" {
		t.Fatalf("default required bots should only require CodeRabbit, got %#v", cfg.RequiredBots)
	}
}

func TestLoadConfigFeedbackBotsIncludeCodexByDefault(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "")
	t.Setenv("CRQ_FEEDBACK_BOTS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// RequiredBots (convergence gate) stays CodeRabbit-only, but FeedbackBots
	// (finding extraction) must also include Codex so its reviews aren't dropped.
	has := func(list []string, want string) bool {
		for _, b := range list {
			if b == want {
				return true
			}
		}
		return false
	}
	if has(cfg.RequiredBots, "chatgpt-codex-connector[bot]") {
		t.Fatalf("Codex must not be a required (convergence-gating) bot, got %#v", cfg.RequiredBots)
	}
	if !has(cfg.FeedbackBots, "coderabbitai[bot]") || !has(cfg.FeedbackBots, "chatgpt-codex-connector[bot]") {
		t.Fatalf("feedback bots should include CodeRabbit and Codex by default, got %#v", cfg.FeedbackBots)
	}
}

func TestLoadConfigFeedbackBotsOverride(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_FEEDBACK_BOTS", "only-this[bot]")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.FeedbackBots) != 1 || cfg.FeedbackBots[0] != "only-this[bot]" {
		t.Fatalf("CRQ_FEEDBACK_BOTS should override the default, got %#v", cfg.FeedbackBots)
	}
}

func TestUnionBotsDedupesAndPreservesOrder(t *testing.T) {
	got := unionBots([]string{"coderabbitai[bot]", ""}, []string{"coderabbitai", "chatgpt-codex-connector[bot]"})
	want := []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	if len(got) != len(want) {
		t.Fatalf("unionBots length mismatch: got %#v want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unionBots[%d] = %q, want %q (full %#v)", i, got[i], want[i], got)
		}
	}
}

func TestLoadConfigDefaultRequiredBotFollowsCustomBot(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_BOT", "custom-review-bot")
	t.Setenv("CRQ_REQUIRED_BOTS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bot != "custom-review-bot" {
		t.Fatalf("custom bot mismatch: %q", cfg.Bot)
	}
	if len(cfg.RequiredBots) != 1 || cfg.RequiredBots[0] != "custom-review-bot" {
		t.Fatalf("default required bots should follow custom CRQ_BOT, got %#v", cfg.RequiredBots)
	}
}
