package crq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
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

func TestLoadConfigSkipsDependabotByDefault(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_AUTOREVIEW_SKIP_AUTHORS", "dependabot[bot]")
	os.Unsetenv("CRQ_AUTOREVIEW_SKIP_AUTHORS")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SkipAuthors["dependabot"] {
		t.Fatalf("autoreview should skip dependabot PRs by default, got %#v", cfg.SkipAuthors)
	}
}

func TestLoadConfigEmptySkipAuthorsDisablesFilter(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_AUTOREVIEW_SKIP_AUTHORS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SkipAuthors) != 0 {
		t.Fatalf("explicit empty CRQ_AUTOREVIEW_SKIP_AUTHORS should re-enable bot PR reviews, got %#v", cfg.SkipAuthors)
	}
}

func TestLoadConfigDefaultAutoReviewSkipMarker(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_AUTOREVIEW_SKIP_MARKER", "restore-after-test")
	os.Unsetenv("CRQ_AUTOREVIEW_SKIP_MARKER")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkipMarker != "<!-- crq:skip-autoreview -->" {
		t.Fatalf("unexpected autoreview skip marker: %q", cfg.SkipMarker)
	}
}

func TestLoadConfigEmptyAutoReviewSkipMarkerDisablesOptOut(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_AUTOREVIEW_SKIP_MARKER", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkipMarker != "" {
		t.Fatalf("explicit empty marker should disable the opt-out, got %q", cfg.SkipMarker)
	}
}

func TestAuthorSetNormalizesCaseAndBotSuffix(t *testing.T) {
	set := authorSet("Dependabot[bot], renovate ,")
	if len(set) != 2 || !set["dependabot"] || !set["renovate"] {
		t.Fatalf("expected normalized {dependabot, renovate}, got %#v", set)
	}
}

func coBotByName(cfg Config, name string) (CoBotConfig, bool) {
	for _, cb := range cfg.CoBots {
		if cb.Name == name {
			return cb, true
		}
	}
	return CoBotConfig{}, false
}

func TestLoadConfigCoBotsDefaults(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	for _, key := range []string{"CRQ_REQUIRED_BOTS", "CRQ_FEEDBACK_BOTS", "CRQ_COBOTS", "CRQ_CODEX_CMD"} {
		t.Setenv(key, "")
	}
	os.Unsetenv("CRQ_COBOTS") // "" would disable all; the default is all known
	os.Unsetenv("CRQ_CODEX_CMD")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CoBots) != 3 {
		t.Fatalf("default CoBots = %#v, want codex+bugbot+macroscope", cfg.CoBots)
	}
	codex, _ := coBotByName(cfg, "codex")
	if codex.Trigger != engine.TriggerNever || codex.Command != "@codex review" || codex.Required {
		t.Fatalf("codex defaults wrong: %+v", codex)
	}

	bugbot, _ := coBotByName(cfg, "bugbot")
	if bugbot.Trigger != engine.TriggerSelfHeal || bugbot.Command != "bugbot run" || bugbot.SelfHealGrace != 10*time.Minute {
		t.Fatalf("bugbot defaults wrong: %+v", bugbot)
	}
	macro, _ := coBotByName(cfg, "macroscope")
	if macro.Trigger != engine.TriggerSelfHeal || macro.Command != "@macroscope-app review" {
		t.Fatalf("macroscope defaults wrong: %+v", macro)
	}
	// Wanted co-reviewers surface findings: their logins join FeedbackBots.
	for _, want := range []string{"cursor[bot]", "macroscopeapp[bot]", dialect.CodexBotLogin} {
		found := false
		for _, b := range cfg.FeedbackBots {
			if b == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("FeedbackBots missing %s: %#v", want, cfg.FeedbackBots)
		}
	}
	// Wanted-only bots never fold into RequiredBots.
	if len(cfg.RequiredBots) != 1 {
		t.Fatalf("RequiredBots must stay CodeRabbit-only by default: %#v", cfg.RequiredBots)
	}
}

func TestLoadConfigCoBotsExplicitEmptyDisablesAll(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "")
	t.Setenv("CRQ_FEEDBACK_BOTS", "")
	t.Setenv("CRQ_COBOTS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CoBots) != 0 {
		t.Fatalf("CRQ_COBOTS=\"\" must disable all co-bots, got %#v", cfg.CoBots)
	}

	if len(cfg.FeedbackBots) != 1 || cfg.FeedbackBots[0] != "coderabbitai[bot]" {
		t.Fatalf("FeedbackBots must collapse to the required set: %#v", cfg.FeedbackBots)
	}
}

