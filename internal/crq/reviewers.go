package crq

import (
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
)

// Reviewer is one configured reviewer, and the single description of it.
//
// crq carried four lists for this: Bot named the primary, RequiredBots gated
// convergence, FeedbackBots decided whose findings surfaced, and CoBots held
// per-bot trigger policy. Each answered a different question about the same set,
// so adding a reviewer meant editing four places and keeping them consistent, and
// reading the configuration meant cross-referencing them.
//
// Budget is the field that matters most, because it is the only reason a queue
// exists: an account-metered reviewer must be serialized against a shared
// allowance, and one that costs nothing has no reason to wait behind anybody.
// Saying so as data is what lets the rules ask what a reviewer costs rather than
// whether it happens to be CodeRabbit.
type Reviewer struct {
	Login  string
	Name   string
	Budget dialect.Budget
	// Required gates convergence: crq will not call a round done until this
	// reviewer has answered for the head.
	Required bool
	// Command and Trigger describe how crq asks for a review. An empty Command
	// means crq cannot ask at all, whatever the mode says.
	Command       string
	Trigger       engine.TriggerMode
	SelfHealGrace time.Duration
}

// Metered reports whether this reviewer spends the shared account allowance —
// the property the fire slot and the quota gate exist for.
func (r Reviewer) Metered() bool { return r.Budget == dialect.BudgetAccount }

// Primary returns the account-metered reviewer, which is the one the queue
// serializes. Exactly one is configured today; the second return is false when
// none is.
func (c Config) Primary() (Reviewer, bool) {
	for _, r := range c.Reviewers {
		if r.Metered() {
			return r, true
		}
	}
	return Reviewer{}, false
}

// FreeRunning returns the reviewers that cost the account nothing, so they never
// take the fire slot and never wait on the quota window.
func (c Config) FreeRunning() []Reviewer {
	var out []Reviewer
	for _, r := range c.Reviewers {
		if !r.Metered() {
			out = append(out, r)
		}
	}
	return out
}

// reviewerLogins is the login list for a predicate over the configured
// reviewers, in configuration order.
func (c Config) reviewerLogins(want func(Reviewer) bool) []string {
	var out []string
	for _, r := range c.Reviewers {
		if want(r) {
			out = append(out, r.Login)
		}
	}
	return out
}

// buildReviewers assembles the one list from what the environment parsed: the
// configured primary, the enabled co-reviewers, and any other login the operator
// required that neither covers.
//
// It must describe the existing configuration exactly, not a tidier version of
// it. Four ways of getting that wrong showed up in review, and each was a silent
// behaviour change hiding inside a "pure refactor":
//
//   - a required bot outside the primary and the registry (say sonar[bot]) has no
//     entry to build from, and dropping it would stop gating a reviewer the
//     operator asked to wait for;
//   - CRQ_REQUIRED_BOTS may deliberately exclude the primary, so requiredness is
//     read, never assumed;
//   - CRQ_BOT may name a registry bot, which would otherwise appear twice —
//     once metered, once free-running;
//   - the primary's trigger lives in ReviewCommand, and an empty Command means
//     "crq cannot ask this reviewer", which is false for it.
func buildReviewers(primary, primaryCommand string, required []string, coBots []CoBotConfig) []Reviewer {
	requiredSet := map[string]bool{}
	for _, login := range required {
		if login = strings.TrimSpace(login); login != "" {
			requiredSet[dialect.NormalizeBotName(login)] = true
		}
	}
	seen := map[string]bool{}
	out := make([]Reviewer, 0, len(coBots)+len(required)+1)

	if primary != "" {
		key := dialect.NormalizeBotName(primary)
		seen[key] = true
		out = append(out, Reviewer{
			Login:    primary,
			Name:     key,
			Budget:   dialect.BudgetAccount,
			Required: requiredSet[key],
			Command:  primaryCommand,
			Trigger:  engine.TriggerAlways,
		})
	}
	for _, cb := range coBots {
		key := dialect.NormalizeBotName(cb.Login)
		if seen[key] {
			continue // already configured as the primary
		}
		seen[key] = true
		out = append(out, Reviewer{
			Login:         cb.Login,
			Name:          cb.Name,
			Budget:        dialect.BudgetNone,
			Required:      cb.Required || requiredSet[key],
			Command:       cb.Command,
			Trigger:       cb.Trigger,
			SelfHealGrace: cb.SelfHealGrace,
		})
	}
	// Whatever else the operator required. crq knows nothing about how to trigger
	// it, but it still gates convergence, which is the whole reason it was listed.
	for _, login := range required {
		login = strings.TrimSpace(login)
		key := dialect.NormalizeBotName(login)
		if login == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Reviewer{
			Login:    login,
			Name:     key,
			Budget:   dialect.BudgetNone,
			Required: true,
			Trigger:  engine.TriggerNever,
		})
	}
	return out
}

// silenceTrigger stops crq posting a co-reviewer trigger for login while leaving
// its entry — and therefore its registry wording and check hooks — in place.
// Used for a primary that is also a registry bot: it is triggered as the
// primary, and asking it twice is the bug, but its evidence is still read here.
func silenceTrigger(coBots []CoBotConfig, login string) []CoBotConfig {
	if login == "" {
		return coBots
	}
	key := dialect.NormalizeBotName(login)
	out := make([]CoBotConfig, 0, len(coBots))
	for _, cb := range coBots {
		if dialect.NormalizeBotName(cb.Login) == key {
			cb.Trigger = engine.TriggerNever
		}
		out = append(out, cb)
	}
	return out
}

