package crq

import (
	"context"
	"strings"
	"testing"
)

// Solver settings exist so two repositories the same watcher handles can be
// fixed differently, so what this pins is the layering and — because these
// values are handed to an agent's command line — the validation that stops a
// session dying on its first argument.
func TestSolverLayering(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DispatchMaxAttempts = 3
	cfg.DispatchCommand = []string{"/usr/bin/claude"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// Nothing recorded: every value is this host's env.
	view, err := svc.Solver(ctx, "o/plain")
	if err != nil {
		t.Fatal(err)
	}
	if view.Overridden || view.MaxAttempts != 3 || view.Model != "" {
		t.Fatalf("view = %+v, want the env values with no record", view)
	}
	if view.Agent != "/usr/bin/claude" {
		t.Errorf("agent = %q, want the fleet's — it is baked into the session script", view.Agent)
	}

	// A fleet default reaches every repository.
	if _, err := svc.SetFleetSolver(ctx, SolverChange{Effort: strptr("medium")}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/plain"); v.Effort != "medium" || v.Sources["effort"] != "fleet" {
		t.Errorf("view = %+v, want the fleet default applied and named", v)
	}

	// A repository's own record wins over it, field by field: setting the model
	// here must not discard the fleet's effort.
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{
		Model: strptr("opus"), MaxAttempts: intptr(5),
	}); err != nil {
		t.Fatal(err)
	}
	v, _ := svc.Solver(ctx, "o/special")
	if v.Model != "opus" || v.Sources["model"] != "repo" {
		t.Errorf("model = %q from %q, want the repository's own", v.Model, v.Sources["model"])
	}
	if v.Effort != "medium" || v.Sources["effort"] != "fleet" {
		t.Errorf("effort = %q from %q, want the fleet default still showing through", v.Effort, v.Sources["effort"])
	}
	if v.MaxAttempts != 5 {
		t.Errorf("attempts = %d, want the repository's 5 over the env's 3", v.MaxAttempts)
	}

	// And the resolved config is what a dispatch would actually run with.
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.cfgFor(st, "o/special"); got.FixModel != "opus" || got.DispatchMaxAttempts != 5 {
		t.Errorf("cfg = model %q attempts %d, want the record applied", got.FixModel, got.DispatchMaxAttempts)
	}
	if got := svc.cfgFor(st, "o/plain"); got.DispatchMaxAttempts != 3 {
		t.Errorf("attempts = %d, want another repository unaffected", got.DispatchMaxAttempts)
	}

	// A value the agent would reject is refused here instead.
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{Effort: strptr("turbo")}); err == nil {
		t.Error("an unknown effort must be refused before it reaches a command line")
	}
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{Prompt: strptr(strings.Repeat("x", 4001))}); err == nil {
		t.Error("an unbounded standing prompt must be refused — it is appended to every session")
	}
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{MaxAttempts: intptr(99)}); err == nil {
		t.Error("an absurd attempt budget must be refused")
	}

	// Emptying every field clears the record rather than leaving one that
	// overrides nothing.
	if _, err := svc.SetSolver(ctx, "o/special", SolverChange{
		Model: strptr(""), MaxAttempts: intptr(0),
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Solver(ctx, "o/special"); v.Overridden {
		t.Error("a record with nothing in it must not report the repository as overridden")
	}
}
