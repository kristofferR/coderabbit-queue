package crq

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
)

// Setup people get wrong is setup that silently does nothing, which is the exact
// failure this whole feature exists to prevent. A dry run has to describe the
// real thing, and it must not touch anything.
func TestInstallDrainPlansWithoutWriting(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	agent := fakeAgent(t, "claude")
	plan, err := svc.InstallDrain(context.Background(), agent, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Started {
		t.Errorf("plan = %+v, want a dry run that started nothing", plan)
	}
	if plan.Agent != agent {
		t.Errorf("agent = %q, want the one asked for", plan.Agent)
	}
	if len(plan.Repos) != 1 || plan.Repos[0] != "owner/name" {
		t.Errorf("repos = %v, want the configured fleet", plan.Repos)
	}
	for _, path := range []string{plan.Prompt, plan.Wrapper, plan.Unit, plan.LogDir} {
		if path == "" {
			t.Fatalf("plan = %+v, want every path named so --dry-run is reviewable", plan)
		}
	}
	if len(plan.Commands) == 0 {
		t.Error("a plan that runs nothing cannot survive a logout")
	}
	// The service must be the platform's own, not Linux's everywhere.
	if runtime.GOOS == "darwin" && !strings.HasSuffix(plan.Unit, ".plist") {
		t.Errorf("unit = %q, want a launchd agent on macOS", plan.Unit)
	}
	if runtime.GOOS == "linux" && !strings.HasSuffix(plan.Unit, ".service") {
		t.Errorf("unit = %q, want a systemd unit on linux", plan.Unit)
	}
}

// A missing agent must fail loudly at install time. Discovering it at the first
// dispatch means a drain that looks installed and fixes nothing.
func TestInstallDrainRefusesWithoutAnAgent(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	_, err := svc.InstallDrain(context.Background(), "definitely-not-a-real-binary", nil, nil, true)
	if err == nil {
		t.Skip("a binary by that name exists on this machine")
	}
}

// The prompt crq installs is the one documented, because it is the same bytes.
func TestEmbeddedPromptCarriesTheRulesThatCostUs(t *testing.T) {
	for _, want := range []string{"DETACHED", "HEAD:refs/heads/", "crq resolve"} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("the embedded fix prompt no longer mentions %q", want)
		}
	}
}

// The service does not inherit the installing shell, and then crq reads its
// configuration file. A unit that names neither the file nor the settings that
// reached the install from the shell alone starts a watcher that loads a
// different queue — or none — while the install reports Started.
func TestDrainUnitCarriesTheConfigurationTheInstallRead(t *testing.T) {
	config := filepath.Join(t.TempDir(), "env")
	t.Setenv("CRQ_CONFIG", config)

	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	cfg.DashboardIssue = 7
	cfg.CalibrationPR = 1
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallDrain(context.Background(), fakeAgent(t, "claude"), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	unit := svc.drainUnit(plan)
	// The state ref included: without it the service falls back to the default
	// and reads a queue nobody else is using, which looks exactly like an idle
	// fleet.
	for _, want := range []string{config, cfg.GateRepo, "CRQ_ISSUE", "CRQ_CAL_PR", "CRQ_STATE_REF=" + cfg.StateRef} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit does not carry %q; the drain would not find it:\n%s", want, unit)
		}
	}
	// A secret in a file every local user can read is not the way to hand the
	// service a credential.
	if strings.Contains(unit, "GITHUB_TOKEN") || strings.Contains(unit, "GH_TOKEN") {
		t.Errorf("the unit carries a token:\n%s", unit)
	}
	// Rewriting it for the same configuration must produce the same file.
	if again := svc.drainUnit(plan); again != unit {
		t.Error("two renderings of one configuration differ; every re-install would rewrite the unit")
	}
}

func TestDrainUnitCarriesEffectiveReviewerConfiguration(t *testing.T) {
	cfg := firingConfig()
	cfg.Bot = "custom-reviewer[bot]"
	cfg.RequiredBots = []string{"custom-reviewer[bot]", "cursor[bot]"}
	cfg.FeedbackBots = []string{"custom-reviewer[bot]", "cursor[bot]", "observer[bot]"}
	cfg.ReviewCommand = "@custom review this"
	cfg.RateLimitCoDegrade = false
	cfg.CoBots = []CoBotConfig{{
		Name: "bugbot", Login: "cursor[bot]", Command: "bugbot run now",
		Trigger: engine.TriggerAlways, Required: true, SelfHealGrace: 4 * time.Minute,
	}}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	plan := DrainInstall{Platform: "linux", Wrapper: "/tmp/crq drain", LogDir: "/tmp/crq logs"}

	unit := svc.drainUnit(plan)
	for _, want := range []string{
		`CRQ_BOT=custom-reviewer[bot]`,
		`CRQ_REQUIRED_BOTS=custom-reviewer[bot],cursor[bot]`,
		`CRQ_FEEDBACK_BOTS=custom-reviewer[bot],cursor[bot],observer[bot]`,
		`CRQ_REVIEW_CMD=@custom review this`,
		`CRQ_COBOTS=bugbot`,
		`CRQ_COBOT_BUGBOT_CMD=bugbot run now`,
		`CRQ_COBOT_BUGBOT_TRIGGER=always`,
		`CRQ_COBOT_BUGBOT_REQUIRED=true`,
		`CRQ_COBOT_BUGBOT_GRACE=4m0s`,
		`CRQ_RL_CO_DEGRADE=0`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not carry %q:\n%s", want, unit)
		}
	}
}

