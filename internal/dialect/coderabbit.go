package dialect

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CodeRabbit classifies CodeRabbit's comment bodies. The markers come from
// config (CRQ_COMPLETION_MARKER, CRQ_RL_MARKER, CRQ_CAL_REPLY_MARKER); the
// remaining phrasings are CodeRabbit's own current wording, pinned by the
// golden corpus in testdata/coderabbit.
type CodeRabbit struct {
	CompletionMarker  string
	RateLimitMarker   string
	CalibrationMarker string
}

// IsCompletionReply reports whether body is the bot's reply to a processed
// review command (CodeRabbit: "Review finished."). An empty marker disables
// the completion-reply convergence fallback entirely.
func (d CodeRabbit) IsCompletionReply(body string) bool {
	marker := strings.TrimSpace(d.CompletionMarker)
	if marker == "" {
		return false
	}
	return strings.Contains(strings.ToLower(body), strings.ToLower(marker))
}

// IsAutoReply reports whether body is one of the bot's auto-generated replies
// to a command — completion, rate-limit, skip, or progress. The bot posts
// exactly one per command, which is what lets completions be paired to the
// command they answer.
func (d CodeRabbit) IsAutoReply(body string) bool {
	marker := strings.TrimSpace(d.CalibrationMarker)
	if marker == "" {
		return false
	}
	return strings.Contains(strings.ToLower(body), strings.ToLower(marker))
}

// IsRateLimited reports whether a CodeRabbit comment is a rate-limit notice. It
// matches the configured CRQ_RL_MARKER plus CodeRabbit's current phrasings (the
// "Fair Usage Limits Policy" / "currently rate limited" message), which the old
// "rate limited by coderabbit.ai" marker alone misses — so a fired review that
// comes back rate-limited is detected and crq backs off instead of firing on.
func (d CodeRabbit) IsRateLimited(body string) bool {
	l := strings.ToLower(body)
	if m := strings.ToLower(strings.TrimSpace(d.RateLimitMarker)); m != "" && strings.Contains(l, m) {
		return true
	}
	return strings.Contains(l, "currently rate limited") ||
		strings.Contains(l, "rate limited under") ||
		strings.Contains(l, "fair usage limits policy")
}

// IsReviewsPaused reports whether a CodeRabbit comment is the "Reviews paused"
// auto-pause notice. CodeRabbit posts this when a branch is under active
// development (an influx of new commits) and auto_pause_after_reviewed_commits
// kicks in. It acknowledges the branch but is not a review of the fired head, so
// — like a rate-limit notice — it must not be mistaken for a completed review
// round: doing so would falsely converge a loop with zero findings. crq keeps
// triggering reviews explicitly, and "@coderabbitai review" still produces a
// single review while auto-review is paused, so the round completes on the real
// review, not this note.
func (d CodeRabbit) IsReviewsPaused(body string) bool {
	l := strings.ToLower(body)
	return strings.Contains(l, "reviews paused") ||
		strings.Contains(l, "automatically paused this review") ||
		strings.Contains(l, "auto_pause_after_reviewed_commits")
}

// IsReviewAlreadyDone identifies CodeRabbit's "does not re-review already
// reviewed commits" acknowledgement. The text is only a claim, not completion
// evidence: callers require a matching GitHub review before trusting it.
// The same boilerplate can appear inside a rate-limit notice's help section, so
// a comment that is itself a rate limit is excluded.
func (d CodeRabbit) IsReviewAlreadyDone(body string) bool {
	l := strings.ToLower(body)
	if !strings.Contains(l, "does not re-review already reviewed") &&
		!strings.Contains(l, "already reviewed commit") {
		return false
	}
	return !d.IsRateLimited(l)
}

// IsReviewInProgress reports whether body is CodeRabbit's editable top-summary
// state for a review that has started but has not finished. CodeRabbit can post
// a "Review finished" command reply before this summary leaves the processing
// state, so the reply alone is not a terminal signal.
func (d CodeRabbit) IsReviewInProgress(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "currently processing new changes in this pr") ||
		strings.Contains(lower, "review in progress by coderabbit.ai")
}

