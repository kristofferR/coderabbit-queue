package dialect

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goldenCR mirrors the marker defaults in Config, so the corpus classifies
// exactly as production does.
var goldenCR = CodeRabbit{
	CompletionMarker:  "Review finished",
	RateLimitMarker:   "rate limited by coderabbit.ai",
	CalibrationMarker: "auto-generated reply by CodeRabbit",
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	return string(data)
}

// TestGoldenClassification pins one corpus file per known bot-message format.
// When a bot ships a new phrasing, add a file and a row — the row IS the spec.
func TestGoldenClassification(t *testing.T) {
	cases := []struct {
		file            string
		rateLimited     bool
		paused          bool
		inProgress      bool
		failed          bool
		alreadyDone     bool
		completionReply bool
		autoReply       bool
		noAction        bool
		codexClean      bool
		codexUsageLimit bool
		nonActionable   bool
		availableIn     time.Duration // 0 = no window must parse
		reviewedSHA     string
		// author + wantKind pin Classifier.Classify's dominant kind for the file;
		// wantKind == EvOther (the zero value) skips the Classify assertion.
		author   string
		wantKind EventKind
	}{
		{file: "coderabbit/rate-limit-fair-usage.md", rateLimited: true, autoReply: true, availableIn: 48 * time.Minute},
		// Contains the "does not re-review" boilerplate in its help section —
		// must still classify as a rate limit, NOT as an already-reviewed ack.
		{file: "coderabbit/rate-limit-bold-window.md", rateLimited: true, autoReply: true, availableIn: 40 * time.Minute},
		{file: "coderabbit/rate-limit-legacy.md", rateLimited: true, availableIn: 3 * time.Minute},
		// No parseable window: the engine must fall back to its conservative fixed block.
		{file: "coderabbit/rate-limit-no-window.md", rateLimited: true, autoReply: true},
		{file: "coderabbit/review-in-progress.md", inProgress: true},
		{file: "coderabbit/review-failed.md", failed: true},
		{file: "coderabbit/reviews-paused.md", paused: true},
		{file: "coderabbit/no-actionable-comments.md", noAction: true},
		{file: "coderabbit/already-reviewed.md", alreadyDone: true, autoReply: true},
		{file: "coderabbit/completion-reply.md", completionReply: true, autoReply: true},
		// The standalone trailer is an ack; a real finding CARRYING the trailer
		// must stay actionable (a substring match dropped four real findings).
		{file: "coderabbit/thread-ack-also-applies.md", nonActionable: true},
		{file: "coderabbit/finding-with-also-applies-trailer.md"},
		{file: "codex/clean-summary-legacy.md", codexClean: true, noAction: true, nonActionable: true, author: "chatgpt-codex-connector[bot]", wantKind: EvCodexClean},
		{file: "codex/clean-summary-tada.md", codexClean: true, noAction: true, nonActionable: true, reviewedSHA: "4d9e8bca82", author: "chatgpt-codex-connector[bot]", wantKind: EvCodexClean},
		{file: "codex/usage-limit.md", codexUsageLimit: true, nonActionable: true, author: "chatgpt-codex-connector[bot]", wantKind: EvCodexUsageLimit},
		// Codex's "create an environment" platform ad, posted as a thread reply —
		// never a finding, never a rebuttal.
		{file: "codex/environment-notice.md", nonActionable: true, author: "chatgpt-codex-connector[bot]", wantKind: EvCodexNotice},
		{file: "codex/review-command.md", author: "kristofferR", wantKind: EvCodexCommand},
	}
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	classifier := Classifier{CodeRabbit: goldenCR, Bot: "coderabbitai[bot]", ReviewCommand: "@coderabbitai review", CodexCommand: "@codex review"}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body := readGolden(t, tc.file)
			checks := []struct {
				name string
				got  bool
				want bool
			}{
				{"IsRateLimited", goldenCR.IsRateLimited(body), tc.rateLimited},
				{"IsReviewsPaused", goldenCR.IsReviewsPaused(body), tc.paused},
				{"IsReviewInProgress", goldenCR.IsReviewInProgress(body), tc.inProgress},
				{"IsReviewFailure", goldenCR.IsReviewFailure(body), tc.failed},
				{"IsReviewAlreadyDone", goldenCR.IsReviewAlreadyDone(body), tc.alreadyDone},
				{"IsCompletionReply", goldenCR.IsCompletionReply(body), tc.completionReply},
				{"IsAutoReply", goldenCR.IsAutoReply(body), tc.autoReply},
				{"IsNoActionReviewCompletion", IsNoActionReviewCompletion(body), tc.noAction},
				{"IsCodexNoActionReviewCompletion", IsCodexNoActionReviewCompletion(body), tc.codexClean},
				{"IsCodexUsageLimit", IsCodexUsageLimit(body), tc.codexUsageLimit},
				{"IsNonActionableText", IsNonActionableText(body), tc.nonActionable},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
				}
			}
			reset := ParseAvailableIn(body, base)
			if tc.availableIn == 0 {
				if reset != nil {
					t.Errorf("ParseAvailableIn = %v, want none", reset)
				}
			} else if reset == nil || !reset.Equal(base.Add(tc.availableIn)) {
				t.Errorf("ParseAvailableIn = %v, want base+%v", reset, tc.availableIn)
			}
			if got := CodexReviewedCommitSHA(body); got != tc.reviewedSHA {
				t.Errorf("CodexReviewedCommitSHA = %q, want %q", got, tc.reviewedSHA)
			}
			if tc.wantKind != EvOther {
				if got := classifier.Classify(tc.author, body, 1, base, base).Kind; got != tc.wantKind {
					t.Errorf("Classify kind = %v, want %v", got, tc.wantKind)
				}
			}
		})
	}
}

