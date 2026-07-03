package crq

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLooksLikePathAcceptsRootFiles(t *testing.T) {
	for _, p := range []string{"Dockerfile", "Makefile", "LICENSE", "go.mod", "src/app.go", "a/b/c"} {
		if !looksLikePath(p) {
			t.Fatalf("expected %q to be treated as a path", p)
		}
	}
	for _, p := range []string{"", "Additional comments", "🧹 Nitpick comments", "two words"} {
		if looksLikePath(p) {
			t.Fatalf("expected %q NOT to be a path", p)
		}
	}
}

func TestDedupeSuppressesResolvedThreadPromptDuplicate(t *testing.T) {
	findings := []Finding{
		{Bot: "coderabbitai", Path: "internal/x.go", Line: 10, Title: "dup", Body: "do x", Source: "review_prompt"},
	}
	suppress := map[string]bool{"coderabbitai|internal/x.go|10": true}
	if got := dedupeFindings(findings, suppress); len(got) != 0 {
		t.Fatalf("expected the prompt duplicate at a resolved-thread location to be suppressed, got %#v", got)
	}
	if got := dedupeFindings(findings, nil); len(got) != 1 {
		t.Fatalf("expected the prompt finding to survive when not suppressed, got %#v", got)
	}
}

func TestFeedbackBoundsIssueCommentsToHead(t *testing.T) {
	cfg := Config{
		Bot:             "coderabbitai[bot]",
		RequiredBots:    []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		RateLimitMarker: "rate limited by coderabbit.ai",
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC()
	sha := "abcdef1234567890"
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 3)] = pull
	gc := gitCommit{SHA: sha}
	gc.Committer.Date = headTime
	gh.commits[sha] = gc
	mkc := func(id int64, body string, at time.Time) IssueComment {
		ic := IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		ic.User.Login = "chatgpt-codex-connector[bot]"
		return ic
	}
	gh.comments[fakeKey("o/repo", 3)] = []IssueComment{
		mkc(1, "Stale finding from the previous head", headTime.Add(-time.Hour)),
		mkc(2, "Current finding for this head", headTime.Add(time.Minute)),
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	rep, err := svc.Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || !strings.Contains(rep.Findings[0].Body, "Current finding") {
		t.Fatalf("expected only the post-head issue comment as a finding, got %#v", rep.Findings)
	}
}

func TestFeedbackSurfacesCodexEvenWhenNotRequired(t *testing.T) {
	// Regression: Codex (chatgpt-codex-connector) reviews a PR and posts inline
	// findings, but it isn't in RequiredBots (which is CodeRabbit-only by default).
	// crq must still surface Codex's findings — and must NOT falsely converge just
	// because CodeRabbit reviewed clean — while not waiting on Codex to converge.
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, extraFeedbackBots),
	}
	gh := newFakeGitHub()
	sha := "abcdef1234567890"
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 7)] = pull

	// CodeRabbit reviewed this head and found nothing (empty body → no findings).
	crReview := Review{ID: 1, Body: "", CommitID: sha, SubmittedAt: time.Now().UTC()}
	crReview.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 7)] = []Review{crReview}

	// Codex left an inline finding on the same head (REST review-comment path, since
	// the fake GraphQL is unavailable and Feedback falls back to it).
	cx := ReviewComment{ID: 22, Body: "**Fix the off-by-one.** This clips the last row.", Path: "app/x.go", Line: 10, CommitID: sha}
	cx.User.Login = "chatgpt-codex-connector[bot]"
	gh.reviewComments[fakeKey("o/repo", 7)] = []ReviewComment{cx}

	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	rep, err := svc.Feedback(context.Background(), "o/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || !strings.Contains(rep.Findings[0].Body, "off-by-one") {
		t.Fatalf("expected the Codex finding to be surfaced, got %#v", rep.Findings)
	}
	if normalizeBotName(rep.Findings[0].Bot) != "chatgpt-codex-connector" {
		t.Fatalf("expected the finding attributed to Codex, got %q", rep.Findings[0].Bot)
	}
	if rep.Converged {
		t.Fatal("must not converge while a Codex finding is open")
	}
	// Convergence gate stays CodeRabbit-only: Codex must not be tracked in ReviewedBy
	// (otherwise crq would hang on repos where Codex never reviews).
	if _, tracked := rep.ReviewedBy["chatgpt-codex-connector[bot]"]; tracked {
		t.Fatalf("Codex must not gate convergence, ReviewedBy=%#v", rep.ReviewedBy)
	}
	if reviewed, ok := rep.ReviewedBy["coderabbitai[bot]"]; !ok || !reviewed {
		t.Fatalf("CodeRabbit should be marked reviewed, ReviewedBy=%#v", rep.ReviewedBy)
	}
}

