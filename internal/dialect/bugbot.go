package dialect

import (
	"regexp"
	"strings"
)

// Cursor Bugbot wording. Verified against the live corpus in
// testdata/bugbot/ (pingdotgg/t3code PRs): it auto-reviews every push, posts a
// COMMENTED review summary only when it FOUND issues, carries each finding as
// an inline comment with a stable BUG_ID, and reports a clean round only via
// its "Cursor Bugbot" check run (app slug "cursor") — never on the timeline.

// BugbotLogin is the canonical Cursor Bugbot GitHub app login.
const BugbotLogin = "cursor[bot]"

const bugbotCheckName = "Cursor Bugbot"

var (
	// bugbotReviewMarkerRE matches the hidden marker Bugbot opens its review
	// summary body with.
	bugbotReviewMarkerRE = regexp.MustCompile(`<!--\s*BUGBOT_REVIEW\s*-->`)
	// bugbotFooterRE matches the "Reviewed by [Cursor Bugbot](…) for commit
	// <full-sha>" footer line on inline finding comments.
	bugbotFooterRE = regexp.MustCompile(`(?im)^.*Reviewed by \[Cursor Bugbot\]\([^)]*\) for commit ([0-9a-fA-F]{7,40}).*$`)
	// bugbotBugIDRE matches the stable per-bug identity marker.
	bugbotBugIDRE = regexp.MustCompile(`<!--\s*BUGBOT_BUG_ID:\s*([0-9a-fA-F-]+)\s*-->`)
)

func IsBugbotBot(login string) bool {
	return NormalizeBotName(login) == NormalizeBotName(BugbotLogin)
}

// IsBugbotReviewSummary reports whether a review body is Bugbot's summary
// ("Cursor Bugbot has reviewed your changes … found N potential issues").
// The summary itself carries ZERO findings — they arrive as inline comments —
// so the review-body finding parsers must never extract from it.
func IsBugbotReviewSummary(body string) bool {
	return bugbotReviewMarkerRE.MatchString(body)
}

// BugbotReviewedCommitSHA extracts the commit hash from the "Reviewed by
// [Cursor Bugbot](…) for commit <sha>" footer, or "" when absent.
func BugbotReviewedCommitSHA(text string) string {
	match := bugbotFooterRE.FindStringSubmatch(text)
	if len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return ""
}

// BugbotFindingDedupeKey extracts the BUG_ID marker that keeps a Bugbot
// finding's identity stable when it re-reports the same bug in a new thread
// after a push.
func BugbotFindingDedupeKey(body string) (string, bool) {
	match := bugbotBugIDRE.FindStringSubmatch(body)
	if len(match) == 2 {
		return strings.ToLower(match[1]), true
	}
	return "", false
}

// CleanBugbotCommentText strips Bugbot's footer line from a finding body
// (htmlCommentRE in CompactReviewBody already strips the marker comments).
func CleanBugbotCommentText(text string) string {
	return strings.TrimSpace(bugbotFooterRE.ReplaceAllString(text, ""))
}

// ClassifyBugbotCheck classifies one check run owned by the "cursor" app.
// Only the check named "Cursor Bugbot" is its review: clean rounds conclude
// `success` with "no issues found" in the summary, findings rounds conclude
// `neutral` (verified live). Clean-ness is decided by summary wording, not by
// the conclusion, so a conclusion-vocabulary change degrades to CheckDone
// (findings still gate via threads) instead of misreading clean.
//
// A run that terminated WITHOUT delivering a review (failure, cancelled,
// timed_out) is still status "completed", so it must be rejected explicitly:
// counting it as a finished review lets a round converge with no findings
// even though Bugbot never actually reviewed the code.
func ClassifyBugbotCheck(name, title, summary, status, conclusion string) CheckVerdict {
	if name != bugbotCheckName {
		return CheckUnrelated
	}
	if !strings.EqualFold(status, "completed") {
		return CheckInProgress
	}
	if checkRunFailed(conclusion) {
		return CheckFailed
	}
	if strings.Contains(NormalizeReviewText(summary), "no issues found") ||
		strings.Contains(NormalizeReviewText(title), "no issues found") {
		return CheckDoneClean
	}
	return CheckDone
}
