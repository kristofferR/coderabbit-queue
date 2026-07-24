package engine

import (
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Co-reviewer evidence and gate algebra, keyed by login. These are the
// bot-shape-generic rules ("participated", "clean at SHA", "cannot finish",
// "was commanded") the Codex-specific helpers in codex.go now wrap; check
// runs count as evidence/activity alongside reviews and clean summaries
// because Bugbot's clean rounds exist ONLY as a check run.

// roundCoCommandID reads the trigger comment recorded for login this round,
// falling back to the legacy Codex fields for rounds built directly (tests)
// rather than loaded through state.Normalize's fold.
func roundCoCommandID(r state.Round, login string) int64 {
	if c := r.Co(login); c.CommandID != 0 {
		return c.CommandID
	}
	if dialect.IsCodexBot(login) {
		return r.CodexCommandID
	}
	return 0
}

func roundCoCommandedAt(r state.Round, login string) *time.Time {
	if c := r.Co(login); c.CommandedAt != nil {
		return c.CommandedAt
	}
	if dialect.IsCodexBot(login) {
		return r.CodexCommandedAt
	}
	return nil
}

// eventConcerns reports whether a classified event concerns the co-reviewer
// login: by the classifier's For attribution when present, otherwise by
// author. A For-less co-command is attributed to Codex — the only bot whose
// commands existed before For did (migration shim for hand-built events).
func eventConcerns(ev dialect.BotEvent, login string) bool {
	if ev.For != "" {
		return sameBot(ev.For, login)
	}
	if ev.Kind == dialect.EvCoCommand {
		return dialect.IsCodexBot(login)
	}
	return sameBot(ev.Bot, login)
}

// coCutoff is the evidence floor for one co-reviewer: evidence produced in
// response to crq's own trigger command binds from the command time, which
// can precede a deferred CodeRabbit fire (the command posts while the round
// is still queued behind a rate-limit window or busy slot).
func coCutoff(r state.Round, login string) time.Time {
	cut := roundCutoff(r)
	if at := roundCoCommandedAt(r, login); at != nil {
		t := at.UTC()
		if cut.IsZero() || t.Before(cut) {
			return t
		}
	}
	return cut
}

// coChecks yields login's check runs from the observation.
func coChecks(obs Observation, login string) []CheckSeen {
	var out []CheckSeen
	for _, c := range obs.Checks {
		if sameBot(c.Bot, login) {
			out = append(out, c)
		}
	}
	return out
}

// coCheckAny reports whether ANY check run of login's exists for the head,
// including an in-progress one — a running check will deliver evidence, so
// posting a trigger alongside it would double-review.
func coCheckAny(obs Observation, login string) bool {
	return len(coChecks(obs, login)) > 0
}

// coCheckReviewedAt reports the newest COMPLETED check verdict for the head
// (Done or DoneClean — findings, if any, still gate via threads).
func coCheckReviewedAt(obs Observation, login string) (time.Time, bool) {
	var latest time.Time
	matched := false
	for _, c := range coChecks(obs, login) {
		if c.Verdict == dialect.CheckDone || c.Verdict == dialect.CheckDoneClean {
			matched = true
			if c.CompletedAt.After(latest) {
				latest = c.CompletedAt
			}
		}
	}
	return latest, matched
}

// coReviewedRound reports whether a submitted review by login binds to this
// round: one whose commit prefixes the head, or — SHA-less — one submitted
// at/after the fire.
func coReviewedRound(r state.Round, obs Observation, login string, cutoff time.Time) bool {
	for _, review := range obs.Reviews {
		if !sameBot(review.Bot, login) {
			continue
		}
		if r.Head != "" && review.Commit != "" && strings.HasPrefix(review.Commit, r.Head) {
			return true
		}
		if review.Commit == "" && !review.SubmittedAt.IsZero() && notBefore(review.SubmittedAt, cutoff) {
			return true
		}
	}
	return false
}

// coCommentedRound reports whether login posted an actionable comment or a
// clean summary at/after the round's fire — the round-window evidence that
// means it is participating. Its notices (unable, acks, verdicts) do not
// count.
func coCommentedRound(obs Observation, login string, cutoff time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvOther && sameBot(ev.Bot, login) && notBefore(ev.ObservedTime(), cutoff) {
			return true
		}
		if ev.Kind == dialect.EvCoClean && eventConcerns(ev, login) && notBefore(ev.ObservedTime(), cutoff) {
			return true
		}
	}
	return false
}

