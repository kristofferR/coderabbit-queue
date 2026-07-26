package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// HoldResult reports a PR's hold state after a change.
type HoldResult struct {
	Repo   string    `json:"repo"`
	PR     int       `json:"pr"`
	Held   bool      `json:"held"`
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at,omitempty"`
}

// Hold takes a PR out of the review queue in one write.
//
// Holding used to need two commands that could not be one: the skip marker
// stops fleet auto-review from enqueueing, `crq cancel` stops the pump, and
// between the two a daemon fired anyway — which is exactly what happened while
// holding a PR off CodeRabbit earlier today. A hold is one fact, recorded where
// every firing path already looks, so there is no window between the halves.
//
// It does not cancel a round already in flight: that review is bought and its
// findings are still worth having. It stops the next one.
func (s *Service) Hold(ctx context.Context, repo string, pr int, reason string) (HoldResult, error) {
	repo = NormalizeRepo(repo)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return HoldResult{}, errors.New("a hold needs a reason: it is a note to whoever finds the PR stopped")
	}
	now := s.clock().UTC()
	state, err := s.store.Update(ctx, func(st *State) error {
		st.Hold(repo, pr, reason, s.cfg.Host, now)
		return nil
	})
	if err != nil {
		return HoldResult{}, err
	}
	s.sync(ctx, state)
	if s.log != nil {
		s.log.Printf("%s#%d held: %s", repo, pr, reason)
	}
	return HoldResult{Repo: repo, PR: pr, Held: true, Reason: reason, By: s.cfg.Host, At: now}, nil
}

// Unhold puts a PR back in the queue.
func (s *Service) Unhold(ctx context.Context, repo string, pr int) (HoldResult, error) {
	repo = NormalizeRepo(repo)
	released := false
	state, err := s.store.Update(ctx, func(st *State) error {
		if !st.Unhold(repo, pr) {
			return ErrNoChange
		}
		released = true
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return HoldResult{}, err
	}
	if released {
		s.sync(ctx, state)
		if s.log != nil {
			s.log.Printf("%s#%d released", repo, pr)
		}
	}
	return HoldResult{Repo: repo, PR: pr, Held: false}, nil
}

// Holds lists every held PR.
func (s *Service) Holds(ctx context.Context) ([]HoldResult, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]HoldResult, 0, len(st.Holds))
	for key, h := range st.Holds {
		repo, number, ok := strings.Cut(key, "#")
		if !ok {
			continue
		}
		pr := 0
		if _, err := fmt.Sscanf(number, "%d", &pr); err != nil {
			continue
		}
		out = append(out, HoldResult{Repo: repo, PR: pr, Held: true, Reason: h.Reason, By: h.By, At: h.At})
	}
	sortHolds(out)
	return out, nil
}

// sortHolds orders by repo then PR, so a listing is stable rather than however
// the map happened to iterate.
func sortHolds(holds []HoldResult) {
	sort.Slice(holds, func(i, j int) bool {
		if holds[i].Repo != holds[j].Repo {
			return holds[i].Repo < holds[j].Repo
		}
		return holds[i].PR < holds[j].PR
	})
}
