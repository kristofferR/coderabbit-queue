package crq

import (
	"testing"
	"time"
)

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
