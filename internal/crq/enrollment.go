package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// EnrollmentView is one repository's enrollment as it will actually be applied.
type EnrollmentView struct {
	Repo string `json:"repo"`
	// Source is where the answer comes from, which matters more than the answer:
	// a repository forced on by a host's env cannot be turned off from here, and
	// saying "managed" would invite someone to try.
	//
	//	state    — a record in shared state decided it
	//	env      — this host's CRQ_REPOS lists it, with no record either way
	//	excluded — this host's CRQ_EXCLUDE names it (absolute; a kill switch)
	//	scope    — no allow-list at all, so everything in CRQ_SCOPE is reviewed
	//	off      — no record, no env mention, and an allow-list that omits it
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	// EnvConflict says the host's env and the shared record disagree. The record
	// wins, but silently overriding a file someone edited is how a fleet grows a
	// mystery, so it is reported.
	EnvConflict bool     `json:"env_conflict,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	By          string   `json:"by,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	Lagging     []string `json:"lagging_hosts,omitempty"`
}

// Enrollment reports whether crq reviews repo, and why.
func (s *Service) Enrollment(ctx context.Context, repo string) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return EnrollmentView{}, err
	}
	return s.enrollmentOf(st, repo), nil
}

// enrollmentOf resolves one repository against the record and this host's env.
//
// Precedence, and the reasoning for it: CRQ_EXCLUDE is absolute, because it is a
// per-host kill switch and the machine that has one usually has a reason the
// fleet does not know. Otherwise an explicit record wins in BOTH directions — an
// Off switch that does nothing but explain which file to go and edit on another
// machine is not a switch. Env alone still enrolls, so nothing changes for a
// fleet that never touches this.
func (s *Service) enrollmentOf(st State, repo string) EnrollmentView {
	repo = NormalizeRepo(repo)
	view := EnrollmentView{Repo: repo}
	inEnv := s.cfg.AllowRepos[repo]
	switch {
	case s.cfg.ExcludeRepos[repo]:
		view.Source, view.Enabled = "excluded", false
		return view
	case repo == NormalizeRepo(s.cfg.GateRepo):
		// The gate repository holds the queue's own state and dashboard;
		// reviewing it would be crq reviewing its own bookkeeping.
		view.Source, view.Enabled = "excluded", false
		return view
	}
	if rec, ok := st.Enrollment(repo); ok {
		view.Source, view.Enabled = "state", rec.Enabled
		view.Reason, view.By = rec.Reason, rec.By
		if rec.UpdatedAt != nil {
			view.UpdatedAt = rec.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		// Only one direction is a disagreement: a record that turns off a
		// repository a host's CRQ_REPOS lists. A record that turns one ON that
		// env never mentioned is the feature working, not a conflict.
		view.EnvConflict = inEnv && !rec.Enabled
		view.Lagging = st.LaggingWriters(CapsEnrollment, s.clock().UTC())
		return view
	}
	switch {
	case inEnv:
		view.Source, view.Enabled = "env", true
	case len(s.cfg.AllowRepos) == 0:
		view.Source, view.Enabled = "scope", true
	default:
		view.Source, view.Enabled = "off", false
	}
	return view
}

// SetEnrollment records whether crq reviews repo. Turning one off needs a
// reason: the repository disappears from every queue, and "why did this stop
// being reviewed" is a question the fleet should be able to answer itself.
func (s *Service) SetEnrollment(ctx context.Context, repo string, enabled bool, reason string) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	if !enabled && strings.TrimSpace(reason) == "" {
		return EnrollmentView{}, errors.New("turning a repository off needs --reason (every screen that shows it will show why)")
	}
	if s.cfg.ExcludeRepos[repo] {
		return EnrollmentView{}, fmt.Errorf("%s is in CRQ_EXCLUDE on %s — that is a per-host kill switch and shared state does not override it", repo, s.cfg.Host)
	}
	if repo == NormalizeRepo(s.cfg.GateRepo) {
		return EnrollmentView{}, fmt.Errorf("%s is the gate repository: it holds crq's own state and dashboard", repo)
	}
	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		if cur, ok := st.Enrollment(repo); ok && cur.Enabled == enabled && cur.Reason == reason {
			return ErrNoChange
		}
		st.SetEnrollment(repo, RepoEnrollment{
			Enabled: enabled, Reason: reason, By: s.cfg.Host, UpdatedAt: &now,
		})
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return EnrollmentView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else {
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return EnrollmentView{}, err
		}
	}
	return s.enrollmentOf(st, repo), nil
}

// ClearEnrollment drops the record, handing the repository back to the hosts'
// env files.
func (s *Service) ClearEnrollment(ctx context.Context, repo string) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	st, err := s.store.Update(ctx, func(st *State) error {
		if !st.ClearEnrollment(repo) {
			return ErrNoChange
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return EnrollmentView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else {
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return EnrollmentView{}, err
		}
	}
	return s.enrollmentOf(st, repo), nil
}

// Enrollments lists every repository this host would act on, plus every one a
// record mentions — including the ones turned off, because an "off" nobody can
// see is how a repository quietly stops being reviewed.
func (s *Service) Enrollments(ctx context.Context) ([]EnrollmentView, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var repos []string
	add := func(repo string) {
		repo = NormalizeRepo(repo)
		if repo == "" || seen[repo] {
			return
		}
		seen[repo] = true
		repos = append(repos, repo)
	}
	for repo := range s.cfg.AllowRepos {
		add(repo)
	}
	for _, repo := range st.EnrolledRepos() {
		add(repo)
	}
	sort.Strings(repos)
	out := make([]EnrollmentView, 0, len(repos))
	for _, repo := range repos {
		out = append(out, s.enrollmentOf(st, repo))
	}
	return out, nil
}

// reviewsRepo is the one question every scan path asks: may this host enqueue
// work for repo? Sharing it is what keeps autoreview, watch and autofix from
// each growing their own slightly different answer.
func (s *Service) reviewsRepo(st State, repo string) bool {
	return s.enrollmentOf(st, repo).Enabled
}

// scanTargets is the list a pass should search: every repository enrolled by
// env or by record. Empty means "no allow-list anywhere", which is the signal to
// search CRQ_SCOPE owner-wide instead.
func (s *Service) scanTargets(st State) []string {
	// An empty CRQ_REPOS means this host searches CRQ_SCOPE owner-wide. Records
	// must not narrow that to themselves: enrolling one repository would then
	// silently stop every other one from being scanned.
	if len(s.cfg.AllowRepos) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for repo := range s.cfg.AllowRepos {
		if s.reviewsRepo(st, repo) && !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	for _, repo := range st.EnrolledRepos() {
		if s.reviewsRepo(st, repo) && !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	sort.Strings(out)
	return out
}

// EnrollmentIn answers for an already-loaded state, so a caller rendering many
// repositories does not re-read the ref once per row.
func (s *Service) EnrollmentIn(st State, repo string) EnrollmentView {
	return s.enrollmentOf(st, repo)
}

// ScopeRepos lists the repositories in CRQ_SCOPE, for choosing one to enroll.
// It is the one genuinely expensive read in the dashboard — a multi-page REST
// walk per owner — so callers are expected to cache it.
func (s *Service) ScopeRepos(ctx context.Context) ([]ghapi.Repo, error) {
	seen := map[string]bool{}
	var out []ghapi.Repo
	for _, owner := range s.cfg.Scope {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		repos, err := s.gh.ListOwnerRepos(ctx, owner, 300)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			key := NormalizeRepo(r.FullName)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out, nil
}
