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
	FireCoOnly                          // CodeRabbit reviewed the head but a gating co-reviewer still must — post only its trigger
	FireCoReviewWait                    // CodeRabbit reviewed the head; a gating co-bot has not — wait for it, bounded, without posting or holding the slot
	FireCoDeferred                      // account blocked — post only co-reviewer triggers now; the round stays queued so CodeRabbit fires when the window opens
	FireSupersede                       // observed head differs — supersede the round first
	FireDrop                            // PR closed/merged — abandon the round
)

// Deprecated: Codex-named aliases for the generic co-reviewer verdicts, kept
// while crq migrates.
const (
	FireCodexOnly     = FireCoOnly
	FireCodexDeferred = FireCoDeferred
)

type FireDecision struct {
	Verdict FireVerdict
	Reason  string
	// Adopt fields identify the existing command comment (FireAdopt), or the
	// adopted Codex command on the legacy FireCoDeferred adopt path.
	AdoptCommandID int64
	AdoptAt        time.Time
	// PostCo lists the co-reviewer logins whose trigger commands the apply
	// layer posts alongside this verdict (FirePost/FireAdopt/FireCoOnly/
	// FireCoDeferred). See DecideCoPost.
	PostCo []string
	// AdoptCo identifies live co-reviewer trigger comments to record as this
	// round's command anchors instead of posting duplicates (FireCoDeferred).
	AdoptCo map[string]CommandSeen
	// PostCodex mirrors "Codex ∈ PostCo".
	//
	// Deprecated: consume PostCo instead.
	PostCodex bool
}

// Global is the cross-PR state a fire decision needs.
type Global struct {
	SlotFree     bool
	BlockedUntil *time.Time // CodeRabbit account quota block
	LastFired    *time.Time // global pacing anchor
}

// coReviewers resolves the effective co-reviewer policies: the explicit list
// when set, otherwise the one synthesized Codex entry (trigger always iff
// configured-required) that preserves the pre-registry behavior.
func (p Policy) coReviewers() []CoReviewerPolicy {
	if p.CoReviewers != nil {
		return p.CoReviewers
	}
	return []CoReviewerPolicy{codexPolicy(p)}
}

// CoReviewerPolicies exposes the effective co-reviewer list to the apply
// layer (self-heal sweeps, trigger posting), which shares DecideCoPost with
// the fire path and so must iterate the same resolved entries.
func (p Policy) CoReviewerPolicies() []CoReviewerPolicy { return p.coReviewers() }

// CoReviewedHead reports whether login already has head review evidence (a
// head review, a SHA-matched clean summary, or a completed check run) — the
// feedback layer surfaces it as per-bot status.
func CoReviewedHead(obs Observation, login string) bool { return coReviewedHead(obs, login) }

// requiredBot reports whether login is in RequiredBots (normalized).
func requiredBot(p Policy, login string) bool {
	norm := dialect.NormalizeBotName(login)
	for _, bot := range p.RequiredBots {
		if dialect.NormalizeBotName(strings.TrimSpace(bot)) == norm {
			return true
		}
	}
	return false
}

// decideCoPosts collects the co-reviewer logins whose trigger crq should post
// while firing this round. Fire-time posting is the always-mode path; a
// selfheal trigger anchors on the fire and so never posts before it.
func decideCoPosts(r state.Round, obs Observation, p Policy, now time.Time) []string {
	var out []string
	for _, cp := range p.coReviewers() {
		if DecideCoPost(r, obs, cp, len(obs.co(cp.Login).Commands) > 0, time.Time{}, now) {
			out = append(out, cp.Login)
		}
	}
	return out
}

func hasCodexLogin(logins []string) bool {
	for _, login := range logins {
		if dialect.IsCodexBot(login) {
			return true
		}
	}
	return false
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
	reviewedHead := false
	for _, review := range obs.Reviews {
		if sameBot(review.Bot, p.Bot) && review.Commit != "" && strings.HasPrefix(review.Commit, obs.Head) {
			reviewedHead = true
			break
		}
	}
	if !g.SlotFree {
		// Co-reviewers need no fire slot: a round parked behind another PR's
		// in-flight review can start its co-reviewer rounds immediately. The
		// round stays queued and CodeRabbit fires once the slot frees, with the
		// recorded command ids preventing duplicate posts. NOT for a head
		// CodeRabbit already reviewed — that round belongs to the dedupe
		// resolution below once the slot frees (a queued round a co-bot answers
		// clean cannot complete, so deferring it here could wedge the wait).
		if !reviewedHead {
			if d, ok := decideCoDeferred(r, obs, p, now, "fire slot busy"); ok {
				return d
			}
		}
		return FireDecision{Verdict: FireNo, Reason: "fire slot busy"}
	}
	// Belt-and-braces live check: even with a fresh round, never fire at a
	// head the bot has already reviewed (e.g. state was reinitialized). But a
	// CodeRabbit review does not finish a round that a gating co-reviewer still
	// must speak on — command (or wait for) it instead of deduping it away.
	// This resolution runs BEFORE the account-block and pacing gates: none of
	// its verdicts spend CodeRabbit quota (dedupe completes, FireCoOnly posts
	// only co-reviewer triggers, a co-review wait posts nothing), so an account
	// block from another PR must not delay them.
	if reviewedHead {
		return coAwareDedupe(r, obs, p, now)
	}
	if g.BlockedUntil != nil && g.BlockedUntil.After(now) {
		// Degrade instead of stalling: the block only gates CodeRabbit quota,
		// so ask the co-reviewers now and leave the round queued — CodeRabbit
		// still fires the moment the window opens. DecideCoPost's guards
		// (command configured, no already-posted or live command) make this
		// idempotent per round.
		if d, ok := decideCoDeferred(r, obs, p, now, "account blocked"); ok {
			return d
		}
		return FireDecision{Verdict: FireNo, Reason: "account blocked until " + g.BlockedUntil.UTC().Format(time.RFC3339)}
	}
	if g.LastFired != nil && now.Sub(*g.LastFired) < p.MinInterval {
		return FireDecision{Verdict: FireNo, Reason: "min interval"}
	}
	// crq posts always-mode co-reviewer triggers in the same fire step when the
	// bot does not auto-review and no command exists for this head.
	postCo := decideCoPosts(r, obs, p, now)
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
		return FireDecision{Verdict: FireAdopt, Reason: "review command already posted", AdoptCommandID: newest.ID, AdoptAt: at, PostCo: postCo, PostCodex: hasCodexLogin(postCo)}
	}
	return FireDecision{Verdict: FirePost, PostCo: postCo, PostCodex: hasCodexLogin(postCo)}
}