func TestParseReviewBodyFindingsExtractsOutsideDiffItems(t *testing.T) {
	review := Review{
		ID: 99,
		Body: `> [!CAUTION]
> Some comments are outside the diff and can't be posted inline.
>
> <details>
> <summary>Outside diff range comments (1)</summary><blockquote>
>
> <details>
> <summary>internal/foo.go (1)</summary><blockquote>
>
> ` + "`42-43`: _Functional Correctness_ | _🟠 Major_ | _⚡ Quick win_" + `
>
> **Fix the cancellation path.**
>
> The operation can continue after cancellation.
>
> <!-- cr-comment:v1:abcdef -->
>
> </blockquote></details>
> </blockquote></details>`,
		CommitID:    "abcdef1234567890",
		SubmittedAt: time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %#v", len(findings), findings)
	}
	if findings[0].Path != "internal/foo.go" || findings[0].Line != 42 || findings[0].Severity != "major" {
		t.Fatalf("first finding mismatch: %#v", findings[0])
	}
	if findings[0].Title != "Fix the cancellation path." {
		t.Fatalf("title mismatch: %#v", findings[0])
	}
}

func TestParseReviewBodyFindingsExtractsCommentsFailedToPost(t *testing.T) {
	// CodeRabbit's "Comments failed to post" section uses un-backticked line
	// headers (561-573:) unlike the backticked "Outside diff range" form.
	review := Review{
		ID: 7,
		Body: "<details>\n<summary>🛑 Comments failed to post (1)</summary><blockquote>\n\n" +
			"<details>\n<summary>src-tauri/inject/messenger.js (1)</summary><blockquote>\n\n" +
			"561-573: _📐 Maintainability & Code Quality_ | _🟠 Major_ | _⚡ Quick win_\n\n" +
			"**Move the hide-names toggle out of `messenger.js` or update the allowlist first.**\n\n" +
			"This adds a new injection-layer responsibility outside the documented scope.\n\n" +
			"</blockquote></details>\n\n</blockquote></details>",
		CommitID:    "165f71e41",
		SubmittedAt: time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %#v", len(findings), findings)
	}
	f := findings[0]
	if f.Path != "src-tauri/inject/messenger.js" || f.Line != 561 || f.Severity != "major" {
		t.Fatalf("finding mismatch: %#v", f)
	}
	if f.Title != "Move the hide-names toggle out of `messenger.js` or update the allowlist first." {
		t.Fatalf("title mismatch: %q", f.Title)
	}
}

func TestParseReviewBodyFindingsExtractsPromptBlock(t *testing.T) {
	review := Review{
		ID: 100,
		Body: `<details>
<summary>🤖 Prompt for all review comments with AI agents</summary>

` + "```" + `
Verify each finding against current code.

Inline comments:
In ` + "`@src/app.ts`" + `:
- Around line 12-14: The parser accepts stale state. Re-read the latest state
  before writing so concurrent updates are not lost.

Outside diff comments:
In @README.md:
- Line 7: Add the missing install warning.
` + "```" + `
</details>`,
		CommitID:    "abcdef1234567890",
		SubmittedAt: time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %#v", len(findings), findings)
	}
	if findings[0].Path != "src/app.ts" || findings[0].Line != 12 || findings[0].Source != "review_prompt" {
		t.Fatalf("first prompt finding mismatch: %#v", findings[0])
	}
	if findings[1].Path != "README.md" || findings[1].Line != 7 {
		t.Fatalf("second prompt finding mismatch: %#v", findings[1])
	}
}

