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
