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

func TestFeedbackCountsCompletionReplyForFiredHead(t *testing.T) {
	// A re-review with nothing new to say produces no review object: CodeRabbit
	// only replies "Review finished" to the command. That reply must satisfy
	// ReviewedBy when crq state proves the command was fired for this head —
	// otherwise the loop times out on a complete round. It only counts when the
	// bot has actually reviewed the PR before: on a never-reviewed PR the same
	// ack can arrive seconds after the trigger while the real review is still
	// queued on CodeRabbit's side.
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	completion := "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished."
	setup := func(replyAt time.Time, replyBody string, seedHistory, priorReview bool) *Service {
		gh := newFakeGitHub()
		var pull Pull
		pull.State = "open"
		pull.Head.SHA = sha
		gh.pulls[fakeKey("o/repo", 3)] = pull
		trigger := IssueComment{ID: 1, Body: "@coderabbitai review", CreatedAt: firedAt, UpdatedAt: firedAt}
		trigger.User.Login = "kristofferR"
		reply := IssueComment{ID: 2, Body: replyBody, CreatedAt: replyAt, UpdatedAt: replyAt}
		reply.User.Login = "coderabbitai[bot]"
		gh.comments[fakeKey("o/repo", 3)] = []IssueComment{trigger, reply}
		if priorReview {
			// A no-findings re-review presupposes an earlier review of the PR
			// (on some older commit); without one the completion reply must
			// not stand in for a review (covered by the dedicated case below).
			prior := Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour)}
			prior.User.Login = "coderabbitai[bot]"
			gh.reviews[fakeKey("o/repo", 3)] = []Review{prior}
		}
		store := NewMemoryStore(cfg)
		if seedHistory {
			if _, err := store.Update(context.Background(), func(st *State) error {
				st.History = append(st.History, HistoryItem{Repo: "o/repo", PR: 3, Commit: head, At: firedAt, Host: "testhost"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		return NewService(cfg, gh, store, nil)
	}

	rep, err := setup(firedAt.Add(5*time.Second), completion, true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ReviewedBy["coderabbitai[bot]"] || !rep.Converged {
		t.Fatalf("a completion reply after the fired command must satisfy the configured bot, got %#v", rep)
	}

	// A reply older than the fire belongs to an earlier round.
	rep, err = setup(firedAt.Add(-time.Minute), completion, true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("a pre-fire reply must not satisfy the configured bot, got %#v", rep)
	}

	// Without a state anchor tying a fire to this head, issue comments never
	// satisfy ReviewedBy (the pre-existing rule).
	rep, err = setup(firedAt.Add(5*time.Second), completion, false, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("without a fired-head anchor the reply must not count, got %#v", rep)
	}

	// A rate-limit reply is not a completion.
	rep, err = setup(firedAt.Add(5*time.Second), "⚠️ rate limited by coderabbit.ai — please wait", true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("a rate-limit reply must not satisfy the configured bot, got %#v", rep)
	}

	// An acknowledgement/progress reply without the completion marker is not a
	// completion either — the review is still running.
	rep, err = setup(firedAt.Add(5*time.Second), "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nFull review triggered.", true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("an ack reply without the completion marker must not satisfy the configured bot, got %#v", rep)
	}

	// A completion reply on a PR the bot has never reviewed is CodeRabbit
	// racing its own scheduler (instant "Review finished" ack, real review
	// still to come) — it must not converge the round.
	rep, err = setup(firedAt.Add(5*time.Second), completion, true, false).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("a completion reply without any prior review must not satisfy the configured bot, got %#v", rep)
	}

	cfg.CompletionMarker = ""
	rep, err = setup(firedAt.Add(5*time.Second), completion, true, true).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("an empty completion marker must disable the completion fallback, got %#v", rep)
	}
}

func TestFeedbackRejectsCompletionReplyFromEarlierRound(t *testing.T) {
	// Overlapping rounds: an old command's review is still finishing when the
	// PR is pushed and a new command fires. The old round's "Review finished"
	// lands after the new firedAt, but pairs with the old command — it must
	// not converge the new round. The completion answering the new command
	// must.
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	completion := "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished."
	mk := func(id int64, login, body string, at time.Time) IssueComment {
		ic := IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		ic.User.Login = login
		return ic
	}
	setup := func(comments []IssueComment) *Service {
		gh := newFakeGitHub()
		var pull Pull
		pull.State = "open"
		pull.Head.SHA = sha
		gh.pulls[fakeKey("o/repo", 3)] = pull
		gh.comments[fakeKey("o/repo", 3)] = comments
		// The completion fallback requires a prior review by the bot (the old
		// round's review of the previous head).
		prior := Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour)}
		prior.User.Login = "coderabbitai[bot]"
		gh.reviews[fakeKey("o/repo", 3)] = []Review{prior}
		store := NewMemoryStore(cfg)
		if _, err := store.Update(context.Background(), func(st *State) error {
			st.History = append(st.History, HistoryItem{Repo: "o/repo", PR: 3, Commit: head, At: firedAt, Host: "testhost"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return NewService(cfg, gh, store, nil)
	}

	oldCmd := mk(1, "kristofferR", "@coderabbitai review", firedAt.Add(-5*time.Minute))
	newCmd := mk(2, "kristofferR", "@coderabbitai review", firedAt)
	oldDone := mk(3, "coderabbitai[bot]", completion, firedAt.Add(30*time.Second))

	rep, err := setup([]IssueComment{oldCmd, newCmd, oldDone}).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewedBy["coderabbitai[bot]"] || rep.Converged {
		t.Fatalf("the old round's completion must not converge the new round, got %#v", rep)
	}

	newDone := mk(4, "coderabbitai[bot]", completion, firedAt.Add(2*time.Minute))
	rep, err = setup([]IssueComment{oldCmd, newCmd, oldDone, newDone}).Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ReviewedBy["coderabbitai[bot]"] || !rep.Converged {
		t.Fatalf("the completion answering the new command must converge the round, got %#v", rep)
	}
}

func TestFeedbackSkipsReviewAnsweredCommandsWhenPairingCompletionReplies(t *testing.T) {
	cfg := Config{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		ReviewCommand:     "@coderabbitai review",
		RateLimitMarker:   "rate limited by coderabbit.ai",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		CompletionMarker:  "Review finished",
	}
	sha := "abcdef1234567890"
	head := sha[:9]
	firedAt := time.Now().UTC().Add(-10 * time.Minute)
	completion := "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished."
	mk := func(id int64, login, body string, at time.Time) IssueComment {
		ic := IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		ic.User.Login = login
		return ic
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey("o/repo", 3)] = pull
	gh.comments[fakeKey("o/repo", 3)] = []IssueComment{
		mk(1, "kristofferR", "@coderabbitai review", firedAt.Add(-5*time.Minute)),
		mk(2, "kristofferR", "@coderabbitai review", firedAt),
		mk(3, "coderabbitai[bot]", completion, firedAt.Add(30*time.Second)),
	}
	oldReview := Review{ID: 44, CommitID: "1111111111111111", SubmittedAt: firedAt.Add(-4 * time.Minute)}
	oldReview.User.Login = cfg.Bot
	gh.reviews[fakeKey("o/repo", 3)] = []Review{oldReview}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.History = append(st.History, HistoryItem{Repo: "o/repo", PR: 3, Commit: head, At: firedAt, Host: "testhost"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	rep, err := svc.Feedback(context.Background(), "o/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ReviewedBy["coderabbitai[bot]"] || !rep.Converged {
		t.Fatalf("the old review object should consume the old command so the completion pairs with the fired command, got %#v", rep)
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

func TestParseReviewBodyFindingsExtractsNestedQuoteSections(t *testing.T) {
	// CodeRabbit nests "Outside diff range" / "Duplicate" / nitpick sections
	// two-plus blockquote levels deep. A single-level quote strip leaves
	// "> " prefixes that break the anchored line-range header match, so
	// every finding in those sections used to be silently dropped.
	review := Review{
		ID: 100,
		Body: "> [!WARNING]\n" +
			"> Review had issues posting inline.\n" +
			">\n" +
			"> <details>\n" +
			"> <summary>Outside diff range comments (2)</summary><blockquote>\n" +
			">\n" +
			"> > <details>\n" +
			"> > <summary>internal/deep.go (1)</summary><blockquote>\n" +
			"> >\n" +
			"> > `10-12`: _Functional Correctness_ | _Major_\n" +
			"> >\n" +
			"> > **Nested finding one.**\n" +
			"> >\n" +
			"> > Body of the first nested finding.\n" +
			"> >\n" +
			"> > </blockquote></details>\n" +
			"> > <details>\n" +
			"> > <summary>internal/deeper.go (1)</summary><blockquote>\n" +
			"> >\n" +
			"> > > `20-21`: _Maintainability_ | _Minor_\n" +
			"> > >\n" +
			"> > > **Nested finding two.**\n" +
			"> > >\n" +
			"> > > Body of the second, even deeper finding.\n" +
			"> >\n" +
			"> > </blockquote></details>\n" +
			"> </blockquote></details>",
		CommitID:    "abcdef1234567890",
		SubmittedAt: time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC),
	}
	findings := parseReviewBodyFindings(review, "coderabbitai[bot]")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %#v", len(findings), findings)
	}
	if findings[0].Path != "internal/deep.go" || findings[0].Line != 10 || findings[0].Title != "Nested finding one." {
		t.Fatalf("first finding mismatch: %#v", findings[0])
	}
	if findings[1].Path != "internal/deeper.go" || findings[1].Line != 20 || findings[1].Title != "Nested finding two." {
		t.Fatalf("second finding mismatch: %#v", findings[1])
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

func TestFeedbackSurfacesBodyFindingsFromSupersededCommit(t *testing.T) {
	// Regression: CodeRabbit's inline comments failed to post (GitHub 5xx / code
	// review limits), so 2 findings exist ONLY in the review body's prompt block,
	// on a commit that is no longer the head (the branch was rebased / squash-
	// merged after the review). Gating body extraction to the head silently drops
	// the whole review; crq must still surface those findings since no newer
	// CodeRabbit review supersedes them.
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, extraFeedbackBots),
	}
	gh := newFakeGitHub()
	head := "9999999999999999"
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey("o/repo", 5)] = pull

	review := Review{
		ID: 7,
		Body: `**Actionable comments posted: 2**
<details>
<summary>🤖 Prompt for all review comments with AI agents</summary>

` + "```" + `
Verify each finding against current code.

Inline comments:
In ` + "`@src/app.ts`" + `:
- Around line 12-14: The parser accepts stale state. Re-read the latest state.
- Around line 40-42: Validate the HTTP status before decoding the body.
` + "```" + `
</details>`,
		// Reviewed an earlier commit; the head has since moved on.
		CommitID:    "1111111111111111",
		SubmittedAt: time.Now().UTC().Add(-time.Hour),
	}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []Review{review}

	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	rep, err := svc.Feedback(context.Background(), "o/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("expected 2 body findings from the superseded-commit review, got %d: %#v", len(rep.Findings), rep.Findings)
	}
	// The review never landed on the head, so convergence must still wait.
	if rep.Converged {
		t.Fatal("must not converge: CodeRabbit has not reviewed the current head")
	}
	if reviewed := rep.ReviewedBy["coderabbitai[bot]"]; reviewed {
		t.Fatalf("CodeRabbit must not be marked reviewed for a non-head commit, ReviewedBy=%#v", rep.ReviewedBy)
	}
}

func TestFeedbackNewerHeadReviewSupersedesOldBodyFindings(t *testing.T) {
	// The companion to the above: once CodeRabbit re-reviews the current head and
	// finds nothing (empty body), its newer review supersedes the older body
	// findings so the loop can converge instead of resurfacing addressed items.
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, extraFeedbackBots),
	}
	gh := newFakeGitHub()
	head := "9999999999999999"
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey("o/repo", 5)] = pull

	old := Review{
		ID: 7,
		Body: "**Actionable comments posted: 1**\n<details>\n<summary>🤖 Prompt for all review comments with AI agents</summary>\n\n" +
			"```\nIn `@src/app.ts`:\n- Around line 12-14: Stale state.\n```\n</details>",
		CommitID:    "1111111111111111",
		SubmittedAt: time.Now().UTC().Add(-time.Hour),
	}
	old.User.Login = "coderabbitai[bot]"
	fresh := Review{ID: 9, Body: "", CommitID: head, SubmittedAt: time.Now().UTC()}
	fresh.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []Review{old, fresh}

	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	rep, err := svc.Feedback(context.Background(), "o/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("newer head review should supersede the old body findings, got %#v", rep.Findings)
	}
	if !rep.Converged {
		t.Fatalf("should converge: fresh head review is clean, ReviewedBy=%#v", rep.ReviewedBy)
	}
}

func TestFeedbackCurrentRoundDoesNotResurfacePreRoundBodyFindings(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: unionBots([]string{"coderabbitai[bot]"}, extraFeedbackBots),
	}
	gh := newFakeGitHub()
	head := "9999999999999999"
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey("o/repo", 5)] = pull

	old := Review{
		ID: 7,
		Body: "**Actionable comments posted: 1**\n<details>\n<summary>🤖 Prompt for all review comments with AI agents</summary>\n\n" +
			"```\nIn `@src/app.ts`:\n- Around line 12-14: Stale state.\n```\n</details>",
		CommitID:    "1111111111111111",
		SubmittedAt: started.Add(-time.Hour),
	}
	old.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []Review{old}
	completion := IssueComment{
		ID:        10,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	completion.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 5)] = []IssueComment{completion}

	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("o/repo", 5)
		st.Fired[key] = head[:9]
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "o/repo",
			PR:        5,
			Head:      head[:9],
			StartedAt: started,
			Deadline:  started.Add(time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := NewService(cfg, gh, store, nil).Feedback(ctx, "o/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("pre-round body findings must not be re-emitted: %#v", rep.Findings)
	}
	if !rep.Converged || !rep.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("current clean completion should converge: %#v", rep)
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

func TestFeedbackMarksCurrentNoActionCompletionCommentReviewed(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("o/repo", 1)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "o/repo",
			PR:        1,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("expected completion comment to converge the round, got %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("expected CodeRabbit to be marked reviewed, ReviewedBy=%#v", report.ReviewedBy)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("completion comment must not become a finding: %#v", report.Findings)
	}
}

func TestFeedbackIgnoresStaleNoActionCompletionComment(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(-time.Minute),
		UpdatedAt: started.Add(-time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("o/repo", 1)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "o/repo",
			PR:        1,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Converged || report.Status != "waiting" || report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("stale completion comment must not satisfy the current round, got %#v", report)
	}
}

func TestFeedbackDoesNotUseNoActionCompletionWhileCodexRequiredWithoutThumbsUp(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("o/repo", 1)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "o/repo",
			PR:        1,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Converged || report.Status != "waiting" {
		t.Fatalf("expected to keep waiting for Codex without a thumbs-up, got %#v", report)
	}
	if report.ReviewedBy["coderabbitai[bot]"] || report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("no-action comment must not satisfy the round while Codex is active without +1: %#v", report.ReviewedBy)
	}
}