// decideCoDeferred starts or adopts the co-reviewer half of a round while
// CodeRabbit cannot fire. Each co-bot's Commands are cutoff-filtered by
// observe(), so an existing command here is safe to bind to this head and must
// be recorded as the round anchor rather than merely suppressing a duplicate
// post. The legacy Adopt fields mirror the Codex entry for pre-migration
// consumers.
func decideCoDeferred(r state.Round, obs Observation, p Policy, now time.Time, reason string) (FireDecision, bool) {
	if !p.RateLimitCodexDegrade {
		return FireDecision{}, false
	}
	var post []string
	adopt := map[string]CommandSeen{}
	for _, cp := range p.coReviewers() {
		if roundCoCommandID(r, cp.Login) != 0 {
			continue
		}
		commands := obs.co(cp.Login).Commands
		if DecideCoPost(r, obs, cp, len(commands) > 0, time.Time{}, now) {
			post = append(post, cp.Login)
			continue
		}
		// Only a trigger-capable bot may anchor the deferred round on an
		// adopted command; for the rest a live human command is just ambient.
		if cp.Trigger != TriggerAlways {
			continue
		}
		if newest := newestCommand(commands); newest != nil {
			adopt[cp.Login] = *newest
		}
	}
	if len(post) == 0 && len(adopt) == 0 {
		return FireDecision{}, false
	}
	d := FireDecision{Verdict: FireCoDeferred, PostCo: post, PostCodex: hasCodexLogin(post)}
	if len(adopt) > 0 {
		d.AdoptCo = adopt
	}
	switch {
	case len(post) > 0 && len(adopt) > 0:
		d.Reason = reason + "; requesting/adopting co-reviews now, coderabbit deferred"
	case len(post) > 0:
		d.Reason = reason + "; requesting co-review now, coderabbit deferred"
	default:
		d.Reason = reason + "; adopting existing co-review command, coderabbit deferred"
	}
	if cmd, ok := adopt[dialect.CodexBotLogin]; ok {
		at := cmd.CreatedAt
		if at.IsZero() {
			at = cmd.UpdatedAt
		}
		d.AdoptCommandID = cmd.ID
		d.AdoptAt = at
	}
	return d, true
}

func newestCommand(commands []CommandSeen) *CommandSeen {
	var newest *CommandSeen
	for i := range commands {
		cmd := &commands[i]
		if newest == nil || cmd.CreatedAt.After(newest.CreatedAt) {
			newest = cmd
		}
	}
	return newest
}

// coAwareDedupe resolves what to do when CodeRabbit already reviewed the head.
// If no gating co-reviewer is still outstanding, the round is genuinely done
// (FireDedupe). If a required-or-auto-active co-bot has no review of this head
// yet, the round is not done: post the triggers crq may (FireCoOnly). When crq
// may not post but the bot will still produce evidence on its own — it
// auto-reviews, or a command is already on the PR awaiting its answer — wait
// for it, bounded, without posting or holding the slot (FireCoReviewWait);
// leaving the round queued with no deadline is the bug that hangs the loop
// forever. Only when a co-bot gates purely by configuration with no way to
// obtain its review (no command configured/on the PR and no auto-review) fall
// back to completing on CodeRabbit's review; the feedback gate then surfaces
// it as still pending rather than the round wedging in an un-timed fire loop.
// Completion counts the existing CodeRabbit review, so a FireCoOnly round
// waits on the co-reviewers alone.
func coAwareDedupe(r state.Round, obs Observation, p Policy, now time.Time) FireDecision {
	var post []string
	wait := false
	for _, cp := range p.coReviewers() {
		co := obs.co(cp.Login)
		gates := requiredBot(p, cp.Login) || co.AutoActive
		if !gates || coReviewedHead(obs, cp.Login) {
			continue
		}
		if DecideCoPost(r, obs, cp, len(co.Commands) > 0, time.Time{}, now) {
			post = append(post, cp.Login)
			continue
		}
		if co.AutoActive || len(co.Commands) > 0 || roundCoCommandID(r, cp.Login) != 0 {
			wait = true
		}
	}
	if len(post) > 0 {
		return FireDecision{Verdict: FireCoOnly, Reason: "coderabbit reviewed head; co-review still required", PostCo: post, PostCodex: hasCodexLogin(post)}
	}
	if wait {
		return FireDecision{Verdict: FireCoReviewWait, Reason: "awaiting co-review"}
	}
	return FireDecision{Verdict: FireDedupe, Reason: "bot already reviewed head"}
}