// TestGoldenFindings pins the review-body finding extractors against real
// review-body markup shapes.
func TestGoldenFindings(t *testing.T) {
	meta := ReviewMeta{
		ID:          99,
		CommitID:    "abcdef1234567890",
		HTMLURL:     "https://example.test/r/99",
		SubmittedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
	type want struct {
		path     string
		line     int
		severity string // "" = don't check
		title    string // "" = don't check
		source   string
		commit   string // "" = don't check
	}
	cases := []struct {
		file string
		bot  string
		want []want
	}{
		{
			file: "coderabbit/findings-outside-diff.md",
			bot:  "coderabbitai[bot]",
			want: []want{{path: "internal/foo.go", line: 42, severity: "major", title: "Fix the cancellation path.", source: "review_body"}},
		},
		{
			file: "coderabbit/findings-nested-quotes.md",
			bot:  "coderabbitai[bot]",
			want: []want{
				{path: "internal/deep.go", line: 10, severity: "major", title: "Nested finding one.", source: "review_body"},
				{path: "internal/deeper.go", line: 20, severity: "minor", title: "Nested finding two.", source: "review_body"},
			},
		},
		{
			file: "coderabbit/findings-failed-to-post.md",
			bot:  "coderabbitai[bot]",
			want: []want{{path: "src-tauri/inject/messenger.js", line: 561, severity: "major", title: "Move the hide-names toggle out of `messenger.js` or update the allowlist first.", source: "review_body"}},
		},
		{
			file: "coderabbit/findings-prompt-block.md",
			bot:  "coderabbitai[bot]",
			want: []want{
				{path: "src/app.ts", line: 12, source: "review_prompt"},
				{path: "README.md", line: 7, source: "review_prompt"},
			},
		},
		{
			file: "codex/findings-outside-diff.md",
			bot:  "chatgpt-codex-connector[bot]",
			want: []want{{path: "convex/sections/aiCommands.ts", line: 2170, severity: "minor", title: "Query learning history by topic before taking", source: "review_body", commit: "347388ffd"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body := readGolden(t, tc.file)
			got := ParseReviewBodyFindings(body, meta, tc.bot)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings, want %d: %#v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				f := got[i]
				if f.Path != w.path || f.Line != w.line {
					t.Errorf("finding %d location = %s:%d, want %s:%d", i, f.Path, f.Line, w.path, w.line)
				}
				if w.severity != "" && f.Severity != w.severity {
					t.Errorf("finding %d severity = %q, want %q", i, f.Severity, w.severity)
				}
				if w.title != "" && f.Title != w.title {
					t.Errorf("finding %d title = %q, want %q", i, f.Title, w.title)
				}
				if f.Source != w.source {
					t.Errorf("finding %d source = %q, want %q", i, f.Source, w.source)
				}
				if w.commit != "" && f.Commit != w.commit {
					t.Errorf("finding %d commit = %q, want %q", i, f.Commit, w.commit)
				}
			}
		})
	}
}

// TestGoldenReplyVerdict pins the concede/contest classification of a bot's
// reply to the agent's decline, using CodeRabbit's real replies from PR #30.
func TestGoldenReplyVerdict(t *testing.T) {
	cases := []struct {
		file      string
		withdrawn bool
		retained  bool
	}{
		{file: "coderabbit/reply-withdrawn.md", withdrawn: true},
		{file: "coderabbit/reply-retained.md", retained: true},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body := readGolden(t, tc.file)
			if got := IsReviewFindingWithdrawn(body); got != tc.withdrawn {
				t.Errorf("IsReviewFindingWithdrawn = %v, want %v", got, tc.withdrawn)
			}
			if got := IsReviewFindingRetained(body); got != tc.retained {
				t.Errorf("IsReviewFindingRetained = %v, want %v", got, tc.retained)
			}
		})
	}
}
