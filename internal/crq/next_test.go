package crq

import (
	"context"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
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

	// 3. The fix landed on a new head. The old finding has no thread to resolve,
	//    so it stops blocking, but the reviewer has not seen the new head — crq
	//    must say HOLD (or wait), never push. This is the rule agents break;
	//    here it is a value, not a paragraph.
	f.setLocalWork(true, "uncommitted changes in the working tree")
	f.clk.advance(time.Minute)
	f.setHead(repo, pr, "bbbbbbbb2")
	f.setCommitDate("bbbbbbbb2", f.clk.now())
	held := f.next(repo, pr)
	if held.Action != string(engine.ActionHold) && held.Action != string(engine.ActionWait) {
		t.Fatalf("action = %q (%s), want hold or wait while the new head is unreviewed", held.Action, held.Reason)
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