func TestThreadFindingsSurfacesUnresolvedAcrossCommits(t *testing.T) {
	bots := botSet([]string{"coderabbitai[bot]"})
	mk := func(resolved, outdated bool, oid string) reviewThread {
		var th reviewThread
		th.ID = "PRRT_x"
		th.IsResolved = resolved
		th.IsOutdated = outdated
		th.Path = "internal/foo.go"
		th.Line = 42
		c := struct {
			DatabaseID   int64     `json:"databaseId"`
			Body         string    `json:"body"`
			URL          string    `json:"url"`
			Path         string    `json:"path"`
			Line         int       `json:"line"`
			OriginalLine int       `json:"originalLine"`
			CreatedAt    time.Time `json:"createdAt"`
			Author       struct {
				Login string `json:"login"`
			} `json:"author"`
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
			OriginalCommit struct {
				OID string `json:"oid"`
			} `json:"originalCommit"`
		}{Body: "**Potential issue** still unfixed.", Line: 42}
		c.Author.Login = "coderabbitai[bot]"
		c.Commit.OID = oid
		th.Comments.Nodes = append(th.Comments.Nodes, c)
		return th
	}

	// Unresolved + not outdated, filed on an OLD commit (!= current HEAD): still surfaced.
	got := threadFindings(mk(false, false, "0000oldcommit"), bots)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding for unresolved old-commit thread, got %d", len(got))
	}
	if got[0].ThreadID != "PRRT_x" || got[0].Line != 42 {
		t.Fatalf("finding mismatch: %#v", got[0])
	}

	// Resolved and outdated threads are skipped regardless of commit.
	if got := threadFindings(mk(true, false, "0000oldcommit"), bots); len(got) != 0 {
		t.Fatalf("expected resolved thread to be skipped, got %d", len(got))
	}
	if got := threadFindings(mk(false, true, "0000oldcommit"), bots); len(got) != 0 {
		t.Fatalf("expected outdated thread to be skipped, got %d", len(got))
	}
}

func TestInBotsToleratesBotSuffix(t *testing.T) {
	bots := botSet([]string{"coderabbitai[bot]", "chatgpt-codex"})
	// REST reports "coderabbitai[bot]"; GraphQL review threads report "coderabbitai".
	for _, login := range []string{"coderabbitai[bot]", "coderabbitai", "chatgpt-codex", "chatgpt-codex[bot]"} {
		if !inBots(bots, login) {
			t.Fatalf("expected %q to match a configured bot", login)
		}
	}
	if inBots(bots, "some-human") {
		t.Fatal("unexpected match for a non-bot login")
	}
}

func TestMarkReviewedFlipsConfiguredKeyAcrossSuffix(t *testing.T) {
	// A GraphQL login without the [bot] suffix must flip the configured suffixed
	// key in place, not insert a divergent key that would leave convergence (which
	// ANDs every key) permanently false.
	reviewedBy := map[string]bool{"coderabbitai[bot]": false, "chatgpt-codex": false}
	markReviewed(reviewedBy, "coderabbitai")
	if !reviewedBy["coderabbitai[bot]"] || len(reviewedBy) != 2 {
		t.Fatalf("expected the configured key flipped without inserting a new one: %#v", reviewedBy)
	}
	markReviewed(reviewedBy, "chatgpt-codex") // exact match
	if !reviewedBy["chatgpt-codex"] {
		t.Fatalf("exact match failed: %#v", reviewedBy)
	}
	// Configured key without suffix, REST login with suffix — the inverse case.
	rb := map[string]bool{"coderabbitai": false}
	markReviewed(rb, "coderabbitai[bot]")
	if !rb["coderabbitai"] || len(rb) != 1 {
		t.Fatalf("a suffixed REST login should flip the suffix-less key: %#v", rb)
	}
	// An unknown login is a no-op: no panic, no spurious insert.
	rb2 := map[string]bool{"coderabbitai[bot]": false}
	markReviewed(rb2, "some-human")
	if rb2["coderabbitai[bot]"] || len(rb2) != 1 {
		t.Fatalf("unknown login must be a no-op: %#v", rb2)
	}
}

