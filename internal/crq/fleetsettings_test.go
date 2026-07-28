package crq

import (
	"context"
	"testing"
	"time"
)

// The three layers are the whole feature, so this pins their order and — just
// as importantly — that an absent fleet setting changes nothing at all. A fleet
// that never writes a record must behave exactly as it did before the record
// existed.
func TestFleetDefaultsLayering(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	cfg.MinInterval = 90 * time.Second
	cfg.WeeklyReviewLimit = 60
	cfg.AllowRepos = map[string]bool{"o/plain": true, "o/opinionated": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// No record: every value is this host's env, and .sources says so.
	view, err := svc.FleetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Recorded || view.MinInterval != "1m30s" || view.WeeklyLimit != 60 {
		t.Fatalf("view = %+v, want the env values with no record", view)
	}
	for key, src := range view.Sources {
		if src != "env" {
			t.Errorf("source[%s] = %q, want env before anything is recorded", key, src)
		}
	}
	if !view.AutofixDefault {
		t.Error("autofix defaults on, as it always has")
	}

	// A repository with its own override is NOT reached by a fleet default —
	// which is what the impact preview has to say before someone clicks.
	if _, err := svc.SetReviewers(ctx, "o/opinionated", []string{"codex"}, []string{"codex"}, nil); err != nil {
		t.Fatal(err)
	}
	impact, err := svc.PreviewFleet(ctx, FleetChange{MinInterval: strptr("3m")})
	if err != nil {
		t.Fatal(err)
	}
	if impact.Overridden != 1 {
		t.Errorf("overridden = %d, want the one repository with its own answer excluded", impact.Overridden)
	}
	if len(impact.Changes) != 1 {
		t.Errorf("changes = %v, want the pacing change named", impact.Changes)
	}
	// A preview must not write.
	if v, _ := svc.FleetSettings(ctx); v.Recorded {
		t.Fatal("a preview wrote a record")
	}

	// Recording it changes the effective config for repositories that follow.
	if _, _, err := svc.SetFleetSettings(ctx, FleetChange{
		MinInterval: strptr("3m"), WeeklyLimit: intptr(90), AutofixDefault: boolptr(false),
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.cfgFor(st, "o/plain").MinInterval; got != 3*time.Minute {
		t.Errorf("min interval = %s, want the fleet record to win over env", got)
	}
	if got := svc.cfgFor(st, "o/plain").WeeklyReviewLimit; got != 90 {
		t.Errorf("weekly limit = %d, want 90", got)
	}
	if st.AutofixEnabled("o/never-ruled-on") {
		t.Error("a repository with no switch must follow the fleet default, which is now off")
	}
	view, _ = svc.FleetSettings(ctx)
	if view.Sources["min_interval"] != "fleet" || view.Sources["reviewers"] != "env" {
		t.Errorf("sources = %v, want only the recorded settings sourced from the fleet", view.Sources)
	}

	// The override still wins over the record.
	if got := svc.cfgFor(st, "o/opinionated").RequiredBots; len(got) != 1 {
		t.Errorf("required = %v, want the repository's own answer to survive a fleet default", got)
	}

	// Gating on nobody is refused here for the same reason it is per repo.
	if _, err := svc.PreviewFleet(ctx, FleetChange{Required: []string{}}); err == nil {
		t.Error("an empty required set must be refused")
	}
	// So is pacing fast enough to be meaningless.
	if _, err := svc.PreviewFleet(ctx, FleetChange{MinInterval: strptr("1s")}); err == nil {
		t.Error("a sub-5s pacing floor must be refused")
	}

	// Clearing returns every setting to env.
	if _, _, err := svc.SetFleetSettings(ctx, FleetChange{Clear: true}); err != nil {
		t.Fatal(err)
	}
	st, _, _ = store.Load(ctx)
	if got := svc.cfgFor(st, "o/plain").MinInterval; got != 90*time.Second {
		t.Errorf("min interval = %s, want the env value back", got)
	}
	if !st.AutofixEnabled("o/never-ruled-on") {
		t.Error("clearing must restore the default-on answer")
	}
}

func strptr(s string) *string { return &s }
func intptr(n int) *int       { return &n }
func boolptr(b bool) *bool    { return &b }
