package crq

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// setLocalWork pins Next's "does the caller hold unlanded changes" probe so the
// replay does not depend on the checkout the test happens to run in.
func (f *replayFixture) setLocalWork(has bool, reason string) {
	f.svc.localWorkFn = func(context.Context, string) (bool, string) { return has, reason }
}

func (f *replayFixture) closePull(repo string, pr int) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	p := f.gh.pulls[fakeKey(repo, pr)]
	p.State = "closed"
	f.gh.pulls[fakeKey(repo, pr)] = p
}

func (f *replayFixture) next(repo string, pr int) NextReport {
	f.t.Helper()
	report, err := f.svc.Next(f.ctx, repo, pr)
	if err != nil {
		f.t.Fatalf("next %s#%d: %v", repo, pr, err)
	}
	return report
}

func (f *replayFixture) wantAction(report NextReport, want engine.ActionKind) {
	f.t.Helper()
	if report.Action != string(want) {
		f.t.Fatalf("action = %q (%s), want %q", report.Action, report.Reason, want)
	}
}

// A whole review round driven only by `crq next`: the caller never chooses a
// delay, never decides when the head may move, and never reads an exit code.
// Each step asserts the instruction crq returns for a state an agent would
// otherwise have to reason about on its own.
func TestNextDrivesAReviewRound(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 501
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	// 1. Nothing has happened yet: crq fires the review and tells the caller to
	//    wait — with a time it computed, not one the caller invented.
	first := f.next(repo, pr)
	f.wantAction(first, engine.ActionWait)
	if first.RecheckAfter == nil {
		t.Fatal("wait must carry a recheck time")
	}
	if !first.RecheckAfter.After(f.clk.now()) {
		t.Errorf("recheck_after %s is not in the future", first.RecheckAfter)
	}
	if r := f.round(repo, pr); r == nil || r.FiredAt == nil {
		t.Fatalf("next must advance the queue: round = %+v", r)
	}

	// 2. The reviewer answers with a finding. The caller is told to fix it.
	f.clk.advance(2 * time.Minute)
	f.botReview(repo, pr, 900, head, f.clk.now())
	f.botReviewComment(repo, pr, 901, head, "internal/state/state.go", 42,
		"_⚠️ Potential issue_\n\nThis dereferences a nil round.")
	withFinding := f.next(repo, pr)
	f.wantAction(withFinding, engine.ActionFix)
	if len(withFinding.Findings) == 0 {
		t.Fatal("fix must carry the findings to act on")
	}

	// 3. The head moved and the caller still holds changes. No review has been
	//    requested for this head, so holding would stall for nobody — and
	//    queueing one now would spend a window on code about to be replaced.
	//    Land it first.
	f.setLocalWork(true, "uncommitted changes in the working tree")
	f.clk.advance(time.Minute)
	f.setHead(repo, pr, "bbbbbbbb2")
	f.setCommitDate("bbbbbbbb2", f.clk.now())
	postedBefore := f.reviewsPosted(repo, pr)
	f.wantAction(f.next(repo, pr), engine.ActionPush)
	if got := f.reviewsPosted(repo, pr); got != postedBefore {
		t.Errorf("a head that is about to move must not buy a review: posted %d -> %d", postedBefore, got)
	}

	// Once a review IS running for this head, the head must not move.
	f.setLocalWork(false, "")
	f.enqueue(repo, pr)
	f.pump()
	f.setLocalWork(true, "uncommitted changes in the working tree")
	held := f.next(repo, pr)
	if held.Action != string(engine.ActionHold) && held.Action != string(engine.ActionWait) {
		t.Fatalf("action = %q (%s), want hold or wait while the review runs", held.Action, held.Reason)
	}
	if held.RecheckAfter == nil {
		t.Error("a hold must say when to look again")
	}

	// 4. The reviewer answers on the new head with nothing to say, and the
	//    caller has nothing left locally: converged.
	f.setLocalWork(false, "")
	f.clk.advance(time.Minute)
	f.pump()
	f.botReview(repo, pr, 902, "bbbbbbbb2", f.clk.now())
	done := f.next(repo, pr)
	f.wantAction(done, engine.ActionDone)
	if len(done.Findings) != 0 {
		t.Errorf("done must carry no findings, got %d", len(done.Findings))
	}
}