func TestFeedbackSkipsConfiguredBotIssueCommentsAcrossSuffix(t *testing.T) {
	cfg := Config{Bot: "coderabbitai", RequiredBots: []string{"coderabbitai"}}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := IssueComment{Body: "CodeRabbit summary text", UpdatedAt: time.Now().UTC()}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	report, err := svc.Feedback(context.Background(), "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("configured bot summary issue comments should be skipped across suffix forms: %#v", report.Findings)
	}
}

func TestDedupeFindingsDropsNonActionableBotArtifacts(t *testing.T) {
	findings := []Finding{
		{Bot: "coderabbitai", Title: "> Skipped: comment is from another GitHub bot.", Body: "> Skipped: comment is from another GitHub bot.", Source: "review_thread"},
		{Bot: "chatgpt-codex-connector[bot]", Title: "You have reached your Codex usage limits for code reviews.", Body: "You have reached your Codex usage limits for code reviews.", Source: "issue_comment"},
	}
	if got := dedupeFindings(findings, nil); len(got) != 0 {
		t.Fatalf("expected non-actionable bot artifacts to be dropped, got %#v", got)
	}
}

func TestThreadFindingsMatchesGraphQLBotLogin(t *testing.T) {
	bots := botSet([]string{"coderabbitai[bot]"})
	var th reviewThread
	th.ID = "PRRT_z"
	th.Path = "internal/crq/foo.go"
	th.Line = 7
	c := th.Comments.Nodes[:0:0]
	_ = c
	node := struct {
		DatabaseID   int64     `json:"databaseId"`
		Body         string    `json:"body"`
		URL          string    `json:"url"`
		Path         string    `json:"path"`
		Line         int       `json:"line"`
		OriginalLine int       `json:"originalLine"`
		CreatedAt    time.Time `json:"createdAt"`
		Author       struct {
			Login string `json:"login"`
		} `json:"author"`
		Commit struct {
			OID string `json:"oid"`
		} `json:"commit"`
		OriginalCommit struct {
			OID string `json:"oid"`
		} `json:"originalCommit"`
	}{Body: "**Potential issue** fix this", Line: 7}
	node.Author.Login = "coderabbitai" // GraphQL form, no [bot]
	th.Comments.Nodes = append(th.Comments.Nodes, node)

	got := threadFindings(th, bots)
	if len(got) != 1 || got[0].ThreadID != "PRRT_z" {
		t.Fatalf("expected the GraphQL-login thread to surface with its thread_id, got %#v", got)
	}
}

