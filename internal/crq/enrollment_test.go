package crq

import (
	"context"
	"strings"
	"testing"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// TestEnrollmentPrecedence pins the order the whole feature rests on. Getting it
// wrong is not a cosmetic bug: too permissive and crq reviews a repository
// somebody deliberately kept it out of, too strict and the dashboard's Off
// button silently does nothing.
func TestEnrollmentPrecedence(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/allowed": true, "o/fought-over": true}
	cfg.ExcludeRepos = map[string]bool{"o/killed": true}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	// A repository named by neither list, on a host that HAS an allow-list, is
	// off — the allow-list is the whole statement of what this host looks at.
	if v, _ := svc.Enrollment(ctx, "o/unknown"); v.Enabled || v.Source != "off" {
		t.Errorf("unknown repo = %+v, want off", v)
	}
	if v, _ := svc.Enrollment(ctx, "o/allowed"); !v.Enabled || v.Source != "env" {
		t.Errorf("allowed repo = %+v, want enabled by env", v)
	}
	if v, _ := svc.Enrollment(ctx, "o/killed"); v.Enabled || v.Source != "excluded" {
		t.Errorf("excluded repo = %+v, want excluded", v)
	}
	if v, _ := svc.Enrollment(ctx, cfg.GateRepo); v.Enabled {
		t.Errorf("gate repo = %+v, want excluded: it holds crq's own state", v)
	}

	// CRQ_EXCLUDE is a per-host kill switch and shared state does not override
	// it, so the write is refused rather than recorded and ignored.
	if _, err := svc.SetEnrollment(ctx, "o/killed", true, ""); err == nil {
		t.Error("enrolling an env-excluded repo must be refused, not silently recorded")
	}

	// Turning one off is the direction that needs a reason, because it makes a
	// repository disappear from every queue.
	if _, err := svc.SetEnrollment(ctx, "o/fought-over", false, ""); err == nil {
		t.Error("removing without a reason must be refused")
	}

	// A record beats env in BOTH directions. An Off that only tells you which
	// file to edit on another machine is not a switch.
	view, err := svc.SetEnrollment(ctx, "o/fought-over", false, "archived")
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled || view.Source != "state" || !view.EnvConflict {
		t.Errorf("view = %+v, want off by record with the env disagreement reported", view)
	}

	// And it enrolls a repository env never mentioned — which is not a
	// conflict, it is the feature working.
	view, err = svc.SetEnrollment(ctx, "o/added", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Enabled || view.EnvConflict {
		t.Errorf("view = %+v, want enabled with no conflict reported", view)
	}

	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targets, scoped := svc.scanTargets(st)
	if scoped {
		t.Error("a host with an allow-list must search by repository, not owner-wide")
	}
	want := map[string]bool{"o/allowed": true, "o/added": true}
	if len(targets) != len(want) {
		t.Fatalf("scan targets = %v, want exactly %v", targets, want)
	}
	for _, repo := range targets {
		if !want[repo] {
			t.Errorf("scan targets = %v, want %v — a repository turned off must not be searched", targets, want)
		}
	}

	// default hands it back to env.
	if view, err = svc.ClearEnrollment(ctx, "o/fought-over"); err != nil {
		t.Fatal(err)
	}
	if !view.Enabled || view.Source != "env" {
		t.Errorf("view = %+v, want the env answer back", view)
	}
}

// A host with no allow-list searches its whole CRQ_SCOPE. Records must not
// narrow that to themselves, or enrolling one repository would silently stop
// every other one from being scanned.
func TestEnrollmentDoesNotNarrowAScopeWideHost(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"o"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	if v, _ := svc.Enrollment(ctx, "o/anything"); !v.Enabled || v.Source != "scope" {
		t.Errorf("view = %+v, want enabled by scope", v)
	}
	if _, err := svc.SetEnrollment(ctx, "o/one", true, ""); err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if targets, scoped := svc.scanTargets(st); len(targets) != 0 || !scoped {
		t.Errorf("scan targets = %v (scoped %v), want none and scope mode so the pass still searches the whole scope", targets, scoped)
	}
	// The off direction still works there: the per-PR gate reads the record.
	if _, err := svc.SetEnrollment(ctx, "o/noisy", false, "too busy"); err != nil {
		t.Fatal(err)
	}
	st, _, err = svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if svc.reviewsRepo(st, "o/noisy") {
		t.Error("a repository turned off must not be reviewed, scope-wide host or not")
	}
	if !svc.reviewsRepo(st, "o/other") {
		t.Error("turning one repository off must not affect the rest of the scope")
	}
}

// Enrolling a repository is the one click in the product that spends money, so
// the dialog's rule is that an unknown price must never read as a free one. A
// per-PR pricing call that fails — a spent REST quota, an unreadable diff — used
// to be skipped silently, and a backlog nothing could be priced for was
// summarised as having "no per-review cost".
func TestEnrollSummaryNeverPricesAnUnknownAsFree(t *testing.T) {
	none := enrollSummary(EnrollImpact{Open: 4, Eligible: 4, Unpriced: 4}, false)
	if strings.Contains(none, "no per-review cost") {
		t.Errorf("summary = %q, want an unpriced backlog reported as unknown", none)
	}
	if !strings.Contains(none, "could not") {
		t.Errorf("summary = %q, want it to say the cost could not be read", none)
	}
	// A partly priced backlog states both: the money it knows about, and how
	// many pull requests are not in that number.
	partial := enrollSummary(EnrollImpact{Open: 4, Eligible: 4, Low: 1, High: 2, Unpriced: 2}, false)
	if !strings.Contains(partial, "$1.00–$2.00") || !strings.Contains(partial, "2 that could not be priced") {
		t.Errorf("summary = %q, want the known cost and the unpriced count", partial)
	}
	// And a fully priced free backlog still says so.
	free := enrollSummary(EnrollImpact{Open: 2, Eligible: 2}, false)
	if !strings.Contains(free, "no per-review cost") {
		t.Errorf("summary = %q, want a genuinely free backlog unchanged", free)
	}
}

// Turning a repository off has to remove the work already queued for it, not
// merely stop new scans finding more. Pump chooses from Rounds through
// NextEligible, which asks nothing about enrollment — so a queued round kept its
// place and spent the shared allowance on a metered review minutes after
// somebody stopped the repository.
func TestDisablingEnrollmentDropsTheQueuedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/stopped": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/stopped#7"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.Enqueue(ctx, "o/stopped", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "stop reviewing this"); err != nil {
		t.Fatal(err)
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/stopped", 7); round != nil {
		t.Fatalf("round = %+v, want it archived rather than left fire-eligible", round)
	}
	if next := st.NextEligible(svc.clock()); next != nil {
		t.Errorf("next eligible = %+v, want nothing for a repository that was turned off", next)
	}
	// The round is archived, never deleted: it says why it stopped.
	if len(st.Archive) != 1 || st.Archive[0].Phase != PhaseAbandoned ||
		!strings.Contains(st.Archive[0].Note, "turned off") {
		t.Errorf("archive = %+v, want the round kept with the reason it ended", st.Archive)
	}

	// Turning it back on enqueues the head again — an off switch somebody can
	// undo has to hand the repository back the way it found it.
	if _, err := svc.SetEnrollment(ctx, "o/stopped", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/stopped", 7); err != nil {
		t.Fatal(err)
	}
	st, _, _ = store.Load(ctx)
	if round := st.Round("o/stopped", 7); round == nil || round.Phase != PhaseQueued {
		t.Errorf("round = %+v, want a fresh queued round once the repository is back on", round)
	}
}

// Clearing a record hands the repository back to this host's env, which need
// not list it: a record that said ON becomes an effective OFF without
// SetEnrollment ever being called. Pump asks Rounds, not enrollment, so the
// queued work has to go the same way it does when the switch is thrown.
func TestClearingEnrollmentIntoAnExcludingEnvDropsTheQueuedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	// Nonempty and WITHOUT o/adopted: the record is the only thing enrolling it.
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/adopted#3"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/adopted", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/adopted", 3); err != nil {
		t.Fatal(err)
	}
	view, err := svc.ClearEnrollment(ctx, "o/adopted")
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled {
		t.Fatalf("view = %+v, want the env's answer, which omits this repository", view)
	}

	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/adopted", 3); round != nil {
		t.Errorf("round = %+v, want it archived rather than left fire-eligible", round)
	}
	if next := st.NextEligible(svc.clock()); next != nil {
		t.Errorf("next eligible = %+v, want nothing for a repository nothing enrolls now", next)
	}
}