// `next` advances the queue as a side effect, and that step owns the review
// fire — so the two halves of drain-first are both properties of ONE decision:
// undrained feedback for the current head must not buy another review of that
// same head, and feedback carried from an older commit must not stop the new
// head from being reviewed at all.
func TestNextDrainsCurrentHeadWithoutDeadlockingTheNextOne(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 504
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	f.next(repo, pr)
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("the first call must fire exactly one review, got %d", got)
	}

	// Feedback lands ON this head. Until the caller drains it, another review of
	// the same head would spend account quota to be told the same thing.
	f.clk.advance(2 * time.Minute)
	f.botReview(repo, pr, 900, head, f.clk.now())
	f.botReviewComment(repo, pr, 901, head, "internal/state/state.go", 42,
		"_⚠️ Potential issue_\n\nThis dereferences a nil round.")
	f.wantAction(f.next(repo, pr), engine.ActionFix)
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("undrained feedback for this head must not buy another review, posted %d", got)
	}

	// The caller pushes a fix. The old finding belongs to the previous commit,
	// so it can no longer be acted on here — and must not keep the new head from
	// being reviewed. This is the deadlock the narrow gate exists to avoid.
	f.clk.advance(time.Minute)
	f.setHead(repo, pr, "bbbbbbbb2")
	f.setCommitDate("bbbbbbbb2", f.clk.now())
	f.next(repo, pr)
	if got := f.reviewsPosted(repo, pr); got != 2 {
		t.Fatalf("the new head must still get its own review, posted %d", got)
	}
}

// A closed PR is not something a loop can resolve by waiting.
func TestNextBlocksOnClosedPR(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 502
	f.openPull(repo, pr, "aaaaaaaa1")
	f.setCommitDate("aaaaaaaa1", base.Add(-time.Minute))
	f.setLocalWork(false, "")
	f.next(repo, pr)

	f.closePull(repo, pr)
	f.wantAction(f.next(repo, pr), engine.ActionBlocked)
}

// Every wait crq hands back is at least a poll interval away, so a caller
// looping on `crq next` cannot spin — the floor is arithmetic, not etiquette.
func TestNextNeverSchedulesAHotLoop(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 503
	f.openPull(repo, pr, "aaaaaaaa1")
	f.setCommitDate("aaaaaaaa1", base.Add(-time.Minute))
	f.setLocalWork(false, "")

	for i := 0; i < 3; i++ {
		report := f.next(repo, pr)
		if report.RecheckAfter == nil {
			continue
		}
		if got := report.RecheckAfter.Sub(f.clk.now()); got < f.cfg.PollInterval {
			t.Fatalf("recheck in %s, want at least the poll interval %s", got, f.cfg.PollInterval)
		}
	}
}

