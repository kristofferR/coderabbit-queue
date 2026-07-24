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
//
// Co-reviewer kinds are generic — the bot's identity travels as data
// (BotEvent.For), never as a per-bot kind, so the engine's rules stay
// bot-shape-generic ("participated", "clean at SHA", "cannot finish", "was
// commanded") without enumerating bots. Kind values are in-memory only (never
// persisted), so renumbering between versions is safe.
type EventKind int

const (
	EvOther           EventKind = iota
	EvCommand                   // the review trigger command, posted by a human/agent
	EvCoCommand                 // a co-reviewer's trigger command, posted by a human/agent
	EvCompletion                // "Review finished." auto-reply (and not rate-limited)
	EvRateLimited               // CodeRabbit account-quota notice
	EvPaused                    // "Reviews paused" auto-pause notice
	EvInProgress                // editable top summary: review still processing
	EvFailed                    // editable top summary: review failed
	EvAlreadyReviewed           // "does not re-review already reviewed commits" claim
	EvNoAction                  // CodeRabbit clean-review summary (no actionable comments)
	EvCoClean                   // co-reviewer clean-summary issue comment
	EvCoUnable                  // co-reviewer cannot finish this round (Codex usage limits)
	EvCoNotice                  // other non-actionable co-reviewer notice (acks)
	EvCoVerdict                 // Macroscope approvability verdict (informational only)
)

// Deprecated: Codex-named aliases for the generic co-reviewer kinds, kept
// while engine/crq migrate. In-memory only — never persisted.
const (
	EvCodexCommand    = EvCoCommand
	EvCodexClean      = EvCoClean
	EvCodexUsageLimit = EvCoUnable
	EvCodexNotice     = EvCoNotice
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
	AutoReply bool // body carries the auto-reply (calibration) marker
	// SummaryOnly reports that this configured-bot comment declares a
	// summary-and-walkthrough-only plan (CodeRabbit Free on a private repo).
	// It is a property of the ACCOUNT, not of any round, so it rides alongside
	// the dominant Kind rather than competing with it — a rate-limit notice or
	// an in-progress summary must still classify as itself.
	SummaryOnly bool
	Window      *time.Time // EvRateLimited: parsed "available in" deadline
	Remaining   *int       // EvRateLimited: parsed remaining reviews
	SHA         string     // EvCoClean: reviewed-commit sha, "" if absent
	// For is the canonical co-reviewer login the event concerns: the commanded
	// bot for EvCoCommand, the authoring bot for a co-reviewer's own comments.
	// "" for primary-bot and human events.
	For string
	// Approved is the EvCoVerdict approvability verdict.
	Approved *bool
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
// CodeRabbit login; ReviewCommand is the exact trigger comment body;
// CoReviewers are the enabled co-reviewer entries with their config-resolved
// trigger commands (a nil slice falls back to the CodexCommand shim).
type Classifier struct {
	CodeRabbit    CodeRabbit
	Bot           string
	ReviewCommand string
	CoReviewers   []CoReviewer
	// CodexCommand is the exact Codex trigger comment body ("" disables
	// Codex-command matching).
	//
	// Deprecated: shim for callers that predate CoReviewers — when CoReviewers
	// is nil, Classify synthesizes the Codex registry entry from this field.
	CodexCommand string
}

// coReviewers resolves the co-reviewer set: the explicit list when set,
// otherwise the legacy Codex-only shim (preserving that Codex comments
// classify even with command matching disabled).
func (c Classifier) coReviewers() []CoReviewer {
	if c.CoReviewers != nil {
		return c.CoReviewers
	}
	codex, _ := CoReviewerByName("codex")
	codex.Command = strings.TrimSpace(c.CodexCommand)
	return []CoReviewer{codex}
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
	coReviewers := c.coReviewers()
	for _, co := range coReviewers {
		if co.matchesCommand(trimmed) && !co.Is(author) {
			ev.Kind = EvCoCommand
			ev.For = co.Login
			return ev
		}
	}
	for _, co := range coReviewers {
		if !co.Is(author) {
			continue
		}
		ev.For = co.Login
		if co.ClassifyComment != nil {
			coEv := co.ClassifyComment(body)
			ev.Kind = coEv.Kind
			ev.SHA = coEv.SHA
			ev.Approved = coEv.Approved
		}
		return ev
	}
	if !fromConfigured {
		return ev
	}
	ev.AutoReply = c.CodeRabbit.IsAutoReply(body)
	ev.SummaryOnly = c.CodeRabbit.IsSummaryOnlyPlan(body)
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
