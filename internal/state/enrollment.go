package state

import (
	"sort"
	"time"
)

// RepoEnrollment is one repository's answer to "does crq review this project at
// all?", recorded in shared state so every host agrees — and so the decision can
// be made from a dashboard instead of by editing an env file on whichever
// machine happens to run the daemon.
//
// Absent means "no decision here": the host's own CRQ_REPOS/CRQ_EXCLUDE decide,
// exactly as they did before this record existed.
type RepoEnrollment struct {
	Enabled bool `json:"enabled"`
	// Reason is required to turn a repository OFF, for the same reason a hold
	// needs one: an unexplained absence is the one nobody dares reverse.
	Reason    string     `json:"reason,omitempty"`
	By        string     `json:"by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Enrollment returns repo's enrollment record, and whether one exists.
func (s *State) Enrollment(repo string) (RepoEnrollment, bool) {
	e, ok := s.Enrolled[normalizeRepoKey(repo)]
	return e, ok
}

// SetEnrollment records the decision, replacing any earlier one.
func (s *State) SetEnrollment(repo string, e RepoEnrollment) {
	if s.Enrolled == nil {
		s.Enrolled = map[string]RepoEnrollment{}
	}
	s.Enrolled[normalizeRepoKey(repo)] = e
}

// ClearEnrollment drops the record, handing the repository back to the hosts'
// env files. It returns whether there was one.
func (s *State) ClearEnrollment(repo string) bool {
	key := normalizeRepoKey(repo)
	if _, ok := s.Enrolled[key]; !ok {
		return false
	}
	delete(s.Enrolled, key)
	return true
}

// EnrolledRepos is every repository with a record, sorted, whichever way it was
// decided — a repository turned off is as much a decision as one turned on and
// must stay visible.
func (s *State) EnrolledRepos() []string {
	repos := make([]string, 0, len(s.Enrolled))
	for repo := range s.Enrolled {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}
