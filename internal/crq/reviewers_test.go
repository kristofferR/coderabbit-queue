package crq

import (
	"path/filepath"
	"testing"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// isolatedConfig loads config from an empty file so only the env vars a test sets
// are in play.
func isolatedConfig(t *testing.T, env map[string]string) Config {
	t.Helper()
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	for _, key := range []string{
		"CRQ_BOT", "CRQ_REQUIRED_BOTS", "CRQ_FEEDBACK_BOTS", "CRQ_COBOTS",
		"CRQ_COBOT_CODEX_TRIGGER", "CRQ_COBOT_BUGBOT_REQUIRED",
	} {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Budget is the property the queue exists for, so it has to be right before
// anything is built on it: exactly one reviewer is account-metered, and it is the
// configured primary.
func TestReviewersRecordWhatEachOneCosts(t *testing.T) {
	cfg := isolatedConfig(t, nil)

	primary, ok := cfg.Primary()
	if !ok {
		t.Fatal("a primary must be configured by default")
	}
	if primary.Login != "coderabbitai[bot]" || !primary.Metered() {
		t.Errorf("primary = %+v, want the account-metered CodeRabbit", primary)
	}
	if !primary.Required {
		t.Error("the primary gates convergence: a round it paid for is not done until it answers")
	}

	metered := 0
	for _, r := range cfg.Reviewers {
		if r.Metered() {
			metered++
			continue
		}
		if r.Budget != dialect.BudgetNone {
			t.Errorf("%s has budget %q, want none", r.Login, r.Budget)
		}
	}
	if metered != 1 {
		t.Errorf("%d metered reviewers, want exactly 1 — the queue serializes one allowance", metered)
	}
	if len(cfg.FreeRunning())+metered != len(cfg.Reviewers) {
		t.Error("every reviewer is either metered or free-running")
	}
}

// The legacy lists are views of the one list now, so they cannot answer
// differently from it. These are the questions each used to answer separately.
func TestLegacyListsAreDerivedFromTheReviewers(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"defaults", nil},
		{"codex required", map[string]string{"CRQ_REQUIRED_BOTS": "coderabbitai[bot],chatgpt-codex-connector[bot]"}},
		{"bugbot required via its own key", map[string]string{"CRQ_COBOT_BUGBOT_REQUIRED": "true"}},
		{"co-reviewers disabled", map[string]string{"CRQ_COBOTS": ""}},
		{"single co-reviewer", map[string]string{"CRQ_COBOTS": "codex"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := isolatedConfig(t, tc.env)

			want := cfg.reviewerLogins(func(r Reviewer) bool { return r.Required })
			if len(want) != len(cfg.RequiredBots) {
				t.Fatalf("RequiredBots = %v, want %v", cfg.RequiredBots, want)
			}
			for i := range want {
				if cfg.RequiredBots[i] != want[i] {
					t.Fatalf("RequiredBots = %v, want %v", cfg.RequiredBots, want)
				}
			}
			// Every co-reviewer entry corresponds to a free-running reviewer.
			if len(cfg.CoBots) != len(cfg.FreeRunning()) {
				t.Errorf("%d co-bots but %d free-running reviewers", len(cfg.CoBots), len(cfg.FreeRunning()))
			}
			// The primary is never in the co-reviewer set: that is what makes it
			// the one the fire slot serializes.
			primary, _ := cfg.Primary()
			for _, r := range cfg.FreeRunning() {
				if r.Login == primary.Login {
					t.Errorf("the metered primary must not appear as free-running: %s", r.Login)
				}
			}
		})
	}
}

// Feedback bots are the one list an operator may widen beyond who reviews, to
// surface a bot's findings without waiting for it — so an explicit setting still
// wins over the derivation.
func TestExplicitFeedbackBotsSurviveTheDerivation(t *testing.T) {
	cfg := isolatedConfig(t, map[string]string{"CRQ_FEEDBACK_BOTS": "someone[bot]"})
	if len(cfg.FeedbackBots) != 1 || cfg.FeedbackBots[0] != "someone[bot]" {
		t.Errorf("FeedbackBots = %v, want the explicit setting to win", cfg.FeedbackBots)
	}

	// With none set, it covers everyone who reviews.
	cfg = isolatedConfig(t, nil)
	if len(cfg.FeedbackBots) != len(cfg.Reviewers) {
		t.Errorf("FeedbackBots = %v, want one per reviewer (%d)", cfg.FeedbackBots, len(cfg.Reviewers))
	}
}

// The derivation has to describe the configuration that exists, not a tidier one.
// Each row here was a silent behaviour change found in review, hiding inside a
// change that claimed to be a pure refactor.
func TestDerivationLosesNothing(t *testing.T) {
	t.Run("a required bot outside the registry still gates", func(t *testing.T) {
		cfg := isolatedConfig(t, map[string]string{
			"CRQ_REQUIRED_BOTS": "coderabbitai[bot],sonar[bot]",
		})
		found := false
		for _, login := range cfg.RequiredBots {
			if login == "sonar[bot]" {
				found = true
			}
		}
		if !found {
			t.Errorf("RequiredBots = %v, want sonar[bot] retained — dropping it stops gating a reviewer the operator asked to wait for", cfg.RequiredBots)
		}
	})

	t.Run("the primary is not required unless it is listed", func(t *testing.T) {
		cfg := isolatedConfig(t, map[string]string{
			"CRQ_REQUIRED_BOTS": "chatgpt-codex-connector[bot]",
		})
		primary, _ := cfg.Primary()
		if primary.Required {
			t.Error("an explicit required list that omits the primary must be honoured")
		}
		for _, login := range cfg.RequiredBots {
			if login == primary.Login {
				t.Errorf("RequiredBots = %v, want the primary excluded", cfg.RequiredBots)
			}
		}
	})

	t.Run("a primary that is also a registry bot appears once", func(t *testing.T) {
		cfg := isolatedConfig(t, map[string]string{"CRQ_BOT": "chatgpt-codex-connector[bot]"})
		seen := 0
		for _, r := range cfg.Reviewers {
			if r.Login == "chatgpt-codex-connector[bot]" {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("codex appears %d times; it cannot be both metered and free-running", seen)
		}
	})

	t.Run("the primary knows how to be triggered", func(t *testing.T) {
		cfg := isolatedConfig(t, nil)
		primary, _ := cfg.Primary()
		if primary.Command != cfg.ReviewCommand || primary.Command == "" {
			t.Errorf("primary command = %q, want the resolved trigger %q — an empty one means crq cannot ask it at all",
				primary.Command, cfg.ReviewCommand)
		}
	})
}
