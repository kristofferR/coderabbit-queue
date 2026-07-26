package crq

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// DismissResult reports what a dismissal did, per finding ID.
type DismissResult struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	Head string `json:"head"`
	// Dismissed lists the IDs recorded by this call; Already lists the ones this
	// round had already accounted for. Repeating a dismissal is not an error —
	// an agent re-reading its own findings should not have to remember.
	Dismissed []string `json:"dismissed"`
	Already   []string `json:"already_dismissed,omitempty"`
	Reason    string   `json:"reason"`
}

// Dismiss records that an agent has accounted for findings GitHub gives it no
// way to close.
//
// `crq resolve` and `crq decline` both act on a review thread. A review-body
// finding, a review-skipped notice or an outside-diff remark has none, so
// neither command can touch it — and drain-first then blocks every future round
// on a finding that can never drain. The observed end state was a PR where "no
// review was ever requested" for the current head, four rounds running.
//
// A finding must be present at the current head and have no thread. A threaded
// one has resolve and decline, and both put the decision on the PR where the bot
// can answer it; dismissing it instead would converge the round with the thread
// still open.
//
// The record goes on the round for the CURRENT head, creating or superseding
// that round in the same write. That is deliberate: the deadlock's signature is
// that no round exists for the head at all, so a dismissal with nowhere to live
// would change nothing.
func (s *Service) Dismiss(ctx context.Context, repo string, pr int, ids []string, reason string) (DismissResult, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return DismissResult{}, errors.New("a dismissal needs a reason")
	}
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return DismissResult{}, errors.New("no finding id given")
	}

	// Read the findings BEFORE writing anything. Two reasons: a dismissal is only
	// meaningful against a finding that is actually there, and enqueueing first
	// would leave a fire-eligible round behind whenever validation then failed —
	// which the autoreview daemon could claim and spend a review on.
	feedback, err := s.Feedback(ctx, repo, pr)
	if err != nil {
		return DismissResult{}, err
	}
	if !feedback.Open {
		return DismissResult{}, fmt.Errorf("%s#%d is closed", repo, pr)
	}
	current := make(map[string]dialect.Finding, len(feedback.Findings))
	for _, finding := range feedback.Findings {
		current[finding.ID] = finding
	}

	out := DismissResult{Repo: repo, PR: pr, Head: feedback.Head, Reason: reason, Dismissed: []string{}}
	state, err := s.store.Update(ctx, func(st *State) error {
		now := s.clock()
		round := st.Round(repo, pr)
		var err error
		switch {
		case round == nil:
			round, err = st.NewRound(repo, pr, feedback.Head, now)
		case round.Head != feedback.Head:
			round, err = st.Supersede(repo, pr, feedback.Head, now)
		}
		if err != nil {
			return err
		}
		out.Dismissed = out.Dismissed[:0]
		out.Already = nil
		for _, id := range clean {
			if round.IsDismissed(id) {
				out.Already = append(out.Already, id)
				continue
			}
			finding, ok := current[id]
			if !ok {
				return fmt.Errorf("%s is not a finding on %s#%d at %s; re-read them with crq next", id, repo, pr, feedback.Head)
			}
			// A threaded finding has resolve and decline, and both put the
			// decision on the PR where the bot can answer it. Dismissing one
			// would converge the round with the thread still open, silently
			// skipping the rebuttal flow that exists for exactly this.
			if finding.ThreadID != "" {
				return fmt.Errorf("%s has review thread %s: resolve it or decline it, so the decision is on the PR", id, finding.ThreadID)
			}
			round.Dismiss(id, reason)
			out.Dismissed = append(out.Dismissed, id)
		}
		st.PutRound(*round)
		return nil
	})
	if err != nil {
		return DismissResult{}, err
	}
	s.sync(ctx, state)
	if s.log != nil && len(out.Dismissed) > 0 {
		s.log.Printf("%s#%d dismissed %d finding(s) at %s: %s", repo, pr, len(out.Dismissed), out.Head, reason)
	}
	return out, nil
}
