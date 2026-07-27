package crq

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// HoldResult reports a PR's hold state after a change.
type HoldResult struct {
	Repo   string `json:"repo"`
	PR     int    `json:"pr"`
	Held   bool   `json:"held"`
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`
	// At is a pointer because time.Time is a struct: omitempty never omits one,
	// so an unhold response used to carry "at":"0001-01-01T00:00:00Z".
	At *time.Time `json:"at,omitempty"`
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
		at := h.At
		out = append(out, HoldResult{Repo: repo, PR: pr, Held: true, Reason: h.Reason, By: h.By, At: &at})
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
