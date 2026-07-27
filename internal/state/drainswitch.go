package state

import (
	"sort"
	"strings"
	"time"
)

// RepoDrainSwitch is one repository's answer to "may crq fix pull requests
// here?", and when it was recorded.
type RepoDrainSwitch struct {
	Enabled   bool       `json:"enabled"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	By        string     `json:"by,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// DrainEnabled reports whether fix sessions may run for repo.
//
// Default ON, and absence means default: draining is what the watcher is FOR,
// so a repository nobody has ruled on gets fixed. Only an explicit "off"
// recorded here stops it, which is the shape an operator expects — the thing
// works, and you turn it off where you do not want it.
//
// It lives in the state ref rather than in each repository, for the same reason
// the reviewer override does: the daemon has no checkout of what it watches, and
// a daemon and an agent reading different configurations while writing one ref
// is a class of divergence worth not having.
func (s *State) DrainEnabled(repo string) bool {
	sw, ok := s.RepoDrain[drainRepoKey(repo)]
	return !ok || sw.Enabled
}

// DrainSwitch returns repo's explicit setting, and whether one exists.
func (s *State) DrainSwitch(repo string) (RepoDrainSwitch, bool) {
	sw, ok := s.RepoDrain[drainRepoKey(repo)]
	return sw, ok
}

// SetDrainSwitch records whether repo may be fixed, replacing any earlier answer.
func (s *State) SetDrainSwitch(repo string, sw RepoDrainSwitch) {
	if s.RepoDrain == nil {
		s.RepoDrain = map[string]RepoDrainSwitch{}
	}
	s.RepoDrain[drainRepoKey(repo)] = sw
}

// ClearDrainSwitch drops repo's setting, returning it to the default, and
// reports whether one was there.
func (s *State) ClearDrainSwitch(repo string) bool {
	key := drainRepoKey(repo)
	if _, ok := s.RepoDrain[key]; !ok {
		return false
	}
	delete(s.RepoDrain, key)
	return true
}

// DrainSwitches lists every explicit setting, by repository, in a stable order.
func (s *State) DrainSwitches() []string {
	repos := make([]string, 0, len(s.RepoDrain))
	for repo := range s.RepoDrain {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

// drainRepoKey lowercases a repo slug so a switch set as Owner/Name matches a
// round recorded as owner/name. It matches Key's normalization, which is the
// one the rest of this package already keys on.
func drainRepoKey(repo string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(repo), ".git"))
}
