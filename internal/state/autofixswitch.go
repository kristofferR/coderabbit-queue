package state

import (
	"sort"
	"strings"
	"time"
)

// RepoAutofixSwitch is one repository's answer to "may crq fix pull requests
// here?", and when it was recorded.
type RepoAutofixSwitch struct {
	Enabled   bool       `json:"enabled"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	By        string     `json:"by,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// AutofixEnabled reports whether fix sessions may run for repo.
//
// Default ON, and absence means default: fixing is what the watcher is FOR,
// so a repository nobody has ruled on gets fixed. Only an explicit "off"
// recorded here stops it, which is the shape an operator expects — the thing
// works, and you turn it off where you do not want it.
//
// It lives in the state ref rather than in each repository, for the same reason
// the reviewer override does: the daemon has no checkout of what it watches, and
// a daemon and an agent reading different configurations while writing one ref
// is a class of divergence worth not having.
func (s *State) AutofixEnabled(repo string) bool {
	sw, ok := s.RepoAutofix[autofixRepoKey(repo)]
	return !ok || sw.Enabled
}

// AutofixSwitch returns repo's explicit setting, and whether one exists.
func (s *State) AutofixSwitch(repo string) (RepoAutofixSwitch, bool) {
	sw, ok := s.RepoAutofix[autofixRepoKey(repo)]
	return sw, ok
}

// SetAutofixSwitch records whether repo may be fixed, replacing any earlier answer.
func (s *State) SetAutofixSwitch(repo string, sw RepoAutofixSwitch) {
	if s.RepoAutofix == nil {
		s.RepoAutofix = map[string]RepoAutofixSwitch{}
	}
	s.RepoAutofix[autofixRepoKey(repo)] = sw
}

// ClearAutofixSwitch drops repo's setting, returning it to the default, and
// reports whether one was there.
func (s *State) ClearAutofixSwitch(repo string) bool {
	key := autofixRepoKey(repo)
	if _, ok := s.RepoAutofix[key]; !ok {
		return false
	}
	delete(s.RepoAutofix, key)
	return true
}

// AutofixSwitches lists every explicit setting, by repository, in a stable order.
func (s *State) AutofixSwitches() []string {
	repos := make([]string, 0, len(s.RepoAutofix))
	for repo := range s.RepoAutofix {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

// autofixRepoKey lowercases a repo slug so a switch set as Owner/Name matches a
// round recorded as owner/name. It matches Key's normalization, which is the
// one the rest of this package already keys on.
func autofixRepoKey(repo string) string {
	// Lowercase BEFORE trimming, so a ".GIT" suffix normalizes the same as ".git"
	// rather than surviving into the key and matching nothing.
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(repo)), ".git")
}
