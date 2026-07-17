package engine

import (
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// FireVerdict is what Pump should do with a fire-eligible round. Nothing
// outside DecideFire may conclude "post the review command" — this is the
// single owner of that decision.
type FireVerdict int

const (
	FireNo           FireVerdict = iota // skip this pass (Reason says why)
	FirePost                            // reserve the slot and post the command
	FireAdopt                           // a command is already on the PR — adopt it
	FireDedupe                          // bot already reviewed this head — complete without firing
	FireCodexOnly                       // CodeRabbit reviewed the head but a required Codex still must — post only the Codex command
	FireCoReviewWait                    // CodeRabbit reviewed the head; a gating co-bot has not — wait for it, bounded, without posting or holding the slot
	FireSupersede                       // observed head differs — supersede the round first
	FireDrop                            // PR closed/merged — abandon the round
)

type FireDecision struct {
	Verdict FireVerdict
	Reason  string
	// Adopt fields identify the existing command comment (FireAdopt).
	AdoptCommandID int64
	AdoptAt        time.Time
	// PostCodex asks the apply layer to post the Codex review command alongside
	// the CodeRabbit one (FirePost/FireAdopt). See DecideCodexPost.
	PostCodex bool
}

// Global is the cross-PR state a fire decision needs.
type Global struct {
	SlotFree     bool
	BlockedUntil *time.Time // CodeRabbit account quota block
	LastFired    *time.Time // global pacing anchor
}

// DecideFire consolidates v2's scattered fire guards, in order: PR open →
// head readable → head current → round eligible (phase + RetryAt cooldown) →
// slot free → account quota → global pacing → not already reviewed → adopt
// or post.
func DecideFire(g Global, r state.Round, obs Observation, now time.Time, p Policy) FireDecision {
	if !obs.Open {
		return FireDecision{Verdict: FireDrop, Reason: "pr closed"}
	}
	if obs.Head == "" {
		return FireDecision{Verdict: FireNo, Reason: "could not read head"}
	}
	if r.Head != obs.Head {
		return FireDecision{Verdict: FireSupersede, Reason: "head moved to " + obs.Head}
	}
	if !r.FireEligible(now) {
		reason := "round is " + string(r.Phase)
		if r.Phase == state.PhaseAwaitingRetry && r.RetryAt != nil {
			reason = "cooling down until " + r.RetryAt.UTC().Format(time.RFC3339)
		}
		return FireDecision{Verdict: FireNo, Reason: reason}
	}
	if !g.SlotFree {
		return FireDecision{Verdict: FireNo, Reason: "fire slot busy"}
	}
	if g.BlockedUntil != nil && g.BlockedUntil.After(now) {
		return FireDecision{Verdict: FireNo, Reason: "account blocked until " + g.BlockedUntil.UTC().Format(time.RFC3339)}
	}
	if g.LastFired != nil && now.Sub(*g.LastFired) < p.MinInterval {
		return FireDecision{Verdict: FireNo, Reason: "min interval"}
	}
	// Belt-and-braces live check: even with a fresh round, never fire at a
	// head the bot has already reviewed (e.g. state was reinitialized). But a
	// CodeRabbit review does not finish a round that a required Codex still
	// gates — command (or wait for) Codex instead of deduping it away.
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, p.Bot) && review.Commit != "" && strings.HasPrefix(review.Commit, obs.Head) {
			return codexAwareDedupe(r, obs, p)
		}
	}
	// crq posts the Codex command in the same fire step for a configured-required
	// Codex with no auto-review and no existing command for this head.
	postCodex := DecideCodexPost(r, obs, p, len(obs.CodexCommands) > 0)
	// Adopt the newest already-posted command instead of posting a duplicate.
	// observe() has already applied the adoption cutoffs (LastAttemptAt,
	// force-push, already-answered).
	var newest *CommandSeen
	for i := range obs.Commands {
		c := obs.Commands[i]
		if newest == nil || c.CreatedAt.After(newest.CreatedAt) {
			newest = &c
		}
	}
	if newest != nil {
		at := newest.CreatedAt
		if at.IsZero() {
			at = newest.UpdatedAt
		}
		return FireDecision{Verdict: FireAdopt, Reason: "review command already posted", AdoptCommandID: newest.ID, AdoptAt: at, PostCodex: postCodex}
	}
	return FireDecision{Verdict: FirePost, PostCodex: postCodex}
}

// codexAwareDedupe resolves what to do when CodeRabbit already reviewed the head.
// If no gating Codex is still outstanding, the round is genuinely done (FireDedupe).
// If a required-or-auto-active Codex has no review of this head yet, the round is
// not done: post only the Codex command when crq may (FireCodexOnly). When crq may
// not post but Codex will still produce evidence on its own — it auto-reviews, or a
// command is already on the PR awaiting its answer — wait for it, bounded, without
// posting or holding the slot (FireCoReviewWait); leaving the round queued with no
// deadline is the bug that hangs the loop forever. Only when Codex gates purely by
// configuration with no way to obtain its review (no command configured/on the PR
// and no auto-review) fall back to completing on CodeRabbit's review; the feedback
// gate then surfaces Codex as still pending rather than the round wedging in an
// un-timed fire loop. Completion counts the existing CodeRabbit review, so a
// FireCodexOnly round waits on Codex alone.
func codexAwareDedupe(r state.Round, obs Observation, p Policy) FireDecision {
	codexGates := dialect.HasCodexBot(p.RequiredBots) || obs.CodexAutoActive
	if !codexGates || codexReviewedHead(obs) {
		return FireDecision{Verdict: FireDedupe, Reason: "bot already reviewed head"}
	}
	if DecideCodexPost(r, obs, p, len(obs.CodexCommands) > 0) {
		return FireDecision{Verdict: FireCodexOnly, Reason: "coderabbit reviewed head; codex still required"}
	}
	if obs.CodexAutoActive || len(obs.CodexCommands) > 0 || r.CodexCommandID != 0 {
		return FireDecision{Verdict: FireCoReviewWait, Reason: "awaiting codex co-review"}
	}
	return FireDecision{Verdict: FireDedupe, Reason: "bot already reviewed head"}
}
