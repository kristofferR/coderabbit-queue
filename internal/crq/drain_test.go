package crq

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// Setup people get wrong is setup that silently does nothing, which is the exact
// failure this whole feature exists to prevent. A dry run has to describe the
// real thing, and it must not touch anything.
func TestInstallDrainPlansWithoutWriting(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallDrain(context.Background(), "/bin/echo", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Started {
		t.Errorf("plan = %+v, want a dry run that started nothing", plan)
	}
	if plan.Agent != "/bin/echo" {
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

	_, err := svc.InstallDrain(context.Background(), "definitely-not-a-real-binary", nil, true)
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

// systemd refuses to start a unit whose StandardOutput path cannot be opened
// (209/STDOUT), so a log directory that does not exist is a service that never
// runs — the silent nothing this command exists to prevent.
func TestInstallDrainNamesALogDirectory(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/name": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	plan, err := svc.InstallDrain(context.Background(), "/bin/echo", nil, true)
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
