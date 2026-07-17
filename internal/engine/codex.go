package engine

import (
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// codexBot is the Codex GitHub app login the engine flips in ReviewedBy when
// Codex gates a round. The dialect owns the literal and the normalization
// (CodexBotLogin/IsCodexBot/HasCodexBot); this consumes the canonical constant.
const codexBot = dialect.CodexBotLogin

// roundCutoff is the round-window floor: the fire time (UTC), or zero when the
// round has not fired.
func roundCutoff(r state.Round) time.Time {
	if r.FiredAt != nil {
		return r.FiredAt.UTC()
	}
	return time.Time{}
}

// codexReviewedRound reports whether a submitted Codex review binds to this
// round: one whose commit prefixes the head, or — SHA-less — one submitted
// at/after the fire.
func codexReviewedRound(r state.Round, obs Observation, cutoff time.Time) bool {
	for _, review := range obs.Reviews {
		if !dialect.IsCodexBot(review.Bot) {
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

// codexCommentedRound reports whether Codex posted an actionable comment or a
// clean summary at/after the round's fire — the round-window evidence that means
// Codex is participating. Its notices (usage limits, acks) do not count.
func codexCommentedRound(obs Observation, cutoff time.Time) bool {
	for _, ev := range obs.Events {
		if dialect.IsCodexBot(ev.Bot) && ev.Kind == dialect.EvOther && notBefore(ev.ObservedTime(), cutoff) {
			return true
		}
		if ev.Kind == dialect.EvCodexClean && notBefore(ev.ObservedTime(), cutoff) {
			return true
		}
	}
	return false
}

// codexReviewedHead reports whether Codex has a submitted review whose commit
// prefixes the observed head — the "Codex already reviewed this head" fire guard.
func codexReviewedHead(obs Observation) bool {
	for _, review := range obs.Reviews {
		if dialect.IsCodexBot(review.Bot) && obs.Head != "" && review.Commit != "" && strings.HasPrefix(review.Commit, obs.Head) {
			return true
		}
	}
	return false
}

// CodexActiveThisRound reports whether Codex shows any activity bound to this
// round — a head review, a round-window comment/clean summary, or a current
// thumbs-up. observe() stores it on the Observation so the dynamic completion
// gate requires Codex when it participates without being configured-required.
func CodexActiveThisRound(r state.Round, obs Observation) bool {
	cutoff := roundCutoff(r)
	return codexReviewedRound(r, obs, cutoff) || codexCommentedRound(obs, cutoff) || obs.CodexThumbsUp
}

// CodexAutoActive reports whether Codex reviews this PR on its own right now: its
// most recent evidence — a submitted review or a clean summary — was not preceded
// by an `@codex review` command. When true, crq must never post the Codex command
// (Codex reviews unprompted). Only the LATEST evidence decides, so an old
// unprompted review from an epoch when auto-review was on no longer suppresses
// posting once a later commanded review lands; conversely a command posted before
// the latest evidence marks that evidence as commanded, not automatic.
func CodexAutoActive(obs Observation) bool {
	latest, prev, ok := latestCodexEvidence(obs)
	if !ok {
		return false
	}
	// The latest evidence is automatic unless a command plausibly triggered it:
	// one posted in (prev, latest]. A command older than the previous evidence
	// belongs to an earlier round and does not explain this review — otherwise a
	// single manual `@codex review` from three heads ago would suppress posting
	// forever even after Codex went back to reviewing on its own.
	return !codexCommandInWindow(obs, prev, latest)
}

// latestCodexEvidence returns the timestamps of the most recent and second-most
// recent Codex review-or-clean-summary events, and whether any exists. prev is
// zero when there is only one evidence item.
func latestCodexEvidence(obs Observation) (latest, prev time.Time, ok bool) {
	consider := func(at time.Time) {
		if at.IsZero() {
			return
		}
		switch {
		case !ok || at.After(latest):
			prev, latest, ok = latest, at, true
		case at.After(prev):
			prev = at
		}
	}
	for _, review := range obs.Reviews {
		if dialect.IsCodexBot(review.Bot) {
			consider(review.SubmittedAt)
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCodexClean {
			consider(ev.PairTime())
		}
	}
	return latest, prev, ok
}

// codexCommandInWindow reports whether an `@codex review` command was posted
// after `after` and at or before `atOrBefore`. A zero `after` means no lower
// bound (the latest evidence is also the first — any command up to it counts).
func codexCommandInWindow(obs Observation, after, atOrBefore time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind != dialect.EvCodexCommand {
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

// CodexCommandSince reports whether an `@codex review` command comment exists
// at/after since. The self-heal retry uses it (with the round's fire time) to
// tell a fired round whose Codex command is already on the PR from one whose
// Codex post failed.
func CodexCommandSince(obs Observation, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCodexCommand && notBefore(ev.PairTime(), since) {
			return true
		}
	}
	return false
}

// codexUsageLimitedSince reports whether Codex posted its usage-limit
// exhaustion notice at/after since — the round window it can no longer finish.
func codexUsageLimitedSince(obs Observation, since time.Time) bool {
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCodexUsageLimit && notBefore(ev.ObservedTime(), since) {
			return true
		}
	}
	return false
}

// DecideCodexPost reports whether crq should post its Codex review command while
// firing this round. crq only posts for a configured-required Codex that does
// not auto-review; if Codex reviews on its own, has already reviewed the head,
// has been asked already (round.CodexCommandID or a live command on the PR), or
// no command is configured, crq stays out of the way. commandPresent is supplied
// by the caller so the fire path (cutoff-filtered obs.CodexCommands) and the
// self-heal retry (a round-window CodexCommandSince scan) share this rule.
func DecideCodexPost(r state.Round, obs Observation, p Policy, commandPresent bool) bool {
	if r.CodexCommandID != 0 {
		return false
	}
	if strings.TrimSpace(p.CodexCommand) == "" {
		return false
	}
	if !dialect.HasCodexBot(p.RequiredBots) {
		return false
	}
	if obs.CodexAutoActive {
		return false
	}
	if commandPresent {
		return false
	}
	return !codexReviewedHead(obs)
}
