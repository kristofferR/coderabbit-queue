package engine

import (
	"sort"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// CompletionStatus is the ONE answer to "is this round done?" — shared by the
// daemon (slot release / round completion) and the loop (convergence). It
// replaces v2's divergent inflightStatus and Feedback implementations.
type CompletionStatus struct {
	ReviewedBy map[string]bool
	Done       bool
}

// Completion decides which required bots have review evidence for the round's
// head. The rules are exact ports of v2:
//
//  1. A submitted review whose commit prefixes the head counts; with no head
//     to match, submission at/after the fire counts (reviewedByForRound).
//  2. Codex's clean summary counts when its "Reviewed commit" SHA matches the
//     head, or — SHA-less legacy format — when posted at/after the fire.
//  3. CodeRabbit's clean-review summary counts when posted at/after the fire,
//     gated on Codex being inactive or thumbed-up (codexInactiveOrThumbed):
//     if Codex gates or participates in this round, its silence must not let
//     the round converge on CodeRabbit's word alone.
//  4. The completion-reply fallback: a "Review finished." reply pairs to this
//     round's command and stands in for a no-findings re-review — only if the
//     bot has ANY prior submitted review, the pairing is chronologically
//     sound, and no in-progress/rate-limited/paused/failed top-summary state
//     contradicts it (the c22eb4b/e2aa2f0 gates).
func Completion(r state.Round, obs Observation, p Policy) CompletionStatus {
	reviewedBy := map[string]bool{}
	for _, bot := range p.RequiredBots {
		bot = strings.TrimSpace(bot)
		if bot != "" {
			reviewedBy[bot] = false
		}
	}
	cutoff := time.Time{}
	if r.FiredAt != nil {
		cutoff = r.FiredAt.UTC()
	}

	// Dynamic Codex gate: a fired round that Codex participates in — reviewing the
	// PR on its own (CodexAutoActive) or acting this round (CodexActiveThisRound) —
	// waits for Codex too, even when Codex is not configured-required, so its
	// findings are not skipped. A usage-limit exhaustion notice disengages this
	// gate (Codex cannot finish this round); configured-required Codex is left to
	// the wait deadline as before.
	if r.FiredAt != nil && !dialect.HasCodexBot(p.RequiredBots) &&
		(obs.CodexAutoActive || obs.CodexActiveThisRound) && !codexUsageLimitedSince(obs, cutoff) {
		reviewedBy[codexBot] = false
	}

	// 1. Submitted reviews.
	for _, review := range obs.Reviews {
		if r.Head != "" {
			if strings.HasPrefix(review.Commit, r.Head) {
				markReviewed(reviewedBy, review.Bot)
			}
			continue
		}
		if notBefore(review.SubmittedAt, cutoff) {
			markReviewed(reviewedBy, review.Bot)
		}
	}

	// 2. Codex clean-summary issue comments.
	for _, ev := range obs.Events {
		if ev.Kind != dialect.EvCodexClean {
			continue
		}
		if ev.SHA != "" {
			// The newer format names the reviewed commit — bind on that SHA
			// directly. A summary for another commit never counts, and a
			// matching one counts even when the round anchor was lost.
			if r.Head != "" && dialect.SHAPrefixMatch(ev.SHA, r.Head) {
				markReviewed(reviewedBy, ev.Bot)
			}
			continue
		}
		// SHA-less summaries bind from the Codex command time when crq posted
		// it before the (deferred) CodeRabbit fire — see codexCutoff.
		if (r.FiredAt != nil || r.CodexCommandedAt != nil) &&
			notBefore(ev.ObservedTime(), codexCutoff(r)) {
			markReviewed(reviewedBy, ev.Bot)
		}
	}

	// A Codex thumbs-up stands in for its review whenever Codex gates the round.
	// codexInactiveOrThumbed also consumes it on the CodeRabbit clean-summary path;
	// marking it here covers the case where CodeRabbit submitted a real review, so
	// step 3 never runs — otherwise a thumbs-up would engage the dynamic gate
	// without being able to satisfy it.
	if obs.CodexThumbsUp && needsBotReview(reviewedBy, codexBot) {
		markReviewed(reviewedBy, codexBot)
	}

	// 3. CodeRabbit clean-review summary, Codex-gated.
	if r.FiredAt != nil {
		for _, ev := range obs.Events {
			if ev.Kind != dialect.EvNoAction || !sameBot(ev.Bot, p.Bot) || !notBefore(ev.ObservedTime(), cutoff) {
				continue
			}
			if codexInactiveOrThumbed(r, obs, p, cutoff, reviewedBy) {
				markReviewed(reviewedBy, ev.Bot)
			}
		}
	}

	// 4. Completion-reply fallback.
	if needsBotReview(reviewedBy, p.Bot) && r.FiredAt != nil &&
		completionReplyForRound(obs, p, cutoff) {
		markReviewed(reviewedBy, p.Bot)
	}

	return CompletionStatus{ReviewedBy: reviewedBy, Done: allReviewed(reviewedBy)}
}

// codexInactiveOrThumbed ports v2's rule: CodeRabbit's clean summary may only
// converge the round when Codex either has already reviewed, was never active
// on this round, or has thumbed the round up. A thumbs-up also counts as
// Codex's review.
func codexInactiveOrThumbed(r state.Round, obs Observation, p Policy, cutoff time.Time, reviewedBy map[string]bool) bool {
	if reviewedByBot(reviewedBy, codexBot) || codexReviewedRound(r, obs, cutoff) {
		return true
	}
	// Codex either gates by configuration, or participates via a round-window
	// comment/clean summary; its notices (usage limits, acks) do not count.
	if !dialect.HasCodexBot(p.RequiredBots) && !codexCommentedRound(obs, cutoff) {
		return true
	}
	if obs.CodexThumbsUp {
		markReviewed(reviewedBy, codexBot)
		return true
	}
	return false
}

// completionReplyForRound ports v2's completionReplyForFiredCommand: replies
// pair chronologically with the earliest unanswered command, submitted
// reviews consume the command they answered, and a completion only stands
// when the bot has a prior submitted review and no nonterminal or failed
// top-summary state contradicts it.
func completionReplyForRound(obs Observation, p Policy, firedAt time.Time) bool {
	if !botHasAnyReview(obs.Reviews, p.Bot) {
		return false
	}
	for _, reply := range commandReplies(obs, p) {
		if reply.completion && notBefore(reply.commandAt, firedAt) &&
			!stateSince(obs, p, reply.commandAt, dialect.EvInProgress, dialect.EvRateLimited, dialect.EvPaused) &&
			!stateSince(obs, p, reply.commandAt, dialect.EvFailed) {
			return true
		}
	}
	return false
}

// CommandHasCompletionReply reports whether the specific command comment was
// answered by a completion reply with no in-progress/rate-limited/paused top
// summary contradicting it since. It ports v2's reviewCommandHasCompletionReply
// (the adoption guard: a command already answered by a completion reply belongs
// to a finished round and must not be re-adopted as a fresh fire). Unlike the
// convergence fallback it does not require a prior submitted review or gate on
// a failed summary — adoption only asks "was this exact command already spoken
// for".
func CommandHasCompletionReply(obs Observation, p Policy, commandID int64) bool {
	if commandID == 0 {
		return false
	}
	for _, reply := range commandReplies(obs, p) {
		if reply.commandID == commandID && reply.completion &&
			!stateSince(obs, p, reply.commandAt, dialect.EvInProgress, dialect.EvRateLimited, dialect.EvPaused) {
			return true
		}
	}
	return false
}

func botHasAnyReview(reviews []ReviewSeen, bot string) bool {
	for _, review := range reviews {
		if sameBot(review.Bot, bot) {
			return true
		}
	}
	return false
}

// stateSince reports whether the configured bot exposes one of the given
// comment states at/after since. The top summary is edited in place, so
// ObservedTime (UpdatedAt) decides which round the current body belongs to.
func stateSince(obs Observation, p Policy, since time.Time, kinds ...dialect.EventKind) bool {
	for _, ev := range obs.Events {
		if !sameBot(ev.Bot, p.Bot) || !notBefore(ev.ObservedTime(), since) {
			continue
		}
		for _, kind := range kinds {
			if ev.Kind == kind {
				return true
			}
		}
	}
	return false
}

type commandReply struct {
	commandID  int64
	commandAt  time.Time
	completion bool
}

// commandReplies folds the classified event stream (plus submitted reviews)
// into command→reply pairs, exactly as v2's reviewCommandReplies did.
func commandReplies(obs Observation, p Policy) []commandReply {
	type kind int
	const (
		kCommand kind = iota
		kAutoReply
		kReview
	)
	type event struct {
		kind kind
		at   time.Time
		id   int64
		ev   dialect.BotEvent
	}
	var events []event
	for _, ev := range obs.Events {
		switch {
		case ev.Kind == dialect.EvCommand:
			events = append(events, event{kind: kCommand, at: ev.PairTime(), id: ev.CommentID, ev: ev})
		case ev.AutoReply && sameBot(ev.Bot, p.Bot):
			events = append(events, event{kind: kAutoReply, at: ev.PairTime(), id: ev.CommentID, ev: ev})
		}
	}
	for _, review := range obs.Reviews {
		if !sameBot(review.Bot, p.Bot) || review.SubmittedAt.IsZero() {
			continue
		}
		events = append(events, event{kind: kReview, at: review.SubmittedAt, id: review.ReviewID})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].at.Equal(events[j].at) {
			return events[i].at.Before(events[j].at)
		}
		if events[i].kind != events[j].kind {
			return events[i].kind < events[j].kind
		}
		return events[i].id < events[j].id
	})

	var out []commandReply
	var pending []event
	for _, ev := range events {
		switch ev.kind {
		case kCommand:
			pending = append(pending, ev)
		case kReview:
			if len(pending) > 0 {
				pending = pending[1:]
			}
		case kAutoReply:
			if len(pending) == 0 {
				continue
			}
			cmd := pending[0]
			pending = pending[1:]
			out = append(out, commandReply{
				commandID:  cmd.id,
				commandAt:  cmd.at,
				completion: ev.ev.Kind == dialect.EvCompletion,
			})
		}
	}
	return out
}

