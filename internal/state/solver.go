package state

import (
	"sort"
	"time"
)

// SolverSettings is how a fix session should be run: which model, how hard it
// should think, what else to tell it, and the limits crq itself enforces.
//
// It is recorded per repository and, as a default, for the whole fleet. The
// reason it is not one fleet-wide answer is that the repositories differ more
// than the fleet does: a Go service worth a slow careful model and five
// attempts sits next to a docs repository where a fast one and a single
// attempt is the right trade.
//
// What is NOT here is the agent binary. That is chosen at install time and
// baked into the session script, because switching between claude and codex is
// a different command line rather than a different flag — and a per-repo agent
// would mean the watcher could not know what it was about to run until it ran
// it. Model and effort ARE per repo, because every agent has them.
type SolverSettings struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// Prompt is extra instruction appended to the fix prompt, for the standing
	// context a repository needs every time ("this project uses bun, never npm").
	Prompt string `json:"prompt,omitempty"`
	// MaxAttempts bounds fix sessions per head, so a fix that keeps not working
	// stops. Nil inherits.
	MaxAttempts *int `json:"max_attempts,omitempty"`
	// Forks allows sessions on pull requests whose head branch lives in another
	// repository. Off by default fleet-wide: a session runs an agent over that
	// branch's code with approvals bypassed and a write token in reach.
	Forks *bool `json:"forks,omitempty"`
	// SkipAuthors are pull-request authors crq does not enqueue here. Set*
	// distinguishes "not chosen" from "chosen to be nobody".
	SkipAuthors    []string `json:"skip_authors,omitempty"`
	SetSkipAuthors bool     `json:"set_skip_authors,omitempty"`

	By        string     `json:"by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`

	// unknown carries members a newer binary wrote inside this record, for the
	// same reason FleetDefaults does: it is nested, so no outer carrier sees it.
	unknown unknownFields
}

// Empty reports whether this record says nothing at all, which is how a "clear"
// is distinguished from a setting of every field to its zero value.
func (s SolverSettings) Empty() bool {
	return s.Model == "" && s.Effort == "" && s.Prompt == "" &&
		s.MaxAttempts == nil && s.Forks == nil && !s.SetSkipAuthors
}

// Merge layers one record over another: every field this one states wins, and
// every field it leaves absent keeps the base's answer. That is what makes
// "fleet default, then repository" work without either layer knowing about the
// other.
func (s SolverSettings) Merge(over SolverSettings) SolverSettings {
	out := s
	if over.Model != "" {
		out.Model = over.Model
	}
	if over.Effort != "" {
		out.Effort = over.Effort
	}
	if over.Prompt != "" {
		out.Prompt = over.Prompt
	}
	if over.MaxAttempts != nil {
		out.MaxAttempts = over.MaxAttempts
	}
	if over.Forks != nil {
		out.Forks = over.Forks
	}
	if over.SetSkipAuthors {
		out.SkipAuthors, out.SetSkipAuthors = over.SkipAuthors, true
	}
	if over.UpdatedAt != nil {
		out.By, out.UpdatedAt = over.By, over.UpdatedAt
	}
	return out
}

// Solver returns repo's own solver record, and whether one exists.
func (s *State) Solver(repo string) (SolverSettings, bool) {
	sv, ok := s.RepoSolver[normalizeRepoKey(repo)]
	return sv, ok
}

// SetSolver records repo's solver settings, replacing any earlier ones.
func (s *State) SetSolver(repo string, sv SolverSettings, by string, now time.Time) {
	if s.RepoSolver == nil {
		s.RepoSolver = map[string]SolverSettings{}
	}
	at := now.UTC()
	sv.By, sv.UpdatedAt = by, &at
	s.RepoSolver[normalizeRepoKey(repo)] = sv
}

// ClearSolver drops repo's record, returning it to the fleet default.
func (s *State) ClearSolver(repo string) bool {
	key := normalizeRepoKey(repo)
	if _, ok := s.RepoSolver[key]; !ok {
		return false
	}
	delete(s.RepoSolver, key)
	return true
}

// SolverRepos lists every repository with its own record, sorted.
func (s *State) SolverRepos() []string {
	out := make([]string, 0, len(s.RepoSolver))
	for repo := range s.RepoSolver {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// EffectiveSolver is the fleet default with repo's own record layered over it.
func (s *State) EffectiveSolver(repo string) SolverSettings {
	sv, _ := s.Solver(repo)
	return s.Fleet.Solver.Merge(sv)
}