func TestFeedbackUsesNoActionCompletionAfterCodexThumbsUp(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	comment := IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(time.Minute),
		UpdatedAt: started.Add(time.Minute),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	thumb := Reaction{Content: "+1", CreatedAt: started.Add(2 * time.Minute)}
	thumb.User.Login = "chatgpt-codex-connector[bot]"
	gh.reactions[99] = []Reaction{thumb}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("o/repo", 1)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:           "o/repo",
			PR:             1,
			Head:           "abcdef123",
			StartedAt:      started,
			Deadline:       started.Add(time.Hour),
			FiredCommentID: 99,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("expected CodeRabbit no-action plus Codex +1 to converge, got %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] || !report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("expected both bots reviewed, ReviewedBy=%#v", report.ReviewedBy)
	}
}

func TestFeedbackIgnoresOptionalCodexCleanReviewSummary(t *testing.T) {
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		FeedbackBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	review := Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 1)] = []Review{review}
	comment := IssueComment{
		ID:        10,
		Body:      "## Codex Review\n\nDidn't find any major issues. Keep them coming!",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	report, err := svc.Feedback(context.Background(), "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("optional Codex clean summary must not block a clean required review: %#v", report)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Codex clean summary must not become a finding: %#v", report.Findings)
	}
	if _, tracked := report.ReviewedBy["chatgpt-codex-connector[bot]"]; tracked {
		t.Fatalf("optional Codex must remain outside the convergence gate: %#v", report.ReviewedBy)
	}
}

