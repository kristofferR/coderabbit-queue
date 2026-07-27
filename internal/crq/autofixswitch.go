package crq

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AutofixSetting is one repository's autofix answer, as the CLI reports it.
type AutofixSetting struct {
	Repo      string     `json:"repo"`
	Enabled   bool       `json:"enabled"`
	Default   bool       `json:"default"`
	Reason    string     `json:"reason,omitempty"`
	By        string     `json:"by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// SetAutofixEnabled records whether crq may run fix sessions for repo.
//
// Recorded rather than defaulted, so "on" after an "off" is a real answer and
// not just the absence of one — the dashboard and `crq autofix` can then show that
// somebody made the call, and when.
func (s *Service) SetAutofixEnabled(ctx context.Context, repo string, enabled bool, reason string) (AutofixSetting, error) {
	repo = NormalizeRepo(repo)
	if !validRepoSlug(repo) {
		return AutofixSetting{}, fmt.Errorf("repo must be owner/name")
	}
	now := s.clock().UTC()
	sw := RepoAutofixSwitch{Enabled: enabled, UpdatedAt: &now, By: s.cfg.Host, Reason: reason}
	if _, err := s.store.Update(ctx, func(st *State) error {
		st.SetAutofixSwitch(repo, sw)
		return nil
	}); err != nil {
		return AutofixSetting{}, err
	}
	return AutofixSetting{Repo: repo, Enabled: enabled, Reason: reason, By: sw.By, UpdatedAt: sw.UpdatedAt}, nil
}

// ClearAutofixEnabled returns repo to the default (autofix on), reporting whether a
// setting was there.
func (s *Service) ClearAutofixEnabled(ctx context.Context, repo string) (bool, error) {
	repo = NormalizeRepo(repo)
	if !validRepoSlug(repo) {
		return false, fmt.Errorf("repo must be owner/name")
	}
	cleared := false
	if _, err := s.store.Update(ctx, func(st *State) error {
		cleared = st.ClearAutofixSwitch(repo)
		if !cleared {
			return ErrNoChange
		}
		return nil
	}); err != nil {
		return false, err
	}
	return cleared, nil
}

// AutofixSettings reports the autofix answer for every repository in scope, so the
// listing shows what WILL happen rather than only what was written down.
func (s *Service) AutofixSettings(ctx context.Context) ([]AutofixSetting, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []AutofixSetting
	add := func(repo string) {
		key := NormalizeRepo(repo)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		setting := AutofixSetting{Repo: key, Enabled: st.AutofixEnabled(key), Default: true}
		if sw, ok := st.AutofixSwitch(key); ok {
			setting.Default = false
			setting.Reason, setting.By, setting.UpdatedAt = sw.Reason, sw.By, sw.UpdatedAt
		}
		out = append(out, setting)
	}
	watched := make([]string, 0, len(s.cfg.AllowRepos))
	for repo := range s.cfg.AllowRepos {
		watched = append(watched, repo)
	}
	sort.Strings(watched)
	for _, repo := range watched {
		add(repo)
	}
	// A repository ruled on but no longer watched still shows: an "off" nobody
	// can see is how a repository quietly stops being fixed.
	for _, repo := range st.AutofixSwitches() {
		add(repo)
	}
	return out, nil
}

// validRepoSlug reports whether repo is exactly "owner/name". The workspace
// package has its own copy for path safety; this one guards a state key, where
// the hazard is a setting recorded under a name nothing will ever match.
func validRepoSlug(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return false
	}
	// Same segment rules the workspace package applies to a path: "." and ".."
	// are not repository names, and a second slash means this is not one either.
	for _, part := range []string{owner, name} {
		if part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			return false
		}
	}
	return true
}
