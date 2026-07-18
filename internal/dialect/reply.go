package dialect

import "strings"

// A bot replies to the agent's decline of a review finding with one of two
// verdicts: it withdraws the finding (concedes) or retains it (contests). crq
// must read that reply — a contested rebuttal on a thread the agent already
// resolved would otherwise be silently dropped, and the agent would never learn
// the reviewer stood its ground. These classifiers own that wording; the
// surfacing decision lives in crq/feedback. The golden corpus pins CodeRabbit's
// real phrasing from testdata/coderabbit/reply-*.md.

// IsReviewFindingWithdrawn reports whether a bot's reply concedes and withdraws
// its finding — the agent's decline stands and the thread is done.
func IsReviewFindingWithdrawn(text string) bool {
	t := NormalizeReviewText(text)
	return strings.Contains(t, "withdrawing this") ||
		strings.Contains(t, "withdrawing the finding") ||
		strings.Contains(t, "withdrawing my") ||
		strings.Contains(t, "i'll withdraw") ||
		strings.Contains(t, "i will withdraw") ||
		strings.Contains(t, "finding was incorrect") ||
		strings.Contains(t, "my mistake")
}

// IsReviewFindingRetained reports whether a bot's reply retains or contests its
// finding despite the agent's decline — a rebuttal the agent must re-address.
func IsReviewFindingRetained(text string) bool {
	t := NormalizeReviewText(text)
	return strings.Contains(t, "retaining the finding") ||
		strings.Contains(t, "retaining this") ||
		strings.Contains(t, "keeping this finding") ||
		strings.Contains(t, "keeping the finding") ||
		strings.Contains(t, "i disagree") ||
		strings.Contains(t, "i'm not convinced") ||
		strings.Contains(t, "still stands") ||
		strings.Contains(t, "still applies") ||
		strings.Contains(t, "still holds")
}
