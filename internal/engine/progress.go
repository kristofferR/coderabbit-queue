package engine

import (
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Outcome is how a fired/reviewing round moves. It replaces v2's
// inflightStatus (slot release) AND the wait sweep — one implementation.
type Outcome int

const (
	KeepWaiting    Outcome = iota
	OutComplete            // every required bot has evidence → completed
	OutReviewing           // bot acknowledged; release the slot, keep the round open
	OutRetry               // park until Transition.RetryAt (account block, timeout, failure)
	OutReleaseSlot         // reserved but never posted → back to queued
	OutAbandon             // PR closed/merged
)

// AccountBlock is an account-quota update observed from an EvRateLimited event.
type AccountBlock struct {
	Until          time.Time
	CommentID      int64
	CommentUpdated time.Time
}

type Transition struct {
	Outcome Outcome
	Reason  string
	RetryAt time.Time     // OutRetry: earliest re-fire for this head
	Blocked *AccountBlock // account-wide CodeRabbit quota block to record
}

// reserveTimeout mirrors v2: a reservation that never posted its command
// releases after 2 minutes.
const reserveTimeout = 2 * time.Minute

// Progress decides what happened to a reserved/fired/reviewing round. Ports
// v2's inflightStatus order — submitted review → account block → reaction →
// other bot comment → timeout — with two deliberate fixes: the in-progress
// and failed top-summary states now gate the daemon path too (v2 applied
// them only in feedback.go), and every retry carries a RetryAt cooldown
// (v2's timeout requeue carried none — the second #448 re-fire vector).
func Progress(r state.Round, q state.AccountQuota, obs Observation, now time.Time, p Policy) Transition {
	if !obs.Open {
		return Transition{Outcome: OutAbandon, Reason: "pr closed"}
	}
	if r.Phase == state.PhaseReserved {
		if r.ReservedAt != nil && now.Sub(*r.ReservedAt) > reserveTimeout {
			return Transition{Outcome: OutReleaseSlot, Reason: "reserved review was never posted"}
		}
		return Transition{Outcome: KeepWaiting, Reason: "reserving"}
	}
	if r.FiredAt == nil {
		return Transition{Outcome: KeepWaiting, Reason: "no fire recorded"}
	}
	firedAt := r.FiredAt.UTC()
	completion := Completion(r, obs, p)

	// A reviewing round past its wait deadline whose primary review already
	// stands: a gating co-bot (Codex) has gone silent too long, so give up on it —
	// the primary review stands, and re-firing a head the primary already reviewed
	// would spam. Checked before the review loop below, which would otherwise hold
	// a co-review wait open forever on the primary review's ack. A reviewing round
	// with NO primary review is deliberately left to the fall-through (KeepWaiting):
	// the loop bounds and times out its own wait (exit 2), so an expired deadline
	// never resets or re-fires the same head.
	//
	// The one round that fall-through cannot serve is a primary-UNAVAILABLE one:
	// no primary review is ever coming (a summary-only plan, or a skipped
	// review), so the condition above can never become true, and under
	// `autoreview` there is no Loop to enforce the deadline either. Such a round
	// would sit in `reviewing` forever — and because the daemon sweeps only the
	// OLDEST reviewing round per pump, it would also starve every reviewing round
	// behind it. Its deadline is the only thing that can end it.
	if r.Phase == state.PhaseReviewing && r.WaitDeadline != nil && !now.Before(r.WaitDeadline.UTC()) {
		if primaryReviewedHead(r, obs, p) {
			return Transition{Outcome: OutComplete, Reason: "co-review wait elapsed; primary review stands"}
		}
		if PrimaryReviewUnavailable(obs, p, r.Head) {
			return Transition{Outcome: OutComplete, Reason: "co-review wait elapsed; no primary review is coming for this head"}
		}
	}

	// An "already reviewed" ack is only trusted alongside real review
	// evidence; a review matching the round completes or hands off the wait.
	for _, review := range obs.Reviews {
		if !sameBot(review.Bot, p.Bot) || !reviewMatchesRound(review, r.Head, firedAt) {
			continue
		}
		if completion.Done {
			return Transition{Outcome: OutComplete, Reason: "review submitted"}
		}
		if r.Phase == state.PhaseReviewing {
			// Already acknowledged: re-emitting OutReviewing would write the same
			// state and re-sync the dashboard on every sweep of a silent co-bot wait.
			return Transition{Outcome: KeepWaiting, Reason: "reviewing; awaiting remaining bots"}
		}
		return Transition{Outcome: OutReviewing, Reason: "review submitted; awaiting remaining bots"}
	}

	// An account-quota block beats every ack: the fired command did not produce
	// a review.
	for _, ev := range obs.Events {
		if ev.Kind != dialect.EvRateLimited || !sameBot(ev.Bot, p.Bot) || ev.UpdatedAt.Before(firedAt) {
			continue
		}
		until := resolveBlockWindow(ev, q, now, p)
		return Transition{
			Outcome: OutRetry,
			Reason:  dialect.ReasonRateLimited,
			RetryAt: until,
			Blocked: &AccountBlock{Until: until, CommentID: ev.CommentID, CommentUpdated: ev.UpdatedAt},
		}
	}

	// The failed top-summary state: the review itself failed. v2's daemon path
	// treated this as a normal bot comment and released the slot with the wait
	// still pending; parking with a bounded cooldown retries it instead.
	if r.Phase == state.PhaseFired && stateSince(obs, p, firedAt, dialect.EvFailed) {
		return Transition{Outcome: OutRetry, Reason: "review failed", RetryAt: now.Add(p.retryBackoff())}
	}

	// Convergence and slot release answer different questions. A repository may
	// leave the primary out of its required set (required Codex only), and then
	// completion is done the moment that co-reviewer answers — while the
	// account-metered command this round posted is still unacknowledged.
	// Completing here would release the fire slot, letting the next PR spend the
	// shared allowance alongside a review that is still running, and would leave a
	// late account-block reply landing on a completed round nobody progresses. So
	// a round that spent the quota stays fired until the primary speaks (below) or
	// its in-flight timeout gives up on it; a co-only round posted no command of
	// its own and has nothing to wait for.
	if completion.Done && (r.Phase != state.PhaseFired || r.CoOnly) {
		return Transition{Outcome: OutComplete, Reason: "feedback complete"}
	}

	if r.Phase == state.PhaseFired {
		if reason, acked := primaryAck(r, obs, p, firedAt); acked {
			if completion.Done {
				return Transition{Outcome: OutComplete, Reason: "feedback complete"}
			}
			return Transition{Outcome: OutReviewing, Reason: reason}
		}
		if now.Sub(firedAt) > p.InflightTimeout {
			// Nothing more is owed: everything this repository gates on has
			// answered and the slot has been held for the whole in-flight window,
			// so the serialization the wait exists for is satisfied. Re-firing
			// would buy another metered review no configured reviewer asked for.
			if completion.Done {
				return Transition{Outcome: OutComplete, Reason: "feedback complete; primary never acknowledged"}
			}
			return Transition{Outcome: OutRetry, Reason: "in-flight timeout", RetryAt: now.Add(p.retryBackoff())}
		}
		return Transition{Outcome: KeepWaiting, Reason: "review in flight"}
	}

	// Reviewing: the slot is long released and the review is running. The
	// wall-clock wait deadline is the loop's concern (it times out its own wait),
	// not the daemon's — the daemon keeps waiting for real bot evidence rather
	// than re-firing a review that is still in progress.
	return Transition{Outcome: KeepWaiting, Reason: "reviewing"}
}

// primaryAck reports whether the primary has acknowledged this round's command,
// and how — the condition that releases the fire slot.
//
// A bare reaction acknowledges it; so does any other comment of the bot's in the
// round window — but an account-block/paused/already-reviewed notice is not an
// ack (v2), and neither is the in-progress summary of a PREVIOUS round edit...
// which it cannot be: UpdatedAt gates the window. An in-progress summary IS an
// ack that reviewing started.
func primaryAck(r state.Round, obs Observation, p Policy, firedAt time.Time) (string, bool) {
	if obs.Reacted {
		return "bot reacted", true
	}
	for _, ev := range obs.Events {
		if !sameBot(ev.Bot, p.Bot) || ev.CommentID == r.CommandID || ev.UpdatedAt.Before(firedAt) {
			continue
		}
		switch ev.Kind {
		case dialect.EvRateLimited, dialect.EvPaused, dialect.EvAlreadyReviewed:
			continue
		}
		return "bot responded", true
	}
	return "", false
}

// resolveBlockWindow ports v2's requeueInflight window logic: reuse the
// standing block when the SAME edited account-quota comment is re-observed
// (CodeRabbit edits one comment in place — a re-observation must not extend
// the window on every bounce), and fall back to a conservative fixed window
// when no "available in" duration parsed.
func resolveBlockWindow(ev dialect.BotEvent, q state.AccountQuota, now time.Time, p Policy) time.Time {
	until := ev.Window
	sameComment := ev.CommentID != 0 && ev.CommentID == q.RLCommentID
	if sameComment && q.BlockedUntil != nil && q.BlockedUntil.After(now) {
		until = q.BlockedUntil
	}
	if until == nil || !until.After(now) {
		t := now.Add(p.rateLimitFallback())
		return t
	}
	return until.UTC()
}

// primaryReviewedHead reports whether the configured primary bot has a submitted
// review whose commit prefixes the round's head — the review that stands when a
// co-review wait gives up on a silent co-bot.
func primaryReviewedHead(r state.Round, obs Observation, p Policy) bool {
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, p.Bot) && r.Head != "" && review.Commit != "" && strings.HasPrefix(review.Commit, r.Head) {
			return true
		}
	}
	return false
}

// reviewMatchesRound mirrors v2: a known head must match the review commit;
// submission time alone could otherwise let a delayed review of an older
// head complete the new one.
func reviewMatchesRound(review ReviewSeen, head string, firedAt time.Time) bool {
	if head != "" {
		return strings.HasPrefix(review.Commit, head)
	}
	return notBefore(review.SubmittedAt, firedAt)
}