// evidenceBots is the set whose output crq reads: everyone whose findings are
// surfaced, plus everyone it waits for. The two must never diverge — a bot crq
// gates on whose findings it did not surface would hang the round forever.
func (c Config) evidenceBots() map[string]struct{} {
	return dialect.BotSet(unionBots(c.FeedbackBots, c.RequiredBots))
}

// ForRepo applies a repository's reviewer override to the fleet configuration.
//
// The primary is deliberately NOT overridable. Its markers and command are
// injected into the dialect classifiers when the Service is constructed, so a
// per-repo primary would mean per-repo classifiers — a much larger change than
// "which co-reviewers run here", which is what the request actually is.
func (c Config) ForRepo(ov RepoReviewers) Config {
	if !ov.SetCoBots && !ov.SetRequired {
		return c
	}
	out := c

	// The effective required set, whichever half the override named.
	required := c.RequiredBots
	if ov.SetRequired {
		required = ov.Required
	}

	// The effective co-reviewer set. Required implies enabled — the rule the
	// fleet parse already follows, and it has to hold when only the required
	// half is overridden too: a bot that gates but is never triggered makes the
	// round wait for evidence crq never asks for.
	enabled := ov.CoBots
	if !ov.SetCoBots {
		enabled = nil
		for _, cb := range c.CoBots {
			enabled = append(enabled, cb.Login)
		}
	}
	for _, login := range required {
		if sameBot(login, c.Bot) || containsBot(enabled, login) {
			continue
		}
		if _, ok := dialect.CoReviewerByName(login); ok {
			enabled = append(enabled, login)
		}
	}

	keep := make([]CoBotConfig, 0, len(enabled)+1)
	have := map[string]bool{}
	for _, cb := range c.CoBots {
		switch {
		case sameBot(cb.Login, c.Bot):
			// A primary that is itself a registry bot keeps its silenced entry
			// whatever the override says: that entry carries its wording and
			// check-run hooks, and dropping it costs the PRIMARY its evidence.
		case containsBot(enabled, cb.Login):
			cb.Required = containsBot(required, cb.Login)
			cb = promoteTrigger(cb)
		default:
			continue
		}
		keep = append(keep, cb)
		have[dialect.NormalizeBotName(cb.Login)] = true
	}
	// A repository may choose a bot the fleet does not enable — otherwise
	// "which bots for which project" only ever subtracts. Its configuration
	// comes from the registry, since there is no per-bot environment for a repo.
	for _, login := range enabled {
		if have[dialect.NormalizeBotName(login)] {
			continue
		}
		if co, ok := dialect.CoReviewerByName(login); ok {
			keep = append(keep, defaultCoBot(co, containsBot(required, login)))
			have[dialect.NormalizeBotName(login)] = true
		}
	}
	out.CoBots = keep
	out.RequiredBots = append([]string(nil), required...)

	// Rebuild the derived views from the overridden lists, exactly as
	// LoadConfig does, so no view can answer differently from another.
	out.Reviewers = buildReviewers(out.Bot, out.ReviewCommand, out.RequiredBots, out.CoBots)
	out.RequiredBots = out.reviewerLogins(func(r Reviewer) bool { return r.Required })
	if !c.FeedbackBotsExplicit {
		out.FeedbackBots = out.reviewerLogins(func(r Reviewer) bool { return r.Required || !r.Metered() })
	}
	return out
}

// promoteTrigger stops a required co-reviewer from being one crq never asks.
//
// A fleet entry carries the trigger its OWN required-ness produced: Codex
// defaults to never and only becomes always when required. Retaining that entry
// while making the bot required leaves the engine waiting for evidence no
// command was ever posted for. Only a never trigger is promoted, so an operator
// who deliberately configured selfheal keeps it.
func promoteTrigger(cb CoBotConfig) CoBotConfig {
	if !cb.Required || cb.Trigger != engine.TriggerNever || cb.Command == "" {
		return cb
	}
	if co, ok := dialect.CoReviewerByName(cb.Name); ok && co.RequiredTrigger != "" {
		cb.Trigger = triggerMode(co.RequiredTrigger, engine.TriggerAlways)
		return cb
	}
	cb.Trigger = engine.TriggerAlways
	return cb
}

// sameBot reports whether two spellings name the same reviewer.
func sameBot(a, b string) bool {
	return dialect.NormalizeBotName(a) == dialect.NormalizeBotName(b)
}

func containsBot(logins []string, login string) bool {
	key := dialect.NormalizeBotName(login)
	for _, candidate := range logins {
		if dialect.NormalizeBotName(candidate) == key {
			return true
		}
	}
	return false
}

// cfgFor is the configuration crq should use for one repository: the fleet
// default with that repository's override applied.
func (s *Service) cfgFor(st State, repo string) Config {
	ov, ok := st.RepoOverride(repo)
	if !ok {
		return s.cfg
	}
	return s.cfg.ForRepo(ov)
}