func TestNextReportsAdministrativeHoldAfterDrainFirst(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 505
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")
	if _, err := f.store.Update(f.ctx, func(st *State) error {
		st.Hold(repo, pr, "waiting on a decision", "operator", base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	held := f.next(repo, pr)
	f.wantAction(held, engine.ActionBlocked)
	if held.Reason != "held: waiting on a decision" {
		t.Errorf("reason = %q, want the administrative hold", held.Reason)
	}
	if held.RecheckAfter != nil {
		t.Errorf("an administrative hold cannot clear on a timer: %s", held.RecheckAfter)
	}
	if got := f.reviewsPosted(repo, pr); got != 0 {
		t.Fatalf("held PR posted %d reviews", got)
	}

	// Current-head findings still come first: the hold must not hide work that
	// can be drained without moving the head.
	f.botReviewComment(repo, pr, 901, head, "internal/state/state.go", 42,
		"_⚠️ Potential issue_\n\nThis dereferences a nil round.")
	f.wantAction(f.next(repo, pr), engine.ActionFix)
}

// The local-work probe decides push-vs-done, so "is this checkout even the
// PR's repository" has to be exact. Substring matching made owner/app match a
// checkout of owner/application and read its HEAD as unlanded work.
func TestRemoteMatchesRepo(t *testing.T) {
	remotes := func(urls ...string) string {
		var b strings.Builder
		for i, u := range urls {
			fmt.Fprintf(&b, "r%d\t%s (fetch)\nr%d\t%s (push)\n", i, u, i, u)
		}
		return b.String()
	}
	cases := []struct {
		name    string
		remotes string
		repo    string
		want    bool
	}{
		{"https", remotes("https://github.com/owner/app.git"), "owner/app", true},
		{"scp form", remotes("git@github.com:owner/app.git"), "owner/app", true},
		{"ssh url", remotes("ssh://git@github.com/owner/app"), "owner/app", true},
		{"host alias for a second account", remotes("git@github.com-work:owner/app.git"), "owner/app", true},
		{"case insensitive", remotes("https://github.com/Owner/App.git"), "owner/app", true},
		{"fork checkout with the upstream as a second remote",
			remotes("git@github.com:me/app.git", "https://github.com/owner/app.git"), "owner/app", true},
		{"prefix of a longer name does not match",
			remotes("https://github.com/owner/application.git"), "owner/app", false},
		{"same name under another owner does not match",
			remotes("https://github.com/other/app.git"), "owner/app", false},
		{"no remotes", "", "owner/app", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteMatchesRepo(tc.remotes, tc.repo); got != tc.want {
				t.Errorf("remoteMatchesRepo(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}

// The queue exists to serialize ONE thing: CodeRabbit's account-wide review
// limit. A round that will never spend that quota is not a queue citizen, so
// neither an account block nor another PR holding the fire slot may delay it.
//
// `crq loop` learned this; `crq next` did not, and inherited the starvation the
// dogfood exposed — summary-only rounds sitting behind blocked rounds they would
// never spend quota on. Staged at its worst here: another PR HOLDS the slot, so
// Pump short-circuits on the slot holder and never reaches the FIFO or its
// bounded rescue scan at all.
func TestNextResolvesSummaryOnlyWithoutTheQueue(t *testing.T) {
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", dialect.CodexBotLogin}
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	cfg.FeedbackBots = cfg.RequiredBots
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()

	frontPull := ghapi.Pull{State: "open"}
	frontPull.Head.SHA = "1111111111111111"
	gh.pulls[fakeKey("o/front", 10)] = frontPull

	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = "2222222222222222"
	gh.pulls[fakeKey("o/private", 20)] = pull
	walkthrough := ghapi.IssueComment{
		ID:        900,
		Body:      corpusMessage(t, "coderabbit/summary-only-free-plan.md"),
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
	}
	walkthrough.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/private", 20)] = []ghapi.IssueComment{walkthrough}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }
	svc.localWorkFn = func(context.Context, string) (bool, string) { return false, "" }

	seedRound(t, store, cfg, "o/front", 10, "111111111", PhaseFired, now.Add(-time.Minute), 500)
	blockedUntil := now.Add(45 * time.Minute)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		st.FireSlot = &FireSlot{Key: QueueKey("o/front", 10), Token: "seedtok", Since: now.Add(-time.Minute)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Next(context.Background(), "o/private", 20); err != nil {
		t.Fatalf("next: %v", err)
	}

	st, _, _ := store.Load(context.Background())
	if r := st.Round("o/private", 20); r == nil || r.Phase == PhaseQueued {
		t.Fatalf("the summary-only round must leave the queue, got %#v", r)
	}
	for _, p := range gh.posted {
		if strings.Contains(p, cfg.ReviewCommand) {
			t.Fatalf("summary-only must never post the CodeRabbit command, posted=%v", gh.posted)
		}
	}
	if st.FireSlot == nil || st.FireSlot.Key != QueueKey("o/front", 10) {
		t.Fatalf("the front PR must keep the fire slot, got %#v", st.FireSlot)
	}
}
