package crq

import (
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// End-to-end scenarios for Codex firing, auto-review detection, and the
// dynamic completion gate. They reuse the replay fixture (injected clock, fake
// GitHub, MemoryStore) so every claim is driven through the real
// Pump/Feedback/observe pipeline.

const codexLogin = "chatgpt-codex-connector[bot]"

// codexCleanTada is the Codex clean-summary shape pinned by
// testdata/codex/clean-summary-tada.md, parameterized on the reviewed SHA.
func codexClean(sha string) string {
	return "Codex Review: Didn't find any major issues. :tada:\n\n**Reviewed commit:** `" + sha + "`"
}

const codexUsageLimit = "You have reached your Codex usage limits for code reviews. Limits reset periodically."

func newCodexReplayFixture(t *testing.T, base time.Time, mutate func(*Config)) *replayFixture {
	t.Helper()
	clk := newReplayClock(base)
	cfg := replayConfig()
	cfg.CodexCommand = "@codex review"
	if mutate != nil {
		mutate(&cfg)
	}
	gh := newFakeGitHub()
	gh.now = clk.now
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = clk.now
	return &replayFixture{t: t, ctx: t.Context(), clk: clk, gh: gh, store: store, svc: svc, cfg: cfg, bot: cfg.Bot}
}

// codexComment appends an issue comment authored by the Codex app.
func (f *replayFixture) codexComment(repo string, pr int, id int64, body string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}
	c.User.Login = codexLogin
	key := fakeKey(repo, pr)
	f.gh.comments[key] = append(f.gh.comments[key], c)
}

// codexReview appends a submitted Codex review.
func (f *replayFixture) codexReview(repo string, pr int, id int64, commitSHA string, at time.Time) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	r := ghapi.Review{ID: id, CommitID: commitSHA, State: "COMMENTED", SubmittedAt: at.UTC()}
	r.User.Login = codexLogin
	key := fakeKey(repo, pr)
	f.gh.reviews[key] = append(f.gh.reviews[key], r)
}

// codexPosted counts how many `@codex review` commands crq actually posted.
func (f *replayFixture) codexPosted(repo string, pr int) int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	want := QueueKey(repo, pr) + ":" + f.cfg.CodexCommand
	n := 0
	for _, p := range f.gh.posted {
		if p == want {
			n++
		}
	}
	return n
}

// (i) Codex configured-required with no auto-review: crq posts the Codex
// command exactly once alongside the CodeRabbit command, does NOT repost it on
// the post-rate-limit retry of the same head, and completes the round only
// when BOTH reviews are in.
func TestCodexReplayRequiredFiresOnceAndGatesCompletion(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr, head := "o/r", 7, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	if got := f.reviewsPosted(repo, pr); got != 1 {
		t.Fatalf("coderabbit commands = %d, want 1", got)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("codex commands = %d, want 1", got)
	}
	if r := f.round(repo, pr); r == nil || r.CodexCommandID == 0 {
		t.Fatalf("round must record the codex command, got %+v", r)
	}

	// CodeRabbit answers rate-limited; the round parks. After the window one
	// retry fires — but the Codex command is NOT reposted (CodexCommandID set).
	f.botComment(repo, pr, 9001, replayFairUsage(t, 10), f.clk.now().Add(5*time.Second))
	if res := f.pump(); res.Action != "requeued" {
		t.Fatalf("expected requeue, got %+v", res)
	}
	f.clk.advance(11 * time.Minute)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected retry fire, got %+v", res)
	}
	if got := f.reviewsPosted(repo, pr); got != 2 {
		t.Fatalf("coderabbit commands after retry = %d, want 2", got)
	}
	if got := f.codexPosted(repo, pr); got != 1 {
		t.Fatalf("codex must not be reposted on retry, got %d", got)
	}

	// CodeRabbit's review lands: with Codex still outstanding the round must
	// stay open (reviewing), not complete.
	f.botReview(repo, pr, 501, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("round must wait for codex, got %+v", r)
	}

	// Codex's review lands too — now the round completes.
	f.codexReview(repo, pr, 502, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("round must complete after both reviews, got %+v", r)
	}
}

// (ii) Codex configured-required but auto-review is active (a Codex review
// exists that no `@codex review` command preceded): crq must never post the
// Codex command; the round still gates on Codex's own review.
func TestCodexReplayAutoActiveSuppressesCommand(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, func(cfg *Config) {
		cfg.RequiredBots = []string{cfg.Bot, codexLogin}
	})
	repo, pr := "o/r", 8
	oldHead, head := "1111222233334444", "5555666677778888"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))
	// History: Codex reviewed the previous head unprompted → auto-review is on.
	f.codexReview(repo, pr, 400, oldHead, base.Add(-2*time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("auto-active codex must never be commanded, got %d posts", got)
	}

	// CodeRabbit finishes; the round still waits for Codex's auto review.
	f.botReview(repo, pr, 501, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("round must wait for codex auto review, got %+v", r)
	}
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("waiting must not trigger a codex post, got %d", got)
	}

	// Codex auto-reviews the head (clean) — round completes.
	f.codexComment(repo, pr, 700, codexClean(head[:10]), f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("round must complete on codex's auto review, got %+v", r)
	}
}

// (iii) Codex NOT required: when it joins a round on its own (actionable
// comment mid-round), the dynamic gate holds completion until its review — and
// a usage-limit notice disengages the dynamic gate so the round can complete
// on CodeRabbit alone.
func TestCodexReplayDynamicGateAndUsageLimitEscape(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	f := newCodexReplayFixture(t, base, nil) // RequiredBots = coderabbit only
	repo, pr, head := "o/r", 9, "aaaabbbbccccdddd"
	f.openPull(repo, pr, head)
	f.setCommitDate(head, base.Add(-time.Hour))

	f.enqueue(repo, pr)
	if res := f.pump(); res.Action != "fired" {
		t.Fatalf("expected fire, got %+v", res)
	}
	// Not required + not active → no codex command.
	if got := f.codexPosted(repo, pr); got != 0 {
		t.Fatalf("unrequired codex must not be commanded, got %d", got)
	}

	// Codex joins the round with an actionable comment; CodeRabbit finishes.
	// The dynamic gate must keep the round open for Codex.
	f.codexComment(repo, pr, 700, "There is a bug in `foo.go` line 3: the cutoff is inverted.", f.clk.now().Add(30*time.Second))
	f.botReview(repo, pr, 501, head, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseReviewing {
		t.Fatalf("dynamic gate must hold for codex, got %+v", r)
	}
	report, err := f.svc.Feedback(f.ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed, tracked := report.ReviewedBy[codexLogin]; !tracked || reviewed {
		t.Fatalf("dynamically-gated codex must appear pending in ReviewedBy: %+v", report.ReviewedBy)
	}

	// Codex runs out of quota: the dynamic gate disengages and the round
	// completes on CodeRabbit's review alone.
	f.codexComment(repo, pr, 701, codexUsageLimit, f.clk.now().Add(time.Minute))
	f.clk.advance(2 * time.Minute)
	f.pump()
	if r := f.round(repo, pr); r == nil || r.Phase != PhaseCompleted {
		t.Fatalf("usage limit must release the dynamic gate, got %+v", r)
	}
}
