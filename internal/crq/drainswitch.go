package crq

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DrainSetting is one repository's drain answer, as the CLI reports it.
type DrainSetting struct {
	Repo      string     `json:"repo"`
	Enabled   bool       `json:"enabled"`
	Default   bool       `json:"default"`
	Reason    string     `json:"reason,omitempty"`
	By        string     `json:"by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// SetDrainEnabled records whether crq may run fix sessions for repo.
//
// Recorded rather than defaulted, so "on" after an "off" is a real answer and
// not just the absence of one — the dashboard and `crq drain` can then show that
// somebody made the call, and when.
func (s *Service) SetDrainEnabled(ctx context.Context, repo string, enabled bool, reason string) (DrainSetting, error) {
	repo = NormalizeRepo(repo)
	if !validRepoSlug(repo) {
		return DrainSetting{}, fmt.Errorf("repo must be owner/name")
	}
	now := s.clock().UTC()
	sw := RepoDrainSwitch{Enabled: enabled, UpdatedAt: &now, By: s.cfg.Host, Reason: reason}
	if _, err := s.store.Update(ctx, func(st *State) error {
		st.SetDrainSwitch(repo, sw)
		return nil
	}); err != nil {
		return DrainSetting{}, err
	}
	return DrainSetting{Repo: repo, Enabled: enabled, Reason: reason, By: sw.By, UpdatedAt: sw.UpdatedAt}, nil
}

// ClearDrainEnabled returns repo to the default (drained), reporting whether a
// setting was there.
func (s *Service) ClearDrainEnabled(ctx context.Context, repo string) (bool, error) {
	repo = NormalizeRepo(repo)
	if !validRepoSlug(repo) {
		return false, fmt.Errorf("repo must be owner/name")
	}
	cleared := false
	if _, err := s.store.Update(ctx, func(st *State) error {
		cleared = st.ClearDrainSwitch(repo)
		if !cleared {
			return ErrNoChange
		}
		return nil
	}); err != nil {
		return false, err
	}
	return cleared, nil
}

// DrainSettings reports the drain answer for every repository in scope, so the
// listing shows what WILL happen rather than only what was written down.
func (s *Service) DrainSettings(ctx context.Context) ([]DrainSetting, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []DrainSetting
	add := func(repo string) {
		key := NormalizeRepo(repo)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		setting := DrainSetting{Repo: key, Enabled: st.DrainEnabled(key), Default: true}
		if sw, ok := st.DrainSwitch(key); ok {
			setting.Default = false
			setting.Reason, setting.By, setting.UpdatedAt = sw.Reason, sw.By, sw.UpdatedAt
		}
		out = append(out, setting)
	}
	effective := s.fleetCfg(st)
	watched := make([]string, 0, len(effective.AllowRepos))
	for repo := range effective.AllowRepos {
		watched = append(watched, repo)
	}
	sort.Strings(watched)
	for _, repo := range watched {
		add(repo)
	}
	// A repository ruled on but no longer watched still shows: an "off" nobody
	// can see is how a repository quietly stops being fixed.
	for _, repo := range st.DrainSwitches() {
		add(repo)
	}
	return out, nil
}

// validRepoSlug reports whether repo is exactly "owner/name". The workspace
// package has its own copy for path safety; this one guards a state key, where
// the hazard is a setting recorded under a name nothing will ever match.
func validRepoSlug(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && validNameSegment(owner) && validNameSegment(name)
}

// validNameSegment reports whether part is one segment a GitHub owner or
// repository name could be.
//
// Same segment rules the workspace package applies to a path: "." and ".." are
// not names. The character class is GitHub's: an owner or repository name is
// letters, digits, hyphen, underscore and dot, so a segment holding anything
// else — a space, a slash, a control character — is one no scan result can ever
// normalize to. Recorded, it reads as a rule covering something while covering
// nothing at all.
func validNameSegment(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
