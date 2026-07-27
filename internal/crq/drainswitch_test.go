package crq

import (
	"context"
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// Draining is on by default and off only where somebody said so. A repository
// nobody has ruled on gets fixed, because that is what the watcher is for.
func TestDrainIsOnUnlessARepositorySaysOtherwise(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/one": true, "owner/two": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	settings, err := svc.DrainSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 2 {
		t.Fatalf("settings = %+v, want one per watched repository", settings)
	}
	for _, s := range settings {
		if !s.Enabled || !s.Default {
			t.Errorf("%s = %+v, want enabled by default", s.Repo, s)
		}
	}

	if _, err := svc.SetDrainEnabled(ctx, "Owner/One", false, "hand-tuned branch"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Case-insensitive, like every other repository key.
	if st.DrainEnabled("owner/one") {
		t.Error("an explicit off did not take")
	}
	if !st.DrainEnabled("owner/two") {
		t.Error("turning one repository off turned another off too")
	}

	// Back to the default is distinguishable from an explicit on.
	if cleared, err := svc.ClearDrainEnabled(ctx, "owner/one"); err != nil || !cleared {
		t.Fatalf("clear = %v %v, want it to report the setting it removed", cleared, err)
	}
	st, _, _ = store.Load(ctx)
	if !st.DrainEnabled("owner/one") {
		t.Error("clearing the setting did not return the repository to the default")
	}
}

// Off stops FIXING, not watching: the pull request is still observed and still
// reviewed, so its feedback arrives for a person to act on.
func TestDrainOffStillWatchesAndSaysWhy(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"owner/quiet": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State, pull.Number, pull.Head.SHA = "open", 5, "aaaaaaaa1"
	gh.pulls[fakeKey("owner/quiet", 5)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/quiet", 5, "aaaaaaaa1", PhaseQueued, time.Now().UTC(), 0)
	if _, err := svc.SetDrainEnabled(ctx, "owner/quiet", false, "release branch"); err != nil {
		t.Fatal(err)
	}

	var events []WatchEvent
	err := svc.Watch(ctx, WatchOptions{Once: true, Command: []string{"/bin/true"}},
		func(e WatchEvent) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("a repository with draining off stopped being watched")
	}
	for _, e := range events {
		if e.Dispatched {
			t.Errorf("a session ran for a repository with draining off: %+v", e)
		}
	}
}