// The converse: clearing a record for a repository this host's env DOES list
// leaves it enrolled, so its queued work must survive untouched.
func TestClearingEnrollmentBackIntoEnvKeepsTheQueuedRounds(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/listed#5"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/listed", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, "o/listed", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClearEnrollment(ctx, "o/listed"); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/listed", 5); round == nil || round.Phase != PhaseQueued {
		t.Errorf("round = %+v, want the queued round untouched: env still enrolls this repository", round)
	}
}

// An allow-list with every entry switched off is not the same as no allow-list.
// Treating both as "search CRQ_SCOPE owner-wide" made every pass walk the whole
// organisation's open-PR result set for the per-PR gate to reject each row —
// the shared REST quota spent by a host with nothing left to review.
func TestAPassWithAnAllowListButNoActiveRepositorySearchesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Scope = []string{"o"}
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{{Repo: "o/elsewhere", Number: 1, Title: "t"}}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	if _, err := svc.SetEnrollment(ctx, "o/listed", false, "archived"); err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if targets, scoped := svc.scanTargets(st); len(targets) != 0 || scoped {
		t.Fatalf("scan targets = %v (scoped %v), want none and NOT scope mode", targets, scoped)
	}
	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	if gh.searches != 0 {
		t.Errorf("searched %d time(s); a host with no eligible repository has nothing to look for", gh.searches)
	}
}

// The off switch abandons a repository's pending rounds, and every SCAN path
// honours it — but Enqueue is the manual path, and Pump asks nothing about
// enrollment. A `crq next` or `crq loop` run afterwards recreated the round and
// spent a metered review on a repository somebody had deliberately stopped.
func TestEnqueueRefusesARepositoryTurnedOff(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["o/stopped#7"] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.SetEnrollment(ctx, "o/stopped", false, "archived"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Enqueue(ctx, "o/stopped", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Held || !strings.Contains(result.Reason, "archived") {
		t.Errorf("result = %+v, want it refused with the reason the record carries", result)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round("o/stopped", 7); round != nil {
		t.Errorf("round = %+v, want none: a manual enqueue must not undo the off switch", round)
	}

	// A repository this host's env simply does not list is NOT turned off. A
	// manual run against one is the ordinary way `crq next` is used, and
	// refusing it would break every repository outside the fleet's allow-list.
	cfg.AllowRepos = map[string]bool{"o/listed": true}
	gh.pulls["o/unlisted#8"] = pull
	other := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	if result, err := other.Enqueue(ctx, "o/unlisted", 8); err != nil || result.Held {
		t.Errorf("result = %+v, err = %v, want a manual enqueue on an unlisted repository to work", result, err)
	}
}