// coReviewedHeadAt reports the newest verdict by login explicitly bound to
// the observed head: a submitted review, a clean summary naming that SHA, or
// a completed check run (head-scoped by construction). The timestamp is the
// evidence floor used to ignore older unable notices.
func coReviewedHeadAt(obs Observation, login string) (time.Time, bool) {
	var latest time.Time
	matched := false
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, login) && obs.Head != "" && review.Commit != "" && strings.HasPrefix(review.Commit, obs.Head) {
			matched = true
			if review.SubmittedAt.After(latest) {
				latest = review.SubmittedAt
			}
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoClean && eventConcerns(ev, login) && obs.Head != "" && dialect.SHAPrefixMatch(ev.SHA, obs.Head) {
			matched = true
			if at := ev.ObservedTime(); at.After(latest) {
				latest = at
			}
		}
	}
	if at, ok := coCheckReviewedAt(obs, login); ok {
		matched = true
		if at.After(latest) {
			latest = at
		}
	}
	return latest, matched
}

// coReviewedHead is the "login already reviewed this head" fire guard.
func coReviewedHead(obs Observation, login string) bool {
	_, matched := coReviewedHeadAt(obs, login)
	return matched
}

// CoActiveThisRound reports whether login shows activity bound to this round —
// a head review, a round-window comment/clean summary, a per-round verdict
// comment, or any head check run. observe() stores it on the Observation so
// the dynamic completion gate requires the bot when it participates without
// being configured-required. (Codex's thumbs-up quirk is layered on by
// CodexActiveThisRound.)
func CoActiveThisRound(r state.Round, obs Observation, login string) bool {
	cutoff := coCutoff(r, login)
	return coReviewedRound(r, obs, login, cutoff) || coCommentedRound(obs, login, cutoff) ||
		coVerdictSince(obs, login, cutoff) || coCheckAny(obs, login)
}

// coVerdictSince reports a per-round verdict comment (Macroscope's
// Approvability) by login at/after since. A verdict is round PARTICIPATION —
// it engages the dynamic gate so the bot's review is waited for — but never
// completion evidence: only reviews, clean summaries, and completed checks
// mark a bot reviewed.
func coVerdictSince(obs Observation, login string, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoVerdict && eventConcerns(ev, login) && notBefore(ev.ObservedTime(), since) {
			return true
		}
	}
	return false
}

// CoAutoActive reports whether login reviews this PR on its own right now: its
// most recent evidence — a submitted review, a clean summary, or a completed
// check — was not preceded by its trigger command. When true, crq must never
// post the trigger (the bot reviews unprompted). Only the LATEST evidence
// decides, so an old unprompted review from an epoch when auto-review was on
// no longer suppresses posting once a later commanded review lands;
// conversely a command posted before the latest evidence marks that evidence
// as commanded, not automatic.
func CoAutoActive(obs Observation, login string) bool {
	latest, prev, ok := latestCoEvidence(obs, login)
	if !ok {
		return false
	}
	// The latest evidence is automatic unless a command plausibly triggered it:
	// one posted in (prev, latest]. A command older than the previous evidence
	// belongs to an earlier round and does not explain this review — otherwise a
	// single manual trigger from three heads ago would suppress posting forever
	// even after the bot went back to reviewing on its own.
	return !coCommandInWindow(obs, login, prev, latest)
}

// latestCoEvidence returns the timestamps of the most recent and second-most
// recent review-or-clean-summary-or-completed-check events for login, and
// whether any exists. prev is zero when there is only one evidence item.
func latestCoEvidence(obs Observation, login string) (latest, prev time.Time, ok bool) {
	consider := func(at time.Time) {
		if at.IsZero() {
			return
		}
		switch {
		case !ok || at.After(latest):
			prev, latest, ok = latest, at, true
		case at.Equal(latest):
			// prev must stay strictly older: co-timestamped evidence (a review and
			// its clean summary in the same second) must not close the command
			// window to a point, or a command at that instant reads as absent and
			// a commanded review misclassifies as automatic.
		case at.After(prev):
			prev = at
		}
	}
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, login) {
			consider(review.SubmittedAt)
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoClean && eventConcerns(ev, login) {
			consider(ev.PairTime())
		}
	}
	for _, c := range coChecks(obs, login) {
		if c.Verdict == dialect.CheckDone || c.Verdict == dialect.CheckDoneClean {
			consider(c.CompletedAt)
		}
	}
	return latest, prev, ok
}