func TestFeedbackMarksRequiredCodexCleanReviewSummaryReviewed(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	review := Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: started.Add(time.Minute)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 1)] = []Review{review}
	comment := IssueComment{
		ID:        10,
		Body:      "## Codex Review\n\nDidn't find any major issues. Keep them coming!",
		CreatedAt: started.Add(2 * time.Minute),
		UpdatedAt: started.Add(2 * time.Minute),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("o/repo", 1)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "o/repo",
			PR:        1,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Converged || report.Status != "converged" {
		t.Fatalf("current Codex clean summary should converge both required bots: %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] || !report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("expected both required bots reviewed: %#v", report.ReviewedBy)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Codex clean summary must not become a finding: %#v", report.Findings)
	}
}

func TestFeedbackDoesNotUseStaleCodexCleanReviewSummary(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Bot:          "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 1)] = pull
	review := Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: started.Add(time.Minute)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 1)] = []Review{review}
	comment := IssueComment{
		ID:        10,
		Body:      "Codex Review: Didn’t find any major issues. Keep them coming!",
		CreatedAt: started.Add(-time.Minute),
		UpdatedAt: started.Add(-time.Minute),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("o/repo", 1)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "o/repo",
			PR:        1,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, err := svc.Feedback(ctx, "o/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Converged || report.Status != "waiting" {
		t.Fatalf("stale Codex clean summary must not satisfy the current round: %#v", report)
	}
	if !report.ReviewedBy["coderabbitai[bot]"] || report.ReviewedBy["chatgpt-codex-connector[bot]"] {
		t.Fatalf("only CodeRabbit should be current: %#v", report.ReviewedBy)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("stale Codex clean summary must still be non-actionable: %#v", report.Findings)
	}
}

