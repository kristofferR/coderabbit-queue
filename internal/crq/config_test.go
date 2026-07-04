package crq

import (
	"os"
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

func TestLoadConfigFeedbackBotsExcludesCodeRabbitForCustomReviewer(t *testing.T) {
	// A crq configured for a different reviewer must not surface CodeRabbit
	// findings — crq neither fires nor waits for CodeRabbit in that setup.
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_BOT", "custom-review-bot")
	t.Setenv("CRQ_REQUIRED_BOTS", "")
	t.Setenv("CRQ_FEEDBACK_BOTS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range cfg.FeedbackBots {
		if b == "coderabbitai[bot]" {
			t.Fatalf("custom-reviewer feedback bots must not include CodeRabbit, got %#v", cfg.FeedbackBots)
		}
	}
	has := false
	for _, b := range cfg.FeedbackBots {
		if b == "custom-review-bot" {
			has = true
		}
	}
	if !has {
		t.Fatalf("feedback bots should include the configured reviewer, got %#v", cfg.FeedbackBots)
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

func TestLoadConfigPreservesEmptyCompletionMarker(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_COMPLETION_MARKER", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CompletionMarker != "" {
		t.Fatalf("explicit empty CRQ_COMPLETION_MARKER should disable completion matching, got %q", cfg.CompletionMarker)
	}
}

func TestLoadConfigPreservesEmptyCompletionMarkerFromFile(t *testing.T) {
	old, had := os.LookupEnv("CRQ_COMPLETION_MARKER")
	if err := os.Unsetenv("CRQ_COMPLETION_MARKER"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv("CRQ_COMPLETION_MARKER", old)
		} else {
			os.Unsetenv("CRQ_COMPLETION_MARKER")
		}
	})
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("CRQ_COMPLETION_MARKER=\"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRQ_CONFIG", path)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CompletionMarker != "" {
		t.Fatalf("explicit empty CRQ_COMPLETION_MARKER in config file should be preserved, got %q", cfg.CompletionMarker)
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
