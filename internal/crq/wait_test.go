package crq

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
)

// leader marks an autoreview daemon as live, which is what tells the waiter
// somebody else is advancing the queue and it may idle on the state ref.
func (f *replayFixture) leader(until time.Time) {
	f.t.Helper()
	if _, err := f.svc.store.Update(f.ctx, func(st *State) error {
		st.Leader = &LeaderLease{Owner: "daemon", Token: "t", ExpiresAt: until, UpdatedAt: f.clk.now()}
		return nil
	}); err != nil {
		f.t.Fatalf("set leader: %v", err)
	}
}

func (f *replayFixture) writeCount() int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	return len(f.gh.posted) + len(f.gh.deleted) + len(f.gh.createdIssues)
}

// The waiter's whole value is that it can be killed. It must therefore hold no
// state and write nothing — so a harness that SIGTERMs it between turns leaves
// the round exactly as it was, and re-running it (or crq next) is correct.
//
// The observed failures were the opposite: killing `crq loop` either re-fired a
// review on restart, spending account quota, or hit the dedupe and reported a
// converged round whose findings were never collected.
func TestWaitOwnsNothingAndWritesNothing(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 601
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	// The round is already under review, so the waiter has nothing to return yet.
	f.next(repo, pr)
	f.leader(f.clk.now().Add(time.Hour))
	before := f.writeCount()
	roundBefore := *f.round(repo, pr)

	// Idle deterministically: block in the sleep until the context dies, which
	// is exactly the state a harness SIGTERMs the waiter in.
	idling := make(chan struct{})
	var once sync.Once
	f.svc.sleepFn = func(ctx context.Context, _ time.Duration) error {
		once.Do(func() { close(idling) })
		<-ctx.Done()
		return ctx.Err()
	}

	// Kill it mid-wait, exactly as a harness does at a turn boundary.
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.WaitForAction(ctx, repo, pr) //nolint:errcheck // cancellation is the point
	}()
	<-idling
	cancel()
	<-done

	if got := f.writeCount(); got != before {
		t.Errorf("the wait wrote to GitHub (%d -> %d); it must own nothing", before, got)
	}
	after := f.round(repo, pr)
	if after == nil || after.Phase != roundBefore.Phase || after.Head != roundBefore.Head || after.Seq != roundBefore.Seq {
		t.Errorf("the round changed across a killed wait: %+v -> %+v", roundBefore, after)
	}

	// And the killed wait cost nothing: the answer is still there for the asking.
	f.svc.sleepFn = nil
	f.wantAction(f.next(repo, pr), engine.ActionWait)
}

// The waiter returns the moment there is something to act on, and returns the
// same instruction crq next would — they share one decision function precisely
// so the blocking and non-blocking forms cannot disagree.
func TestWaitReturnsWhenTheRoundBecomesActionable(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 602
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	f.next(repo, pr)
	f.leader(f.clk.now().Add(time.Hour))

	// Feedback lands before the wait starts, so it returns on its first pass
	// rather than idling — the cheap path is an optimization, not a delay.
	f.clk.advance(2 * time.Minute)
	f.botReview(repo, pr, 900, head, f.clk.now())
	f.botReviewComment(repo, pr, 901, head, "internal/state/state.go", 42,
		"_⚠️ Potential issue_\n\nThis dereferences a nil round.")

	report, err := f.svc.WaitForAction(f.ctx, repo, pr)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if report.Action != string(engine.ActionFix) {
		t.Fatalf("action = %q (%s), want %q", report.Action, report.Reason, engine.ActionFix)
	}
	if len(report.Findings) == 0 {
		t.Error("an actionable return must carry the findings to act on")
	}
}

// A converged round is actionable too: `done` is an answer, not a state to keep
// waiting through. Without this the waiter would idle forever on a finished PR.
func TestWaitReturnsOnConvergence(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 603
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	f.next(repo, pr)
	f.clk.advance(time.Minute)
	f.botReview(repo, pr, 900, head, f.clk.now())

	report, err := f.svc.WaitForAction(f.ctx, repo, pr)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if report.Action != string(engine.ActionDone) {
		t.Fatalf("action = %q (%s), want %q", report.Action, report.Reason, engine.ActionDone)
	}
}

