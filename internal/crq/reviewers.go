package crq

import (
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
// configured primary, which is the only account-metered reviewer, followed by the
// enabled co-reviewers in registry order.
//
// It is built from the same inputs the four legacy lists were, so this describes
// the existing configuration rather than changing it — the lists are then derived
// back from here, which is what keeps them from drifting apart.
func buildReviewers(primary string, required []string, coBots []CoBotConfig) []Reviewer {
	requiredSet := map[string]bool{}
	for _, login := range required {
		requiredSet[dialect.NormalizeBotName(login)] = true
	}
	out := make([]Reviewer, 0, len(coBots)+1)
	if primary != "" {
		out = append(out, Reviewer{
			Login:  primary,
			Name:   dialect.NormalizeBotName(primary),
			Budget: dialect.BudgetAccount,
			// The primary always gates: a round is not reviewed until the reviewer
			// whose quota it spent has answered.
			Required: true,
			Trigger:  engine.TriggerAlways,
		})
	}
	for _, cb := range coBots {
		out = append(out, Reviewer{
			Login:         cb.Login,
			Name:          cb.Name,
			Budget:        dialect.BudgetNone,
			Required:      cb.Required || requiredSet[dialect.NormalizeBotName(cb.Login)],
			Command:       cb.Command,
			Trigger:       cb.Trigger,
			SelfHealGrace: cb.SelfHealGrace,
		})
	}
	return out
}
