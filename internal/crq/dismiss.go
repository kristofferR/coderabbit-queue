package crq

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// The dismissal is recorded against the round for the CURRENT head, enqueueing
// the PR if crq was not already tracking that head. That is deliberate: the
// deadlock's signature is that no round exists for the head at all, so a
// dismissal with nowhere to live would change nothing.
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

	// Ensure a round exists for the head this dismissal is about.
	if _, err := s.Enqueue(ctx, repo, pr); err != nil {
		return DismissResult{}, err
	}
	head, open, err := s.pullHead(ctx, repo, pr)
	if err != nil {
		return DismissResult{}, err
	}
	if !open {
		return DismissResult{}, fmt.Errorf("%s#%d is closed", repo, pr)
	}

	out := DismissResult{Repo: repo, PR: pr, Head: head, Reason: reason, Dismissed: []string{}}
	if _, err := s.store.Update(ctx, func(st *State) error {
		round := st.Round(repo, pr)
		if round == nil {
			return fmt.Errorf("%s#%d is not tracked", repo, pr)
		}
		// Guard the race the enqueue above cannot close: a push between the
		// enqueue and here means this dismissal is about findings from a head
		// nobody is looking at any more.
		if !strings.HasPrefix(head, round.Head) {
			return fmt.Errorf("%s#%d moved to %s while dismissing; re-read the findings", repo, pr, head[:min(9, len(head))])
		}
		out.Dismissed = out.Dismissed[:0]
		out.Already = nil
		for _, id := range clean {
			if round.Dismiss(id, reason) {
				out.Dismissed = append(out.Dismissed, id)
			} else {
				out.Already = append(out.Already, id)
			}
		}
		st.PutRound(*round)
		return nil
	}); err != nil {
		return DismissResult{}, err
	}
	if s.log != nil && len(out.Dismissed) > 0 {
		s.log.Printf("%s#%d dismissed %d finding(s) at %s: %s", repo, pr, len(out.Dismissed), out.Head, reason)
	}
	return out, nil
}