// IsReviewFailure reports whether body is CodeRabbit's editable top-summary
// failure state. CodeRabbit can still change the command reply to "Review
// finished" after this summary reports that the review itself failed, so the
// reply is not evidence that the current head was reviewed successfully.
func (d CodeRabbit) IsReviewFailure(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "auto-generated comment: failure by coderabbit.ai") ||
		strings.Contains(lower, "## review failed")
}

// ParseAvailableIn extracts CodeRabbit's "next review available in <duration>"
// window from a rate-limit comment and returns base+duration. It tolerates the
// markdown and punctuation CodeRabbit now wraps the value in — the current
// phrasing is "**Next review available in:** **40 minutes**", where a colon and
// bold markers sit between "in" and the number. An unparseable body returns nil;
// the caller then falls back to a conservative fixed window rather than a short
// retry (getting this wrong is exactly what let the daemon re-fire every couple
// of minutes instead of honouring a 40-minute limit).
func ParseAvailableIn(text string, base time.Time) *time.Time {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "available in")
	if idx < 0 {
		return nil
	}
	frag := lower[idx+len("available in"):]
	// Normalise markdown/punctuation to spaces so "in:** **40 minutes**" scans as
	// "40 minutes". Do this before splitting into fields so bold/colon can't fuse
	// onto the number ("**40") and defeat the numeric parse.
	frag = strings.Map(func(r rune) rune {
		switch r {
		case '*', ':', '`', ',', '_', '(', ')':
			return ' '
		}
		return r
	}, frag)
	// Stop at a sentence boundary so a later number in the body isn't read as part
	// of the window.
	if dot := strings.IndexByte(frag, '.'); dot >= 0 {
		frag = frag[:dot]
	}
	fields := strings.Fields(frag)
	var d time.Duration
	for i := 0; i+1 < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch unit := fields[i+1]; {
		case strings.HasPrefix(unit, "hour"):
			d += time.Duration(n) * time.Hour
		case strings.HasPrefix(unit, "minute"):
			d += time.Duration(n) * time.Minute
		case strings.HasPrefix(unit, "second"):
			d += time.Duration(n) * time.Second
		}
	}
	if d == 0 {
		return nil
	}
	t := base.Add(d)
	return &t
}

func ParseQuota(text string, base time.Time) (*int, *time.Time) {
	remaining := ParseRemainingReviews(text)
	reset := ParseAvailableIn(text, base)
	return remaining, reset
}

func ParseRemainingReviews(text string) *int {
	lower := strings.ToLower(text)
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	for i := 0; i < len(words); i++ {
		n, err := strconv.Atoi(words[i])
		if err != nil {
			continue
		}
		if i+2 < len(words) && strings.HasPrefix(words[i+1], "review") && (words[i+2] == "remaining" || words[i+2] == "left") {
			return &n
		}
		if i > 0 && (words[i-1] == "remaining" || words[i-1] == "left") {
			return &n
		}
	}
	return nil
}

var (
	detailSummaryRE = regexp.MustCompile(`(?i)<summary>\s*([^<]+?)\s+\([0-9]+\)\s*</summary>`)
	// Line headers come backticked in "Outside diff range comments" (`12-15`:) and
	// un-backticked in "Comments failed to post" (12-15:) — accept both.
	detailHeaderRE = regexp.MustCompile("^`?([0-9]+)(?:\\s*-\\s*([0-9]+))?`?: *(.*)$")
	promptBlockRE  = regexp.MustCompile("(?is)<summary>[^<]*Prompt for all review comments with AI agents[^<]*</summary>.*?```\\s*(.*?)\\s*```")
	promptFileRE   = regexp.MustCompile("^In (?:`@([^`]+)`|@([^:]+)):$")
	promptBulletRE = regexp.MustCompile("^- (?:Around line|Line)\\s+([0-9]+)(?:\\s*-\\s*([0-9]+))?:\\s*(.*)$")
)

