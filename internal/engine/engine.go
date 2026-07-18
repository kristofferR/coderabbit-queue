// Package engine holds crq's pure decision logic: whether to fire a review
// command (fire.go), how a fired round progresses (progress.go), and when a
// round is complete (completion.go). Every function takes explicit inputs —
// a Round, an Observation, a clock value, a Policy — and performs no I/O, so
// the daemon and `crq loop` share ONE implementation of each decision and
// every rule is table-testable.
package engine

import (
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// Policy carries the configured knobs the decisions depend on.
type Policy struct {
	Bot          string   // configured CodeRabbit login
	RequiredBots []string // bots that gate round completion
	CodexCommand string   // Codex review trigger crq posts ("" disables Codex firing)

	MinInterval       time.Duration // global pacing between fires
	InflightTimeout   time.Duration // fired round with no bot response at all
	RateLimitFallback time.Duration // block window when "available in" is unparseable
	RetryBackoff      time.Duration // cooldown after a non-rate-limit retry (timeout, failure)
}

func (p Policy) rateLimitFallback() time.Duration {
	if p.RateLimitFallback > 0 {
		return p.RateLimitFallback
	}
	return 15 * time.Minute
}

func (p Policy) retryBackoff() time.Duration {
	if p.RetryBackoff > 0 {
		return p.RetryBackoff
	}
	return 5 * time.Minute
}

// ReviewSeen is one submitted bot review, reduced to what decisions need.
type ReviewSeen struct {
	Bot         string
	ReviewID    int64
	Commit      string // short OID ("" when GitHub omitted it)
	SubmittedAt time.Time
}

// CommandSeen is an adoptable review-command comment already on the PR.
// observe() applies the adoption cutoffs (requeue time, force-push time,
// already-answered checks) before it reaches the engine.
type CommandSeen struct {
	ID        int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Observation is everything the engine may know about one PR at one moment.
// crq's observe() builds it from GitHub exactly once per decision.
type Observation struct {
	Head    string // 9-char short head; "" when unreadable
	Open    bool
	Reviews []ReviewSeen
	Events  []dialect.BotEvent
	// Commands are adoptable trigger comments (cutoff-filtered by observe).
	Commands []CommandSeen
	// CodexCommands are adoptable Codex trigger comments present for the head
	// (cutoff-filtered by observe like Commands). A non-empty list means a live
	// `@codex review` already exists, so crq must not post a duplicate.
	CodexCommands []CommandSeen
	// Reacted reports a configured-bot reaction on the round's fired command.
	Reacted bool
	// CodexThumbsUp reports a current Codex +1 on the PR or the fired command
	// (pre-fetched only when a Codex-gated completion needs it).
	CodexThumbsUp bool
	// CodexAutoActive reports that Codex reviews this PR on its own: it has a
	// review or clean summary that no `@codex review` command preceded. When
	// true, crq never posts the Codex command — Codex will review unprompted.
	CodexAutoActive bool
	// CodexActiveThisRound reports Codex activity bound to the current round (a
	// head review, a round-window comment/clean summary, or a thumbs-up). It
	// drives the dynamic completion gate when Codex is not configured-required.
	CodexActiveThisRound bool
}

// notBefore mirrors v2: GitHub timestamps are second-granular, so a bot
// completion in the same second as the trigger must still count.
func notBefore(t, baseline time.Time) bool { return !t.Before(baseline) }

func sameBot(a, b string) bool {
	return dialect.NormalizeBotName(a) == dialect.NormalizeBotName(b)
}

// markReviewed flips the required-bot key that login matches, tolerating the
// "[bot]" suffix difference between REST and GraphQL logins.
func markReviewed(reviewedBy map[string]bool, login string) {
	norm := dialect.NormalizeBotName(login)
	for bot := range reviewedBy {
		if bot == login || dialect.NormalizeBotName(bot) == norm {
			reviewedBy[bot] = true
			return
		}
	}
}

func allReviewed(reviewedBy map[string]bool) bool {
	for _, ok := range reviewedBy {
		if !ok {
			return false
		}
	}
	return true
}
