package engine

import (
	"sort"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// ActionKind is the single instruction a caller of `crq next` executes. It is a
// CLOSED set: an agent driving a review loop reads exactly this field and does
// exactly what it says, so every judgement call that used to be improvised —
// how long to sleep, whether the head may move, whether the round is finished —
// is answered here instead of at the call site.
type ActionKind string

const (
	// ActionFix: actionable findings exist for this head. Fix them, validate,
	// then resolve or decline each thread.
	ActionFix ActionKind = "fix"
	// ActionHold: the caller has work to land but a required reviewer has not
	// answered for this head. Moving the head now would restart that review, so
	// hold it and re-check at Action.At.
	ActionHold ActionKind = "hold"
	// ActionPush: the head is released — commit and push the accumulated fixes.
	ActionPush ActionKind = "push"
	// ActionWait: nothing to do until Action.At.
	ActionWait ActionKind = "wait"
	// ActionDone: every required reviewer answered and no findings remain.
	ActionDone ActionKind = "done"
	// ActionBlocked: the loop cannot proceed without a human (PR closed).
	ActionBlocked ActionKind = "blocked"
)

// Action is the answer NextAction produces.
type Action struct {
	Kind   ActionKind
	Reason string
	// At is when the caller should call again; set for hold and wait, and
	// always strictly in the future (see NextInput.MinDelay).
	At time.Time
	// Pending lists the required bots with no review evidence for this head,
	// in the caller's configured order.
	Pending []string
	// Findings carries the actionable feedback for ActionFix.
	Findings []dialect.Finding
}

// NextInput is everything NextAction decides from. It is a struct rather than a
// long parameter list because the decision genuinely needs the whole picture:
// the round, what was observed, what the findings layer extracted, and the
// caller's own local state.
type NextInput struct {
	Round      state.Round
	Obs        Observation
	Completion CompletionStatus
	// Findings is the actionable feedback the crq layer extracted from the same
	// observation, already filtered to what can still be acted on.
	Findings []dialect.Finding
	Global   Global
	// Primary is the configured primary reviewer's login — the one whose review
	// spends account quota. It is the only bot an account block can delay, so
	// both the rate-limit degrade and the recheck arithmetic need to tell it
	// apart from the co-reviewers.
	Primary string
	// LocalWork reports that the caller holds changes the PR head does not have
	// yet (uncommitted, or committed but unpushed). It is what separates "push
	// your fixes" from "nothing left to do".
	LocalWork bool
	// Deferred marks a CodeRabbit rate-limit degrade: the co-reviewers answered
	// and CodeRabbit's review is still owed, firing at DeferredUntil.
	Deferred      bool
	DeferredUntil *time.Time
	// MinDelay is the floor for Action.At — the caller's poll interval. It makes
	// a hot loop unrepresentable: every wait is at least this long.
	MinDelay time.Duration
}

const defaultMinDelay = 15 * time.Second

func (in NextInput) minDelay() time.Duration {
	if in.MinDelay > 0 {
		return in.MinDelay
	}
	return defaultMinDelay
}

// NextAction reduces the whole review protocol to one instruction.
//
// The order encodes the rules agents get wrong when left to their own devices:
// findings are drained before anything else, the head is held while any
// required reviewer is still pending, and a CodeRabbit rate-limit degrade
// releases the head instead of stalling on it (the queued review fires on the
// new head by itself). Convergence is last, so "done" can only mean every
// required bot answered AND nothing is left to land.
func NextAction(in NextInput, now time.Time) Action {
	if !in.Obs.Open {
		return Action{Kind: ActionBlocked, Reason: "pr closed"}
	}
	if in.Obs.Head == "" {
		// A transient read failure, not a terminal state — come back shortly.
		return Action{Kind: ActionWait, Reason: "could not read head", At: in.nextCheck(now, nil, nil)}
	}

	pending := pendingBots(in.Completion)

	// 1. Findings first. An agent that starts a new review round on top of
	//    unresolved feedback burns account quota to be told the same thing.
	if blocking := BlockingFindings(in.Findings, in.Obs.Head); len(blocking) > 0 {
		return Action{
			Kind:     ActionFix,
			Reason:   "actionable findings for this head",
			Pending:  pending,
			Findings: blocking,
		}
	}

	// 2. A rate-limit degrade releases the head deliberately. The primary's
	//    review is still owed, but it is queued and will fire against whatever
	//    head exists when the window opens — so holding the current head buys
	//    nothing and costs a whole window. This is checked BEFORE the generic
	//    hold below, which would otherwise stall the loop for the full block.
	//
	//    "Released" means released from the PRIMARY only. Every other required
	//    reviewer still gates the head exactly as it always does, so the degrade
	//    uses the same DoneExceptWithEvidence rule Feedback applies to
	//    convergence — otherwise a degraded round pushes out from under a Bugbot
	//    or Macroscope review that is still running.
	if in.Deferred {
		releasedByDegrade := DoneExceptWithEvidence(in.Completion.ReviewedBy, in.Primary, dialect.CodexBotLogin)
		switch {
		case in.LocalWork && releasedByDegrade:
			return Action{
				Kind:    ActionPush,
				Reason:  "co-reviewers answered; primary review deferred and will fire on the new head",
				Pending: pending,
			}
		case in.LocalWork:
			return Action{
				Kind:    ActionHold,
				Reason:  "do not push: the primary review is deferred but another required reviewer has not answered for this head",
				At:      in.nextCheck(now, in.DeferredUntil, pending),
				Pending: pending,
			}
		}
		return Action{
			Kind:    ActionWait,
			Reason:  "primary review deferred while the account is rate-limited",
			At:      in.nextCheck(now, in.DeferredUntil, pending),
			Pending: pending,
		}
	}

	// 3. Required reviewers still pending: the head must not move. Resolving a
	//    thread does not restart a review; pushing does.
	if !in.Completion.Done {
		kind, reason := ActionWait, "awaiting review"
		if in.LocalWork {
			kind, reason = ActionHold, "do not push: a required reviewer has not answered for this head"
		}
		return Action{Kind: kind, Reason: reason, At: in.nextCheck(now, nil, pending), Pending: pending}
	}

	// 4. Everything answered. Land the work, or report convergence.
	if in.LocalWork {
		return Action{Kind: ActionPush, Reason: "all required reviewers answered on this head"}
	}
	return Action{Kind: ActionDone, Reason: "converged: no findings and every required reviewer answered"}
}

// nextCheck is when the caller should call again. It is never sooner than
// MinDelay (so a caller cannot hot-loop) and never sooner than a gate that
// definitely prevents progress — the account-quota window and this round's own
// retry cooldown, both of which DecideFire enforces anyway.
//
// Two things narrow those gates, and both exist because sleeping through
// feedback is worse than one extra call:
//
//   - They only apply to a round still waiting to fire. A fired or reviewing
//     round's answer can land at any moment.
//   - They only apply when the primary is the ONLY reviewer still owed. Neither
//     the account window nor this round's fire cooldown has any hold over a
//     co-reviewer, so a round waiting on one must keep the short cadence — a
//     degraded round otherwise sleeps for the whole block and misses the very
//     co-reviewer findings the degrade exists to deliver.
func (in NextInput) nextCheck(now time.Time, extra *time.Time, pending []string) time.Time {
	at := now.Add(in.minDelay()).UTC()
	if !in.onlyPrimaryPending(pending) {
		return at
	}
	gate := func(t *time.Time) {
		if t != nil && t.After(at) {
			at = t.UTC()
		}
	}
	gate(extra)
	switch in.Round.Phase {
	case state.PhaseQueued, state.PhaseAwaitingRetry:
		gate(in.Global.BlockedUntil)
		if in.Round.Phase == state.PhaseAwaitingRetry {
			gate(in.Round.RetryAt)
		}
	}
	return at
}

// onlyPrimaryPending reports whether every reviewer still owed for this head is
// the primary. An empty pending set counts: there is nobody a short cadence
// would catch.
func (in NextInput) onlyPrimaryPending(pending []string) bool {
	primary := dialect.NormalizeBotName(in.Primary)
	for _, bot := range pending {
		if dialect.NormalizeBotName(bot) != primary {
			return false
		}
	}
	return true
}

// pendingBots lists the required bots with no review evidence yet, sorted for a
// stable answer.
func pendingBots(c CompletionStatus) []string {
	var out []string
	for bot, reviewed := range c.ReviewedBy {
		if !reviewed {
			out = append(out, bot)
		}
	}
	sort.Strings(out)
	return out
}