// DoneExcept reports whether every gating bot EXCEPT the named one has review
// evidence — and at least one other bot gates at all. The vacuous case
// matters: with only the excluded bot required, a degraded round must never
// read as done, or a CodeRabbit rate-limit window with no Codex configured
// would let rounds complete with no review evidence whatsoever.
func DoneExcept(reviewedBy map[string]bool, except string) bool {
	norm := dialect.NormalizeBotName(except)
	others := 0
	for bot, reviewed := range reviewedBy {
		if bot == except || dialect.NormalizeBotName(bot) == norm {
			continue
		}
		if !reviewed {
			return false
		}
		others++
	}
	return others > 0
}

// DoneExceptWithEvidence is DoneExcept after applying independently established
// review evidence for one bot. It preserves every other gating bot, while also
// handling configured login spellings that differ only by the "[bot]" suffix.
func DoneExceptWithEvidence(reviewedBy map[string]bool, except, evidenceBot string) bool {
	withEvidence := make(map[string]bool, len(reviewedBy)+1)
	evidenceNorm := dialect.NormalizeBotName(evidenceBot)
	found := false
	for bot, reviewed := range reviewedBy {
		if dialect.NormalizeBotName(bot) == evidenceNorm {
			reviewed = true
			found = true
		}
		withEvidence[bot] = reviewed
	}
	if !found {
		withEvidence[evidenceBot] = true
	}
	return DoneExcept(withEvidence, except)
}

// needsBotReview reports whether login gates completion (has a ReviewedBy
// key) and its review hasn't been seen yet.
func needsBotReview(reviewedBy map[string]bool, login string) bool {
	norm := dialect.NormalizeBotName(login)
	for bot, reviewed := range reviewedBy {
		if bot == login || dialect.NormalizeBotName(bot) == norm {
			return !reviewed
		}
	}
	return false
}

func reviewedByBot(reviewedBy map[string]bool, login string) bool {
	norm := dialect.NormalizeBotName(login)
	for bot, reviewed := range reviewedBy {
		if reviewed && (bot == login || dialect.NormalizeBotName(bot) == norm) {
			return true
		}
	}
	return false
}