func TestLoopConvergesOnCurrentNoActionCompletionComment(t *testing.T) {
	ctx := context.Background()
	started := time.Now().UTC().Add(-2 * time.Millisecond)
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		PollInterval:        time.Nanosecond,
		FeedbackWaitTimeout: time.Millisecond,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	comment := IssueComment{
		ID:        1,
		Body:      "No actionable comments were generated in the recent review. 🎉",
		CreatedAt: started.Add(500 * time.Microsecond),
		UpdatedAt: started.Add(500 * time.Microsecond),
	}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("owner/repo", 12)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "owner/repo",
			PR:        12,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(time.Millisecond),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.Converged {
		t.Fatalf("expected loop to stop converged on completion comment, code=%d report=%#v", code, report)
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

func TestLoopResumesAwaitingFeedbackWithoutRefiring(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	comment := IssueComment{ID: 91, Body: "Actionable finding on the current head", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
	review := Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review}
	store := NewMemoryStore(cfg)
	started := time.Now().UTC().Add(-time.Minute)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("owner/repo", 12)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "owner/repo",
			PR:        12,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(cfg.FeedbackWaitTimeout),
			ByHost:    "oldhost",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || len(report.Findings) != 1 {
		t.Fatalf("expected resumed loop to return the persisted review feedback, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("resuming an awaiting head must not post another review command, posted=%d", len(gh.posted))
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "" {
		t.Fatalf("feedback wait should clear after findings are collected, got %#v", wait)
	}
	if state.Fired[QueueKey("owner/repo", 12)] != "abcdef123" {
		t.Fatalf("fired marker should remain for dedupe after collection: %#v", state.Fired)
	}
}

func TestLoopWaitsForReplacementReviewInsteadOfReturningCarriedPrompt(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Second,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	stale := Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: time.Now().UTC().Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{stale}
	store := NewMemoryStore(cfg)
	started := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("owner/repo", 12)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "owner/repo",
			PR:        12,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(cfg.FeedbackWaitTimeout),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		fresh := Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
		fresh.User.Login = "coderabbitai[bot]"
		gh.mu.Lock()
		gh.reviews[fakeKey("owner/repo", 12)] = append(gh.reviews[fakeKey("owner/repo", 12)], fresh)
		gh.mu.Unlock()
	}()

	svc := NewService(cfg, gh, store, nil)
	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.Converged || len(report.Findings) != 0 {
		t.Fatalf("loop should wait for the clean replacement review, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("an existing feedback wait must not post another command, posted=%d", len(gh.posted))
	}
}

func TestLoopDoesNotReturnCodexFeedbackBeforeRequiredReviewSlot(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		WaitTimeout:         25 * time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := IssueComment{
		ID:        91,
		Body:      "Actionable finding on the current head",
		CreatedAt: headTime.Add(time.Second),
		UpdatedAt: headTime.Add(time.Second),
	}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	blockedUntil := time.Now().UTC().Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Blocked.BlockedUntil = &blockedUntil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 || report.Status != "timeout" || len(report.Findings) != 0 {
		t.Fatalf("Codex feedback must not finish a round before the required review can fire, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("feedback that is already visible must not fire a review, posted=%d", len(gh.posted))
	}
}

func TestLoopBuffersFasterCodexFeedbackUntilCodeRabbitReviews(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Second,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	codexFinding := IssueComment{
		ID:        91,
		Body:      "Actionable Codex finding on the current head",
		CreatedAt: headTime.Add(time.Second),
		UpdatedAt: headTime.Add(time.Second),
	}
	codexFinding.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{codexFinding}
	store := NewMemoryStore(cfg)
	started := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("owner/repo", 12)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "owner/repo",
			PR:        12,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(cfg.FeedbackWaitTimeout),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		fresh := Review{ID: 9, CommitID: pull.Head.SHA, SubmittedAt: time.Now().UTC()}
		fresh.User.Login = "coderabbitai[bot]"
		gh.mu.Lock()
		gh.reviews[fakeKey("owner/repo", 12)] = append(gh.reviews[fakeKey("owner/repo", 12)], fresh)
		gh.mu.Unlock()
	}()

	svc := NewService(cfg, gh, store, nil)
	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 || len(report.Findings) != 1 || !report.ReviewedBy["coderabbitai[bot]"] {
		t.Fatalf("loop should return Codex feedback only after CodeRabbit completes the round, code=%d report=%#v", code, report)
	}
}

func TestLoopUsesPersistedFeedbackDeadline(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Hour,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	started := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		key := QueueKey("owner/repo", 12)
		st.Fired[key] = "abcdef123"
		st.AwaitingFeedback[key] = FeedbackWait{
			Repo:      "owner/repo",
			PR:        12,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(cfg.FeedbackWaitTimeout),
			ByHost:    "oldhost",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	begin := time.Now()
	report, code, err := svc.Loop(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 250*time.Millisecond {
		t.Fatalf("expired persisted deadline should not reset to a fresh hour; elapsed=%s", elapsed)
	}
	if code != 2 || report.Status != "timeout" {
		t.Fatalf("expected timeout from the persisted deadline, code=%d report=%#v", code, report)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("expired feedback wait still must not refire the same head, posted=%d", len(gh.posted))
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "" {
		t.Fatalf("expired feedback wait should clear after timeout, got %#v", wait)
	}
}

func TestPumpEveryForNeverPumpsMoreThanOncePerMinute(t *testing.T) {
	if got := pumpEveryFor(15 * time.Second); got != time.Minute {
		t.Fatalf("fast polls must clamp pump cadence to a minute, got %v", got)
	}
	if got := pumpEveryFor(5 * time.Minute); got != 5*time.Minute {
		t.Fatalf("slow polls pump once per poll, got %v", got)
	}
}