func TestLoadConfigCoBotRequiredFoldsIntoRequiredBots(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "")
	t.Setenv("CRQ_COBOTS", "") // even disabled, required forces the entry
	t.Setenv("CRQ_COBOT_BUGBOT_REQUIRED", "1")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	bugbot, ok := coBotByName(cfg, "bugbot")
	if !ok || !bugbot.Required {
		t.Fatalf("required bugbot must be enabled: %#v", cfg.CoBots)
	}
	found := false
	for _, b := range cfg.RequiredBots {
		if b == "cursor[bot]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("required co-bot must fold into RequiredBots: %#v", cfg.RequiredBots)
	}
	// Required-ness does not change Bugbot's selfheal default (it auto-reviews).
	if bugbot.Trigger != engine.TriggerSelfHeal {
		t.Fatalf("bugbot trigger = %v, want selfheal", bugbot.Trigger)
	}
}

func TestLoadConfigRequiredBotsListingEnablesCoBot(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "coderabbitai[bot],"+dialect.CodexBotLogin)
	t.Setenv("CRQ_COBOTS", "bugbot")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	codex, ok := coBotByName(cfg, "codex")
	if !ok || !codex.Required {
		t.Fatalf("codex listed in CRQ_REQUIRED_BOTS must be required+enabled: %#v", cfg.CoBots)
	}
	// A configured-required Codex keeps today's fire-time trigger.
	if codex.Trigger != engine.TriggerAlways {
		t.Fatalf("required codex trigger = %v, want always", codex.Trigger)
	}
	if _, ok := coBotByName(cfg, "macroscope"); ok {
		t.Fatalf("macroscope not in CRQ_COBOTS and not required must be absent: %#v", cfg.CoBots)
	}
}

func TestLoadConfigCoBotCmdAliasesAndOverrides(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "")
	t.Setenv("CRQ_CODEX_CMD", "@codex ship it")
	t.Setenv("CRQ_COBOT_BUGBOT_TRIGGER", "always")
	t.Setenv("CRQ_COBOT_MACROSCOPE_CMD", "")
	t.Setenv("CRQ_COBOT_MACROSCOPE_GRACE", "5m")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	codex, _ := coBotByName(cfg, "codex")
	if codex.Command != "@codex ship it" {
		t.Fatalf("CRQ_CODEX_CMD alias not honored: %+v", codex)
	}
	bugbot, _ := coBotByName(cfg, "bugbot")
	if bugbot.Trigger != engine.TriggerAlways {
		t.Fatalf("bugbot trigger override not honored: %+v", bugbot)
	}
	macro, _ := coBotByName(cfg, "macroscope")
	if macro.Command != "" || macro.Trigger != engine.TriggerNever {
		t.Fatalf("an empty command must force trigger never: %+v", macro)
	}
	if macro.SelfHealGrace != 5*time.Minute {
		t.Fatalf("grace override not honored: %+v", macro)
	}

	// The per-bot key wins over the legacy alias.
	t.Setenv("CRQ_COBOT_CODEX_CMD", "@codex review please")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	codex, _ = coBotByName(cfg, "codex")
	if codex.Command != "@codex review please" {
		t.Fatalf("CRQ_COBOT_CODEX_CMD must win over CRQ_CODEX_CMD: %+v", codex)
	}
}

func TestLoadConfigRLCoDegradeAlias(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))

	t.Setenv("CRQ_RL_CO_DEGRADE", "0")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimitCoDegrade {
		t.Fatal("CRQ_RL_CO_DEGRADE=0 must disable the degrade")
	}

	os.Unsetenv("CRQ_RL_CO_DEGRADE")
	t.Setenv("CRQ_RL_CODEX_DEGRADE", "0")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimitCoDegrade {
		t.Fatal("legacy CRQ_RL_CODEX_DEGRADE=0 must still disable the degrade")
	}
}

func TestLoadConfigRejectsUnknownCoBot(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "")
	t.Setenv("CRQ_COBOTS", "codex,buugbot")

	// Silently skipping a typo disabled the co-reviewer the operator asked for,
	// and the symptom — a bot that simply never runs — looks nothing like a
	// misspelling, so this fails loudly instead.
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("a misspelled co-reviewer must fail configuration, not be dropped")
	}
	if !strings.Contains(err.Error(), "buugbot") || !strings.Contains(err.Error(), "bugbot") {
		t.Fatalf("the error must name the typo and the known bots, got %v", err)
	}
}

// CRQ_DISPATCH_CMD is one line of configuration that has to survive a multiword
// argument: splitting inside the quotes hands the fix agent three prompt
// fragments instead of one prompt.
func TestSplitArgvKeepsQuotedArgumentsWhole(t *testing.T) {
	got := splitArgv(`claude -p "fix these findings" --dir '/tmp/with space'`)
	want := []string{"claude", "-p", "fix these findings", "--dir", "/tmp/with space"}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Still not a shell: nothing here expands what the operator did not write.
	if argv := splitArgv(`echo $HOME *.go`); len(argv) != 3 || argv[1] != "$HOME" || argv[2] != "*.go" {
		t.Errorf("argv = %q, want the literal words back", argv)
	}
}