func TestRateLimitDetectionCoversFairUsageFormat(t *testing.T) {
	cfg := Config{RateLimitMarker: "rate limited by coderabbit.ai"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	newMsg := "<!-- This is an auto-generated reply by CodeRabbit -->\n" +
		"You're currently rate limited under our Fair Usage Limits Policy. " +
		"Your next review will be available in 48 minutes."
	oldMsg := "You are rate limited by coderabbit.ai. Reviews available in 3 minutes."

	if !svc.isRateLimited(newMsg) {
		t.Fatal("must detect CodeRabbit's Fair Usage rate-limit message")
	}
	if !svc.isRateLimited(oldMsg) {
		t.Fatal("must still detect the legacy marker")
	}
	if svc.isRateLimited("LGTM — nice fix, nothing about limits here") {
		t.Fatal("must not flag a normal review comment")
	}

	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	if reset := parseAvailableIn(newMsg, base); reset == nil || !reset.Equal(base.Add(48*time.Minute)) {
		t.Fatalf("expected reset base+48m from the new message, got %v", reset)
	}
}

func TestParseAvailableIn(t *testing.T) {
	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	reset := parseAvailableIn("Review limit reached. Reviews available in 1 hour 2 minutes 3 seconds.", base)
	if reset == nil {
		t.Fatal("expected reset")
	}
	want := base.Add(time.Hour + 2*time.Minute + 3*time.Second)
	if !reset.Equal(want) {
		t.Fatalf("reset mismatch: got %s want %s", reset, want)
	}
}

func TestParseQuota(t *testing.T) {
	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	remaining, reset := parseQuota("0 reviews remaining. Reviews available in 3 minutes.", base)
	if remaining == nil || *remaining != 0 {
		t.Fatalf("remaining mismatch: %#v", remaining)
	}
	if reset == nil || !reset.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("reset mismatch: %#v", reset)
	}
}

func TestExtendDeadlineForBlock(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	budget := 20 * time.Minute
	base := now.Add(budget) // original deadline: now + 20m

	// Not blocked → deadline unchanged.
	if got := extendDeadlineForBlock(base, nil, now, budget); !got.Equal(base) {
		t.Fatalf("nil block: want %v, got %v", base, got)
	}
	// Block already elapsed → deadline unchanged.
	past := now.Add(-time.Minute)
	if got := extendDeadlineForBlock(base, &past, now, budget); !got.Equal(base) {
		t.Fatalf("past block: want %v, got %v", base, got)
	}
	// Block extends past the current deadline → push to blockedUntil + budget, so a
	// full review wait remains after the rate-limit window clears.
	until := now.Add(60 * time.Minute)
	if want, got := until.Add(budget), extendDeadlineForBlock(base, &until, now, budget); !got.Equal(want) {
		t.Fatalf("future block: want %v, got %v", want, got)
	}
	// A nearer block must never pull an already-extended deadline earlier.
	later := now.Add(80 * time.Minute)
	near := now.Add(5 * time.Minute)
	if got := extendDeadlineForBlock(later, &near, now, budget); !got.Equal(later) {
		t.Fatalf("must not shrink: want %v, got %v", later, got)
	}
}

func TestBlockedPollInterval(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	base := 15 * time.Second

	// Block far out → capped, so the loop still re-checks periodically (not 15s).
	if got := blockedPollInterval(now.Add(30*time.Minute), now, base); got != 5*time.Minute {
		t.Fatalf("far block: want 5m, got %v", got)
	}
	// Block a couple minutes out → wait until just past it.
	if got := blockedPollInterval(now.Add(2*time.Minute), now, base); got != 2*time.Minute+time.Second {
		t.Fatalf("near block: want 2m1s, got %v", got)
	}
	// Block about to clear (within base) → never poll faster than the base interval.
	if got := blockedPollInterval(now.Add(3*time.Second), now, base); got != base {
		t.Fatalf("imminent block: want %v, got %v", base, got)
	}
}

func TestLoopReportsClosedPRSkip(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Millisecond,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "closed"
	pull.Merged = true
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || report.Status != "skipped" || report.Reason != "pr closed" {
		t.Fatalf("closed PR should be a terminal skipped report, code=%d report=%#v", code, report)
	}
}

func TestLoopRequiresAllRequiredBotsAfterDedupe(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		PollInterval:        time.Nanosecond,
		FeedbackWaitTimeout: time.Nanosecond,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	review := Review{CommitID: "abcdef1234567890"}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fired[QueueKey("owner/repo", 12)] = "abcdef123"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatalf("deduped CodeRabbit review must not succeed before every required bot reviews: %#v", report)
	}
	if report.Status != "timeout" || report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("expected timeout waiting for the missing required bot, code=%d report=%#v", code, report)
	}
}
