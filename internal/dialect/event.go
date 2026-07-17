package dialect

import (
	"strings"
	"time"
)

// EventKind is the dominant classification of one issue comment. Priority
// order in Classify encodes load-bearing semantics: a rate-limit notice wins
// over the completion marker it may also contain (a rate-limited reply must
// never converge a round), and the already-reviewed ack is only reported when
// the body is not itself a rate limit.
type EventKind int

const (
	EvOther EventKind = iota
	EvCommand         // the review trigger command, posted by a human/agent
	EvCompletion      // "Review finished." auto-reply (and not rate-limited)
	EvRateLimited     // CodeRabbit account-quota notice
	EvPaused          // "Reviews paused" auto-pause notice
	EvInProgress      // editable top summary: review still processing
	EvFailed          // editable top summary: review failed
	EvAlreadyReviewed // "does not re-review already reviewed commits" claim
	EvNoAction        // CodeRabbit clean-review summary (no actionable comments)
	EvCodexClean      // Codex clean-summary issue comment
	EvCodexNotice     // non-actionable Codex notice (usage limits, acks)
)

// BotEvent is one classified issue comment. CreatedAt orders command↔reply
// pairing; UpdatedAt matters because CodeRabbit edits its top summary and its
// rate-limit comment in place.
type BotEvent struct {
	Kind      EventKind
	Bot       string // author login as observed (may carry the [bot] suffix)
	CommentID int64
	CreatedAt time.Time
	UpdatedAt time.Time
	AutoReply bool       // body carries the auto-reply (calibration) marker
	Window    *time.Time // EvRateLimited: parsed "available in" deadline
	Remaining *int       // EvRateLimited: parsed remaining reviews
	SHA       string     // EvCodexClean: reviewed-commit sha, "" if absent
}

// PairTime is the timestamp used for command↔reply pairing (CreatedAt, with
// UpdatedAt as fallback for API responses that omit it).
func (e BotEvent) PairTime() time.Time {
	if !e.CreatedAt.IsZero() {
		return e.CreatedAt
	}
	return e.UpdatedAt
}

// ObservedTime is the timestamp used for round-window checks. In-place-edited
// comments (top summary, rate-limit notice) belong to the round of their last
// edit, so UpdatedAt wins when it is later.
func (e BotEvent) ObservedTime() time.Time {
	if e.UpdatedAt.After(e.CreatedAt) {
		return e.UpdatedAt.UTC()
	}
	return e.CreatedAt.UTC()
}

// Classifier classifies issue comments into BotEvents. Bot is the configured
// CodeRabbit login; ReviewCommand is the exact trigger comment body.
type Classifier struct {
	CodeRabbit    CodeRabbit
	Bot           string
	ReviewCommand string
}

// Classify maps one issue comment to its BotEvent. Unrecognized comments
// (including all human commentary) come back as EvOther.
func (c Classifier) Classify(author, body string, id int64, createdAt, updatedAt time.Time) BotEvent {
	ev := BotEvent{Kind: EvOther, Bot: author, CommentID: id, CreatedAt: createdAt, UpdatedAt: updatedAt}
	trimmed := strings.TrimSpace(body)
	fromConfigured := NormalizeBotName(author) == NormalizeBotName(c.Bot)

	if command := strings.TrimSpace(c.ReviewCommand); command != "" && trimmed == command && !fromConfigured {
		ev.Kind = EvCommand
		return ev
	}
	if IsCodexBot(author) {
		if IsCodexNoActionReviewCompletion(body) {
			ev.Kind = EvCodexClean
			ev.SHA = CodexReviewedCommitSHA(body)
		} else if IsNonActionableText(body) {
			ev.Kind = EvCodexNotice
		}
		return ev
	}
	if !fromConfigured {
		return ev
	}
	ev.AutoReply = c.CodeRabbit.IsAutoReply(body)
	switch {
	case c.CodeRabbit.IsRateLimited(body):
		ev.Kind = EvRateLimited
		ev.Window = ParseAvailableIn(body, updatedAt)
		ev.Remaining = ParseRemainingReviews(body)
	case c.CodeRabbit.IsReviewsPaused(body):
		ev.Kind = EvPaused
	case c.CodeRabbit.IsReviewInProgress(body):
		ev.Kind = EvInProgress
	case c.CodeRabbit.IsReviewFailure(body):
		ev.Kind = EvFailed
	case c.CodeRabbit.IsReviewAlreadyDone(body):
		ev.Kind = EvAlreadyReviewed
	case IsNoActionReviewCompletion(body):
		ev.Kind = EvNoAction
	case c.CodeRabbit.IsCompletionReply(body):
		ev.Kind = EvCompletion
	}
	return ev
}
