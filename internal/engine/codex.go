package engine

import (
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Codex-specific layer over the generic co-reviewer algebra in coreview.go:
// the thumbs-up quirk (a Codex +1 stands in for its review) lives here, and
// the pre-migration entry points wrap their generic counterparts keyed by the
// Codex login. The wrappers go away once crq iterates Policy.CoReviewers.

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

// codexPolicy synthesizes the Codex CoReviewerPolicy from the legacy Policy
// fields: trigger always iff Codex is configured-required (DecideCodexPost's
// historical "only when required" guard), never otherwise.
func codexPolicy(p Policy) CoReviewerPolicy {
	cp := CoReviewerPolicy{Login: codexBot, Command: p.CodexCommand, Trigger: TriggerNever}
	if dialect.HasCodexBot(p.RequiredBots) {
		cp.Trigger = TriggerAlways
	}
	return cp
}

func codexCutoff(r state.Round) time.Time { return coCutoff(r, codexBot) }

func codexReviewedRound(r state.Round, obs Observation, cutoff time.Time) bool {
	return coReviewedRound(r, obs, codexBot, cutoff)
}

func codexCommentedRound(obs Observation, cutoff time.Time) bool {
	return coCommentedRound(obs, codexBot, cutoff)
}

// codexReviewedHead is the "Codex already reviewed this head" fire guard.
func codexReviewedHead(obs Observation) bool { return coReviewedHead(obs, codexBot) }

// CodexActiveThisRound is CoActiveThisRound for Codex plus its thumbs-up
// quirk: a current +1 on the PR or the fired command counts as participation.
func CodexActiveThisRound(r state.Round, obs Observation) bool {
	return CoActiveThisRound(r, obs, codexBot) || obs.CodexThumbsUp
}

// CodexAutoActive reports whether Codex reviews this PR on its own right now.
func CodexAutoActive(obs Observation) bool { return CoAutoActive(obs, codexBot) }

// CodexCommandSince reports whether an `@codex review` command comment exists
// at/after since.
func CodexCommandSince(obs Observation, since time.Time) bool {
	return CoCommandSince(obs, codexBot, since)
}

// CodexOnlyEligible reports whether an account-blocked round may degrade to a
// Codex-only round.
func CodexOnlyEligible(r state.Round, obs Observation, blockedUntil *time.Time, now time.Time) bool {
	return CoOnlyEligible(r, obs, codexBot, blockedUntil, now)
}

func codexUsageLimitedSince(obs Observation, since time.Time) bool {
	return coUnableSince(obs, codexBot, since)
}

// DecideCodexPost reports whether crq should post its Codex review command
// while firing this round — DecideCoPost under the synthesized Codex policy
// (post at fire time, required-only, unless Codex auto-reviews).
func DecideCodexPost(r state.Round, obs Observation, p Policy, commandPresent bool) bool {
	return DecideCoPost(r, obs, codexPolicy(p), commandPresent, time.Time{}, time.Time{})
}
