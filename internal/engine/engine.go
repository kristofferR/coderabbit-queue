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

// TriggerMode is how crq may post a co-reviewer's trigger command.
type TriggerMode string

const (
	TriggerNever    TriggerMode = "never"    // crq never posts this bot's command
	TriggerSelfHeal TriggerMode = "selfheal" // post only when an active bot missed the head past a grace period
	TriggerAlways   TriggerMode = "always"   // post at fire time unless the bot auto-reviews
)

// CoReviewerPolicy is the configured stance toward one co-reviewer: whether
// and how crq triggers it. Required-ness stays solely RequiredBots membership.
type CoReviewerPolicy struct {
	Login   string
	Command string      // trigger comment crq posts ("" disables posting)
	Trigger TriggerMode // zero value ("") reads as never
	// SelfHealGrace is how long a selfheal trigger waits past its anchor for
	// the bot to show up on its own (<=0: a conservative default).
	SelfHealGrace time.Duration
}

func (cp CoReviewerPolicy) selfHealGrace() time.Duration {
	if cp.SelfHealGrace > 0 {
		return cp.SelfHealGrace
	}
	return 10 * time.Minute
}

// Policy carries the configured knobs the decisions depend on.
type Policy struct {
	Bot          string   // configured CodeRabbit login
	RequiredBots []string // bots that gate round completion

	// CoReviewers are the enabled co-reviewer bots with their trigger stances.
	CoReviewers []CoReviewerPolicy

	MinInterval       time.Duration // global pacing between fires
	InflightTimeout   time.Duration // fired round with no bot response at all
	RateLimitFallback time.Duration // block window when "available in" is unparseable
	RetryBackoff      time.Duration // cooldown after a non-rate-limit retry (timeout, failure)

	// RateLimitCoDegrade lets an account-blocked round degrade to a
	// co-reviewer-only round (post the triggers now, keep CodeRabbit queued
	// for the window) instead of waiting the block out. CRQ_RL_CO_DEGRADE.
	RateLimitCoDegrade bool
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

// CheckSeen is one classified check run owned by a co-reviewer. observe()
// fetches check runs per-ref for the observed head, so every CheckSeen is
// head-scoped by construction — a completed one IS head review evidence
// (Bugbot's clean rounds exist ONLY as a check run).
type CheckSeen struct {
	Bot         string // co-reviewer login owning the check
	Name        string
	Verdict     dialect.CheckVerdict
	CompletedAt time.Time
}

// CoSeen is one co-reviewer's slice of the Observation, keyed in
// Observation.Co by normalized login.
type CoSeen struct {
	// Commands are live adoptable trigger comments for this bot at the head
	// (cutoff-filtered by observe like Observation.Commands). Non-empty means
	// a trigger already exists, so crq must not post a duplicate.
	Commands []CommandSeen
	// AutoActive reports the bot reviews this PR on its own: its latest
	// review/clean-summary/check evidence was not preceded by a trigger
	// command. When true, crq never posts its command unprompted.
	AutoActive bool
	// ActiveThisRound reports activity bound to the current round (a head
	// review, a round-window comment/clean summary, or a head check run). It
	// drives the dynamic completion gate for non-required co-reviewers.
	ActiveThisRound bool
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
	// Reacted reports a configured-bot reaction on the round's fired command.
	Reacted bool
	// CodexThumbsUp reports a current Codex +1 on the PR or the fired command
	// (pre-fetched only when a Codex-gated completion needs it).
	CodexThumbsUp bool

	// Checks are the head's classified co-reviewer check runs.
	Checks []CheckSeen
	// Co carries each co-reviewer's per-bot observation slice, keyed by
	// normalized login.
	Co map[string]CoSeen
}

// CoSeenFor exposes one co-reviewer's observation slice to the apply layer
// (trigger adoption at fire time shares the engine's view of live commands).
func (o Observation) CoSeenFor(login string) CoSeen { return o.co(login) }

// co returns login's observation slice (zero value when unobserved).
func (o Observation) co(login string) CoSeen {
	return o.Co[dialect.NormalizeBotName(login)]
}

// SummaryOnlyPlan reports whether the configured bot has declared a
// summary-and-walkthrough-only plan on this PR (CodeRabbit Free on a private
// repo — the dialect owns the wording). It means the bot will NEVER submit a
// review here however often it is asked, so the round has exactly one honest
// resolution: run the co-reviewers and let them decide it.
//
// The declaration is an account fact, not a round fact: it lives in the top
// summary CodeRabbit edits in place, so no cutoff applies and any occurrence
// counts. It also self-heals — upgrade the plan and the notice stops shipping,
// so crq resumes firing on the next observation with no state to reset.
func SummaryOnlyPlan(obs Observation, p Policy) bool {
	for _, ev := range obs.Events {
		if ev.SummaryOnly && sameBot(ev.Bot, p.Bot) {
			return true
		}
	}
	return false
}

// ReviewSkippedHead reports whether the configured bot explicitly SKIPPED this
// head — too many files, no usage credits, an unsupported diff. Unlike a rate
// limit there is no window after which it clears: re-firing the same head buys
// the same refusal forever. Unlike a summary-only plan it is per-head, so it
// binds to the SHA the notice names and a reworked head (a split PR) fires
// normally again. A notice naming no SHA binds to whatever head is observed,
// which is the conservative reading for an in-place-edited comment.
func ReviewSkippedHead(obs Observation, p Policy, head string) bool {
	for _, ev := range obs.Events {
		if ev.Kind != dialect.EvSkipped || !sameBot(ev.Bot, p.Bot) {
			continue
		}
		if ev.SHA == "" || head == "" || dialect.SHAPrefixMatch(ev.SHA, head) {
			return true
		}
	}
	return false
}

// PrimaryReviewUnavailable reports that the configured bot will not produce a
// review for this head however long crq waits — its plan only summarizes, or it
// skipped this head outright. Both collapse to the same decision: stop waiting
// on a review that cannot arrive and let the co-reviewers resolve the round.
func PrimaryReviewUnavailable(obs Observation, p Policy, head string) bool {
	return SummaryOnlyPlan(obs, p) || ReviewSkippedHead(obs, p, head)
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
