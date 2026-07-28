package crq

import (
	"context"
	"strings"
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
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
	// "default" and "env" are different answers: firingConfig sets none of
	// these, so nothing here comes from a file and saying "env" would send a
	// reader looking for a line that does not exist.
	for key, src := range view.Sources {
		if src != "default" {
			t.Errorf("source[%s] = %q, want default when nothing sets it", key, src)
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
	if view.Sources["min_interval"] != "fleet" || view.Sources["reviewers"] != "default" {
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

// Unsetting a fleet setting has to actually unset it. The typed fields — the
// two lists and the weekly limit — are the ones where "leave this alone" and
// "the fleet has no answer" look identical in JSON, so this is where a clear
// could report success and change nothing.
func TestSetEnvClearsTypedFleetSettings(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.WeeklyReviewLimit = 90 // this host's env, which a clear must hand back
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	for _, set := range []struct{ key, value string }{
		{"CRQ_COBOTS", "codex"},
		{"CRQ_REQUIRED_BOTS", "coderabbitai[bot]"},
		{"CRQ_WEEKLY_LIMIT", "60"},
	} {
		if _, err := svc.SetEnv(ctx, set.key, set.value, false); err != nil {
			t.Fatalf("recording %s: %v", set.key, err)
		}
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Fleet.SetCoBots || !st.Fleet.SetRequired || st.Fleet.WeeklyLimit == nil {
		t.Fatalf("fleet = %+v, want all three recorded before the clear", st.Fleet)
	}

	for _, key := range []string{"CRQ_COBOTS", "CRQ_REQUIRED_BOTS", "CRQ_WEEKLY_LIMIT"} {
		if _, err := svc.SetEnv(ctx, key, "", true); err != nil {
			t.Fatalf("clearing %s: %v", key, err)
		}
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fleet.SetCoBots || st.Fleet.CoBots != nil {
		t.Errorf("cobots = %v (set=%v), want the record gone", st.Fleet.CoBots, st.Fleet.SetCoBots)
	}
	if st.Fleet.SetRequired || st.Fleet.Required != nil {
		t.Errorf("required = %v (set=%v), want the record gone", st.Fleet.Required, st.Fleet.SetRequired)
	}
	if st.Fleet.WeeklyLimit != nil {
		t.Errorf("weekly limit = %d, want the pointer removed rather than a default written",
			*st.Fleet.WeeklyLimit)
	}
	// The point of removing it: this host's own 90 answers again.
	if got := svc.cfgFor(st, "o/plain").WeeklyReviewLimit; got != 90 {
		t.Errorf("weekly limit = %d, want this host's env value back", got)
	}
}

// A fleet default reaches the repositories that follow the fleet, and a
// repository somebody turned off follows nothing. It has both an enrollment
// record and completed rounds, so it used to reach the requeue set twice over —
// and saving an unrelated default then spent quota there.
func TestReposFollowingFleetExcludesDisabledRepositories(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/on": true, "o/off": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/off", false, "not reviewing this one"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range svc.reposFollowingFleet(st) {
		if repo == "o/off" {
			t.Fatalf("following = %v, want the disabled repository excluded", svc.reposFollowingFleet(st))
		}
	}
}

func strptr(s string) *string { return &s }
func intptr(n int) *int       { return &n }
func boolptr(b bool) *bool    { return &b }

// A fleet-recorded primary has to be the one crq actually asks. The decision
// resolves the record; the apply path used to post this host's startup value —
// so the daemon spent the account's quota reserving a round for one bot and
// then sent the other bot's command, and waited for a review nobody requested.
func TestFireUsesTheFleetResolvedPrimary(t *testing.T) {
	ctx := context.Background()
	// Built from an env map rather than a literal: the fleet's generic settings
	// are applied by re-parsing the configuration crq was built from, which is
	// what a host actually has.
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_HOST": "testhost",
		"CRQ_COBOTS": "", "CRQ_MIN_INTERVAL": "0s", "CRQ_POLL": "1ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnv(ctx, "CRQ_REVIEW_CMD", "@coderabbitai full review", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	if pumped, err := svc.Pump(ctx); err != nil || pumped.Action != "fired" {
		t.Fatalf("pump = %+v, err = %v, want a fired round", pumped, err)
	}
	if len(gh.posted) != 1 || !strings.HasSuffix(gh.posted[0], "@coderabbitai full review") {
		t.Fatalf("posted %q, want the fleet's recorded review command", gh.posted)
	}
	// And the round records the command as the primary's, so the wait is for a
	// bot that was actually asked.
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round("owner/repo", 12)
	if round == nil || len(round.PostedCommands) != 1 || round.PostedCommands[0].Bot != cfg.Bot {
		t.Errorf("posted commands = %+v, want one attributed to %s", round.PostedCommands, cfg.Bot)
	}
}

// A repository that states ONE half of its reviewers still inherits the other
// from the fleet. Treating either half as a complete override dropped it from
// the impact preview and from the requeue: cfgFor handed it the newly required
// reviewer, its completed round stayed a "this head was reviewed" marker, and
// the reviewer somebody had just required was never asked there.
func TestFleetReachesRepositoriesThatOverrideOnlyOneHalf(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.CoBots = codexCoBots(nil)
	cfg.AllowRepos = map[string]bool{"o/half": true, "o/whole": true}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	// o/half names its co-reviewers and inherits the required set; o/whole
	// answers both questions itself.
	if _, err := svc.SetReviewers(ctx, "o/half", []string{"codex"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetReviewers(ctx, "o/whole", []string{"codex"}, []string{"codex"}, nil); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	following := map[string]bool{}
	for _, repo := range svc.reposFollowingFleet(st) {
		following[repo] = true
	}
	if !following["o/half"] {
		t.Errorf("following = %v, want the half-overridden repository included: it still inherits the required set",
			svc.reposFollowingFleet(st))
	}
	if following["o/whole"] {
		t.Errorf("following = %v, want the fully-overridden repository excluded", svc.reposFollowingFleet(st))
	}

	// And the preview counts as "unaffected" only the one that really is.
	impact, err := svc.PreviewFleet(ctx, FleetChange{Required: []string{"coderabbitai[bot]", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if impact.Overridden != 1 {
		t.Errorf("overridden = %d, want only the repository that answers both questions itself", impact.Overridden)
	}
}