// coCommandInWindow reports whether login's trigger command was posted after
// `after` and at or before `atOrBefore`. A zero `after` means no lower bound
// (the latest evidence is also the first — any command up to it counts).
func coCommandInWindow(obs Observation, login string, after, atOrBefore time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind != dialect.EvCoCommand || !eventConcerns(ev, login) {
			continue
		}
		at := ev.PairTime()
		if at.After(atOrBefore) {
			continue
		}
		if !after.IsZero() && !at.After(after) {
			continue
		}
		return true
	}
	return false
}

// CoCommandSince reports whether login's trigger command comment exists
// at/after since. The self-heal retry uses it (with the round's fire time) to
// tell a fired round whose command is already on the PR from one whose post
// failed.
func CoCommandSince(obs Observation, login string, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoCommand && eventConcerns(ev, login) && notBefore(ev.PairTime(), since) {
			return true
		}
	}
	return false
}

// coUnableSince reports whether login declared it cannot finish this round
// (EvCoUnable — Codex's usage-limit exhaustion) at/after since.
func coUnableSince(obs Observation, login string, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoUnable && eventConcerns(ev, login) && notBefore(ev.ObservedTime(), since) {
			return true
		}
	}
	return false
}

// CoOnlyEligible reports whether an account-blocked round may degrade to a
// co-reviewer-only round: the block is live AND login has evidence bound to
// THIS work — a review of the current head, or round-window activity anchored
// by the fire or by crq's own (possibly pre-fire) trigger command — AND no
// unable notice inside that same window. Auto-activity on older heads,
// configuration, or a live unanswered command merely predict evidence;
// degradation waits for the evidence itself, since before the bot responds
// there is nothing to return early anyway, and marking a round deferred stops
// the loop from extending its deadline over the block.
func CoOnlyEligible(r state.Round, obs Observation, login string, blockedUntil *time.Time, now time.Time) bool {
	if blockedUntil == nil || !blockedUntil.After(now) {
		return false
	}
	headEvidenceAt, headReviewed := coReviewedHeadAt(obs, login)
	anchored := r.FiredAt != nil || roundCoCommandedAt(r, login) != nil
	if !headReviewed && !(anchored && obs.co(login).ActiveThisRound) {
		return false
	}
	// The unable floor is the evidence window. For an unfired, uncommanded
	// round the cutoff is zero — floor it at the head review that qualified
	// the round instead, or any old exhaustion notice still on the PR would
	// suppress the degrade until the window expires.
	floor := coCutoff(r, login)
	if floor.IsZero() {
		floor = headEvidenceAt
	}
	return !coUnableSince(obs, login, floor)
}

// DecideCoPost reports whether crq should post login's trigger command for
// this round. Common guards regardless of mode: a command is configured, the
// round has not already commanded this bot, no live command sits on the PR,
// the bot has not reviewed the head, and no check run of its exists for the
// head (including in-progress — a running check will deliver evidence).
//
// Modes: never — false. always — post unless the bot auto-reviews (today's
// Codex behavior; required-ness lives in the config default that picks the
// mode). selfheal — post only for a bot observed active that missed the head,
// once the anchor (the round's fire) is older than the grace period; the
// caller passes anchor and now so the fire path and the sweep share the rule.
func DecideCoPost(r state.Round, obs Observation, cp CoReviewerPolicy, commandPresent bool, anchor, now time.Time) bool {
	if roundCoCommandID(r, cp.Login) != 0 {
		return false
	}
	if strings.TrimSpace(cp.Command) == "" {
		return false
	}
	if commandPresent {
		return false
	}
	if coReviewedHead(obs, cp.Login) {
		return false
	}
	if coCheckAny(obs, cp.Login) {
		return false
	}
	switch cp.Trigger {
	case TriggerAlways:
		return !obs.co(cp.Login).AutoActive
	case TriggerSelfHeal:
		co := obs.co(cp.Login)
		if !co.AutoActive && !co.ActiveThisRound {
			return false
		}
		if anchor.IsZero() {
			return false
		}
		return now.Sub(anchor) >= cp.selfHealGrace()
	default:
		return false
	}
}