func TestSystemdEnvironmentAssignmentsQuoteWhitespace(t *testing.T) {
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "config with spaces", "env"))
	t.Setenv("PATH", "/usr/bin:/opt/tools with spaces/bin")
	plan := DrainInstall{Platform: "linux", Wrapper: "/tmp/crq-drain", LogDir: "/tmp/crq"}

	unit := svc.drainUnit(plan)
	for _, key := range []string{"CRQ_CONFIG", "PATH", "CRQ_REVIEW_CMD"} {
		if !strings.Contains(unit, `Environment="`+key+`=`) {
			t.Errorf("%s assignment is not quoted:\n%s", key, unit)
		}
	}
	if strings.Contains(unit, "Environment=CRQ_CONFIG=") {
		t.Errorf("CRQ_CONFIG was emitted as an unquoted systemd assignment:\n%s", unit)
	}
}

func TestLaunchdMissingJobIsBenignOnlyForBootout(t *testing.T) {
	output := []byte("Boot-out failed: 3: No such process")
	if !launchdJobAbsent("launchctl bootout gui/501/no.kristofferr.crq-drain", output) {
		t.Error("first-install bootout should accept an absent launchd job")
	}
	if launchdJobAbsent("launchctl bootstrap gui/501 /tmp/drain.plist", output) {
		t.Error("a bootstrap failure must never be ignored")
	}
	if launchdJobAbsent("launchctl bootout gui/501/no.kristofferr.crq-drain", []byte("permission denied")) {
		t.Error("a genuine bootout failure must not be ignored")
	}
}

// systemd refuses to start a unit whose StandardOutput path cannot be opened
// (209/STDOUT), so a log directory that does not exist is a service that never
// runs — the silent nothing this command exists to prevent.
func TestInstallDrainNamesALogDirectory(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallDrain(context.Background(), fakeAgent(t, "claude"), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LogDir == "" {
		t.Fatal("no log directory planned; the unit would reference a path nothing creates")
	}
	unit := svc.drainUnit(plan)
	if !strings.Contains(unit, plan.LogDir) {
		t.Errorf("the unit does not write into the directory the install creates:\n%s", unit)
	}
}

// crq knows how to CALL the agents it supports and nothing about which model
// they should use — that belongs in the agent's own config or in --agent-args.
// Getting this wrong is invisible until a session starts and dies on a flag the
// binary does not have.
func TestAgentInvocationPerAgent(t *testing.T) {
	prompt := "/tmp/p.txt"

	claude, err := agentInvocation("/usr/bin/claude", prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-p ", "bypassPermissions", "stream-json", prompt} {
		if !strings.Contains(claude, want) {
			t.Errorf("claude invocation %q missing %q", claude, want)
		}
	}

	codex, err := agentInvocation("/usr/bin/codex", prompt, []string{"-c", "model_reasoning_effort=high"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "model_reasoning_effort=high", prompt} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex invocation %q missing %q", codex, want)
		}
	}
	// Codex takes the prompt as a positional; claude's -p is not its flag.
	if strings.Contains(codex, "--permission-mode") || strings.Contains(codex, "-p ") {
		t.Errorf("codex invocation carries claude's flags: %q", codex)
	}

	// No model anywhere: a queue does not choose the model.
	for _, inv := range []string{claude, codex} {
		if strings.Contains(inv, "--model") || strings.Contains(inv, "-m ") {
			t.Errorf("invocation hardcodes a model: %q", inv)
		}
	}

	// An unknown agent is refused rather than invoked with a guess.
	if _, err := agentInvocation("/usr/bin/mystery", prompt, nil); err == nil {
		t.Error("an unknown agent must be refused unless --agent-args says how to call it")
	}
	if got, err := agentInvocation("/usr/bin/mystery", prompt, []string{"--run"}); err != nil || !strings.Contains(got, "--run") {
		t.Errorf("an unknown agent with explicit args should be honoured, got %q err=%v", got, err)
	}
}

// fakeAgent is an executable with the given name, so an install test exercises
// the real "does this agent exist and how is it called" path.
func fakeAgent(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// "enable --now" does nothing to a unit that is already running, so a reinstall
// with a different agent kept the old one going — an install that reports
// success and changes nothing.
func TestInstallDrainRestartsAnAlreadyRunningService(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("launchd bootout/bootstrap already replaces a running agent")
	}
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallDrain(context.Background(), fakeAgent(t, "claude"), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	restarts := false
	for _, c := range plan.Commands {
		if strings.Contains(c, "restart") {
			restarts = true
		}
	}
	if !restarts {
		t.Errorf("commands = %v, want one that replaces a running service", plan.Commands)
	}
}
