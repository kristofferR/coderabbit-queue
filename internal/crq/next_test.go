package crq

import (
	"context"
	"encoding/json"
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

// A finding with no review thread cannot be resolved or declined — GitHub offers
// nothing to act on — so drain-first blocks every future round on it. The
// observed end state was a PR reporting "no review was ever requested" for its
// current head, four rounds running. `crq dismiss` is the only way out, and this
// pins both halves: that the deadlock is real, and that dismissing ends it.
func TestDismissEndsTheUnresolvableFindingDeadlock(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := newReplayFixture(t, base)
	repo, pr := "owner/repo", 505
	head := "aaaaaaaa1"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Minute))
	f.setLocalWork(false, "")

	f.next(repo, pr)

	// The review lands as a BODY finding: no inline comment, so no thread.
	f.clk.advance(2 * time.Minute)
	f.gh.mu.Lock()
	r := ghapi.Review{ID: 900, CommitID: head, State: "COMMENTED", SubmittedAt: f.clk.now(),
		Body: corpusMessage(t, "coderabbit/findings-outside-diff.md")}
	r.User.Login = f.bot
	key := fakeKey(repo, pr)
	f.gh.reviews[key] = append(f.gh.reviews[key], r)
	f.gh.mu.Unlock()

	report := f.next(repo, pr)
	f.wantAction(report, engine.ActionFix)
	if len(report.Findings) == 0 {
		t.Fatal("the body finding must be reported before it can be dismissed")
	}
	var id string
	for _, finding := range report.Findings {
		if finding.ThreadID == "" {
			id = finding.ID
			break
		}
	}
	if id == "" {
		t.Fatal("this test is only meaningful for a finding with no thread")
	}

	// It repeats forever: nothing the caller can do clears it.
	f.clk.advance(2 * time.Minute)
	f.wantAction(f.next(repo, pr), engine.ActionFix)
	posted := f.reviewsPosted(repo, pr)

	if _, err := f.svc.Dismiss(f.ctx, repo, pr, []string{id}, "already handled in an earlier commit"); err != nil {
		t.Fatal(err)
	}
	// Dismissing is a record, not a review: it must post nothing of its own,
	// even though it enqueues so the decision has a round to live on.
	if got := f.reviewsPosted(repo, pr); got != posted {
		t.Fatalf("dismissing posted a review: %d -> %d", posted, got)
	}

	f.clk.advance(2 * time.Minute)
	after := f.next(repo, pr)
	if after.Action == string(engine.ActionFix) {
		t.Fatalf("a dismissed finding must stop blocking the round, got %+v", after)
	}
	if after.Dismissed != 1 {
		t.Errorf("dismissed = %d, want 1 — the count is how a caller sees it was set aside, not lost", after.Dismissed)
	}
	for _, finding := range after.Findings {
		if finding.ID == id {
			t.Error("a dismissed finding must be withheld from findings")
		}
	}
	// Repeating a successful dismissal must succeed: Feedback has already
	// filtered that ID out, so validating against the current findings alone
	// would fail an interrupted agent on its own earlier success.
	if res, err := f.svc.Dismiss(f.ctx, repo, pr, []string{id}, "already handled in an earlier commit"); err != nil {
		t.Errorf("repeating a dismissal must be idempotent, got %v", err)
	} else if len(res.Already) != 1 {
		t.Errorf("result = %+v, want the id reported as already dismissed", res)
	}

	// A threaded finding is refused: resolve and decline both put the decision on
	// the PR where the bot can answer it, and dismissing one would converge the
	// round with its thread still open.
	f.gh.graphQL = func(query string, _ map[string]any, out any) error {
		if !strings.Contains(query, "reviewThreads") {
			return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"timelineItems":{"nodes":[]}}}}`), out)
		}
		return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"reviewThreads":{
		  "pageInfo":{"hasNextPage":false,"endCursor":""},
		  "nodes":[{"id":"PRRT_open","isResolved":false,"isOutdated":false,"path":"internal/state/state.go","line":42,
		    "comments":{"totalCount":1,"nodes":[{"databaseId":902,"body":"_⚠️ Potential issue_\n\nThis dereferences a nil round.",
		      "path":"internal/state/state.go","line":42,"author":{"login":"coderabbitai[bot]"},"commit":{"oid":"aaaaaaaa1"}}]}}]
		}}}}`), out)
	}
	threaded := f.next(repo, pr)
	var threadedID string
	for _, finding := range threaded.Findings {
		if finding.ThreadID != "" {
			threadedID = finding.ID
			break
		}
	}
	if threadedID == "" {
		t.Fatal("expected a threaded finding to try to dismiss")
	}
	if _, err := f.svc.Dismiss(f.ctx, repo, pr, []string{threadedID}, "not doing this one"); err == nil {
		t.Error("dismissing a finding that HAS a thread must be refused")
	}

	// So is an ID that is not a finding here at all — a stale copy-paste would
	// otherwise record a dismissal that silences whatever later matches it.
	if _, err := f.svc.Dismiss(f.ctx, repo, pr, []string{"deadbeef"}, "typo"); err == nil {
		t.Error("dismissing an unknown finding id must be refused")
	}

	// And a round that has moved on is refused rather than superseded back: the
	// head advanced after the findings were read, so this decision is about a
	// commit nobody is looking at, and superseding would archive the live round.
	if _, _, err := f.svc.recordDismissal(f.ctx, repo, pr, "999999999", []string{id}, "stale", true); err == nil {
		t.Error("recording a dismissal against a stale head must be refused, not superseded")
	}

	// It is scoped to this head. A push supersedes the round, and the next
	// reviewer reporting the same thing must be heard again.
	f.clk.advance(time.Minute)
	f.setHead(repo, pr, "bbbbbbbb2")
	f.setCommitDate("bbbbbbbb2", f.clk.now())
	f.next(repo, pr)
	st, _, err := f.store.Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if round := st.Round(repo, pr); round == nil || round.IsDismissed(id) {
		t.Errorf("the dismissal must not outlive the head it was made for, got %#v", round)
	}
}