// ParseReviewBodyFindings extracts every finding representable only in a
// review's body text: CodeRabbit's failed-to-post/outside-diff detail blocks,
// its "Prompt for AI agents" block, and Codex's blob-link items.
func ParseReviewBodyFindings(body string, review ReviewMeta, bot string) []Finding {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	clean := StripMarkdownQuote(body)
	out := ParseDetailedReviewFindings(clean, review, bot)
	out = append(out, ParsePromptReviewFindings(clean, review, bot)...)
	out = append(out, ParseCodexReviewFindings(clean, review, bot)...)
	return out
}

func ParseDetailedReviewFindings(body string, review ReviewMeta, bot string) []Finding {
	lines := strings.Split(body, "\n")
	var out []Finding
	currentPath := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if match := detailSummaryRE.FindStringSubmatch(line); match != nil {
			summary := strings.TrimSpace(match[1])
			if LooksLikePath(summary) {
				currentPath = summary
			}
			continue
		}
		match := detailHeaderRE.FindStringSubmatch(line)
		if match == nil || currentPath == "" {
			continue
		}
		startLine, _ := strconv.Atoi(match[1])
		meta := strings.TrimSpace(match[3])
		if IsNonActionableText(meta) {
			continue
		}
		start := i + 1
		end := len(lines)
		for j := start; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if detailHeaderRE.MatchString(next) || detailSummaryRE.MatchString(next) {
				end = j
				break
			}
		}
		block := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		title := TitleFromDetailedBlock(block)
		if title == "" {
			title = TitleOf(block)
		}
		bodyText := CompactReviewBody(block)
		finding := Finding{
			Bot:       bot,
			Severity:  SeverityOf(meta + "\n" + block),
			Path:      strings.TrimPrefix(currentPath, "@"),
			Line:      startLine,
			Title:     title,
			Body:      bodyText,
			ReviewID:  review.ID,
			Commit:    ShortOID(review.CommitID),
			URL:       review.HTMLURL,
			Source:    "review_body",
			CreatedAt: review.SubmittedAt,
		}
		if IsActionableFinding(finding) {
			out = append(out, finding)
		}
	}
	return out
}

func ParsePromptReviewFindings(body string, review ReviewMeta, bot string) []Finding {
	var out []Finding
	for _, blockMatch := range promptBlockRE.FindAllStringSubmatch(body, -1) {
		block := blockMatch[1]
		lines := strings.Split(block, "\n")
		currentPath := ""
		for i := 0; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if match := promptFileRE.FindStringSubmatch(line); match != nil {
				currentPath = firstNonEmpty(match[1], match[2])
				currentPath = strings.TrimPrefix(currentPath, "@")
				continue
			}
			match := promptBulletRE.FindStringSubmatch(line)
			if match == nil || currentPath == "" {
				continue
			}
			startLine, _ := strconv.Atoi(match[1])
			parts := []string{strings.TrimSpace(match[3])}
			for j := i + 1; j < len(lines); j++ {
				next := strings.TrimSpace(lines[j])
				if next == "" {
					continue
				}
				if strings.HasPrefix(next, "---") || promptFileRE.MatchString(next) || promptBulletRE.MatchString(next) {
					break
				}
				parts = append(parts, next)
				i = j
			}
			bodyText := strings.TrimSpace(strings.Join(parts, " "))
			finding := Finding{
				Bot:       bot,
				Severity:  SeverityOf(bodyText),
				Path:      currentPath,
				Line:      startLine,
				Title:     TitleOf(bodyText),
				Body:      bodyText,
				ReviewID:  review.ID,
				Commit:    ShortOID(review.CommitID),
				URL:       review.HTMLURL,
				Source:    "review_prompt",
				CreatedAt: review.SubmittedAt,
			}
			if IsActionableFinding(finding) {
				out = append(out, finding)
			}
		}
	}
	return out
}
