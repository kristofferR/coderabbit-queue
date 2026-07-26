package crq

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
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

	// Read the findings BEFORE writing anything. A dismissal is only meaningful
	// against a finding that is actually there, and writing first would leave a
	// round behind whenever validation then failed.
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

	// Already-dismissed IDs are no longer in Findings — Feedback filters them —
	// so a retried call would fail validation on its own earlier success. The
	// command is documented as idempotent, and an interrupted agent repeating
	// itself is the ordinary case.
	alreadyDone := map[string]bool{}
	if st, _, err := s.store.Load(ctx); err == nil {
		if round := st.Round(repo, pr); round != nil && round.Head == feedback.Head {
			for _, id := range clean {
				if round.IsDismissed(id) {
					alreadyDone[id] = true
				}
			}
		}
	} else {
		return DismissResult{}, err
	}

	wanted := map[string]bool{}
	for _, id := range clean {
		if alreadyDone[id] {
			continue
		}
		finding, ok := current[id]
		if !ok {
			return DismissResult{}, fmt.Errorf("%s is not a finding on %s#%d at %s; re-read them with crq next", id, repo, pr, feedback.Head)
		}
		if err := dismissible(finding); err != nil {
			return DismissResult{}, err
		}
		// A finding ID is a hash of its text, not of its commit. Dismissing one
		// carried from an older commit would record it against the current head,
		// and the identical finding re-reported here would then be filtered
		// though it was never judged for this head.
		if finding.Commit != "" && feedback.Head != "" && !dialect.SHAPrefixMatch(finding.Commit, feedback.Head) {
			return DismissResult{}, fmt.Errorf("%s belongs to commit %s, not the current head %s; re-read the findings",
				id, shortSHA(finding.Commit), feedback.Head)
		}
		wanted[id] = true
	}

	// Whether this call drains the head decides if a round may be CREATED here.
	// Creating one while other findings are still open would put a fire-eligible
	// round in the queue that DecideFire cannot hold back — it sees no findings —
	// so a pump could spend the primary's quota on code the caller is still
	// expected to fix.
	remaining := 0
	for _, finding := range engine.BlockingFindings(feedback.Findings, feedback.Head) {
		if !wanted[finding.ID] {
			remaining++
		}
	}

	out := DismissResult{Repo: repo, PR: pr, Head: feedback.Head, Reason: reason, Dismissed: []string{}}
	out.Dismissed, out.Already, err = s.recordDismissal(ctx, repo, pr, feedback.Head, clean, reason, remaining == 0)
	if err != nil {
		return DismissResult{}, err
	}
	if s.log != nil && len(out.Dismissed) > 0 {
		s.log.Printf("%s#%d dismissed %d finding(s) at %s: %s", repo, pr, len(out.Dismissed), out.Head, reason)
	}
	return out, nil
}

// dismissibleSources are the finding kinds that intrinsically have no review
// thread. Everything else either has one, or only LOOKS threadless: the REST
// fallback Feedback uses when the GraphQL thread query fails emits inline
// comments as "review_comment" with no thread ID, because REST does not return
// one — dismissing those would converge a round over threads that are open.
var dismissibleSources = map[string]bool{
	"review_body":    true,
	"review_prompt":  true,
	"review_skipped": true,
	"issue_comment":  true,
}

// dismissible reports why a finding may not be dismissed, or nil.
func dismissible(finding dialect.Finding) error {
	// A threaded finding has resolve and decline, and both put the decision on
	// the PR where the bot can answer it. Dismissing one would converge the
	// round with the thread still open, skipping the rebuttal flow.
	if finding.ThreadID != "" {
		return fmt.Errorf("%s has review thread %s: resolve it or decline it, so the decision is on the PR", finding.ID, finding.ThreadID)
	}
	if !dismissibleSources[finding.Source] {
		return fmt.Errorf("%s came from %s, which can carry a review thread crq could not read; resolve or decline it instead",
			finding.ID, finding.Source)
	}
	return nil
}
