package crq

import (
	"context"
	"testing"
	"time"
)

// One setting, one place. A host that carries its own answer for something the
// fleet has decided is how a repository ends up excluded on one machine and
// reviewed by another, with nothing to say so.
func TestFleetPolicyOverridesTheHostsOwn(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/local": true}
	cfg.MinInterval = time.Minute
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, "repos", "owner/fleet-a,owner/fleet-b"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetFleetConfig(ctx, "min-interval", "5m"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := svc.fleetCfg(st)
	if !got.AllowRepos["owner/fleet-a"] || !got.AllowRepos["owner/fleet-b"] {
		t.Errorf("repos = %v, want the fleet's list", got.AllowRepos)
	}
	if got.AllowRepos["owner/local"] {
		t.Error("the host's own repository list survived the fleet's")
	}
	if got.MinInterval != 5*time.Minute {
		t.Errorf("min-interval = %s, want the fleet's 5m", got.MinInterval)
	}

	// A setting the fleet has no opinion on stays this host's answer.
	if got.SettleWindow != cfg.SettleWindow {
		t.Errorf("settle = %s, want the host's %s when the fleet is silent", got.SettleWindow, cfg.SettleWindow)
	}

	// And unsetting hands it back.
	if dropped, err := svc.UnsetFleetConfig(ctx, "min-interval"); err != nil || !dropped {
		t.Fatalf("unset = %v %v", dropped, err)
	}
	st, _, _ = store.Load(ctx)
	if svc.fleetCfg(st).MinInterval != time.Minute {
		t.Error("unsetting a fleet setting did not return the host's own value")
	}
}

// A value only some binaries can read would break the fleet from the inside,
// so it is refused where it is set — and if one arrives anyway, from a newer
// crq, the host keeps what it has rather than acting on half a policy.
func TestFleetRefusesWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.MinInterval = 90 * time.Second
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, "min-interval", "later"); err == nil {
		t.Error("a duration that does not parse was accepted")
	}
	if err := svc.SetFleetConfig(ctx, "min-interval", "-5m"); err == nil {
		t.Error("a negative window was accepted")
	}
	if err := svc.SetFleetConfig(ctx, "nonsense", "1"); err == nil {
		t.Error("an unknown setting was accepted")
	}

	// Planted directly, as a newer binary would leave it.
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetFleetValue("min-interval", "a fortnight")
		st.SetFleetValue("from-the-future", "whatever")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)
	if got := svc.fleetCfg(st).MinInterval; got != 90*time.Second {
		t.Errorf("min-interval = %s, want this host's 90s kept when the fleet's is unreadable", got)
	}
}

// Seeding is how an existing setup adopts this without retyping it, and it must
// not let a second machine overwrite the first one's answer.
func TestSeedingDoesNotOverwriteWhatTheFleetAlreadySays(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/from-this-host": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.SetFleetConfig(ctx, "repos", "owner/decided-already"); err != nil {
		t.Fatal(err)
	}
	seeded, err := svc.SeedFleetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range seeded {
		if key == "repos" {
			t.Error("seeding overwrote a setting the fleet had already recorded")
		}
	}
	st, _, _ := store.Load(ctx)
	if v, _ := st.FleetValue("repos"); v != "owner/decided-already" {
		t.Errorf("repos = %q, want the fleet's existing answer", v)
	}
	// Everything else it had no answer for is now recorded.
	if v, ok := st.FleetValue("settle"); !ok || v == "" {
		t.Errorf("settle = %q %v, want this host's value seeded", v, ok)
	}
}