// actionable is the whole return condition, so state it as a table: the two
// states a caller cannot act on are exactly the two the waiter absorbs.
func TestActionableStates(t *testing.T) {
	for kind, want := range map[engine.ActionKind]bool{
		engine.ActionFix:     true,
		engine.ActionPush:    true,
		engine.ActionDone:    true,
		engine.ActionBlocked: true,
		engine.ActionWait:    false,
		engine.ActionHold:    false,
	} {
		if got := actionable(kind); got != want {
			t.Errorf("actionable(%q) = %v, want %v", kind, got, want)
		}
	}
}

// Every interval the waiter idles on comes from config, and config can be zero.
// An unset LeaderTTL once put the staleness ceiling in the past, so the watch
// returned without idling and the outer loop spun against the API — the hot
// loop this command exists to prevent. The floor must be arithmetic.
func TestWaitNeverHotLoopsOnZeroedIntervals(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 604
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	f.next(repo, pr)
	f.leader(f.clk.now().Add(time.Hour))

	// The pathological config: no cadence and no lease period to derive one from.
	f.svc.cfg.PollInterval = 0
	f.svc.cfg.LeaderTTL = 0

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	var mu sync.Mutex
	var slept []time.Duration
	f.svc.sleepFn = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		n := len(slept)
		mu.Unlock()
		f.clk.advance(d)
		if n >= 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}

	if _, err := f.svc.WaitForAction(ctx, repo, pr); err == nil {
		t.Fatal("expected the cancelled wait to return an error")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) == 0 {
		t.Fatal("the waiter spun without ever idling")
	}
	for i, d := range slept {
		if d < minWaitTick {
			t.Errorf("sleep %d was %s, want at least the %s floor", i, d, minWaitTick)
		}
	}
}

// A live leader lease means "a daemon is advancing the queue", not "a daemon is
// advancing YOUR pr". The daemon only scans its own scope, so a PR outside the
// fleet with no round for this head would idle forever on a queue nobody was
// ever going to put it in. An untracked head drives itself regardless of who
// holds the lease.
func TestWaitDrivesAnUntrackedHeadEvenUnderALiveLeader(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/outside-the-fleet", 701
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	// A daemon holds the lease, but has never heard of this PR.
	f.leader(f.clk.now().Add(time.Hour))
	if r := f.round(repo, pr); r != nil {
		t.Fatalf("precondition: no round should exist yet, got %#v", r)
	}

	// Idle exactly once so the test cannot hang if the fix regresses.
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.svc.sleepFn = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	f.svc.WaitForAction(ctx, repo, pr) //nolint:errcheck // the cancel is the bound

	if r := f.round(repo, pr); r == nil {
		t.Fatal("the waiter must enqueue an untracked head instead of waiting on a daemon that cannot see it")
	}
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Errorf("the untracked head must get its review requested, posted %d", got)
	}
}

// A hold is itself the reason no round or live leader is advancing the PR.
// Driving Next in that state cannot change anything, but it repeats full
// feedback work every tick; park on the conditional state-ref watch instead.
func TestWaitParksAnAdministrativelyHeldPR(t *testing.T) {
	base := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 702
	head := "bbbbbbbb1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	if _, err := f.svc.Hold(f.ctx, repo, pr, "waiting on a decision"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(f.ctx)
	f.svc.sleepFn = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	if _, err := f.svc.WaitForAction(ctx, repo, pr); err == nil {
		t.Fatal("expected cancellation to stop the parked waiter")
	}
	if got := f.gh.reviewPolls(); got != 1 {
		t.Fatalf("held waiter performed %d full feedback reads before parking, want 1", got)
	}
	if r := f.round(repo, pr); r != nil {
		t.Fatalf("held waiter enqueued a round: %+v", r)
	}
	if got := f.reviewsPosted(repo, pr); got != 0 {
		t.Fatalf("held waiter posted %d reviews", got)
	}
}
