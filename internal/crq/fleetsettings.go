package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// FleetView is the fleet's effective defaults and where each one comes from.
type FleetView struct {
	// Reviewers is the default set every repository inherits, resolved the same
	// way a per-repo view is, so the two can be read side by side.
	Reviewers []ReviewerDetail `json:"reviewers"`
	// Recorded says a fleet record exists at all; without one every value below
	// is this host's env and changing it means editing a file on each machine.
	Recorded    bool   `json:"recorded"`
	MinInterval string `json:"min_interval"`
	WeeklyLimit int    `json:"weekly_limit"`
	// AutofixDefault is whether a repository with no explicit switch is fixed.
	AutofixDefault bool `json:"autofix_default"`
	// Sources names, per setting, whether the value came from the record or from
	// this host's env — the distinction that decides whether changing it here
	// will actually change anything for the other hosts.
	Sources map[string]string `json:"sources"`

	By        string   `json:"by,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	Lagging   []string `json:"lagging_hosts,omitempty"`
}

// FleetImpact is what a proposed change would do, in product terms, before it
// is made. The plan for the dashboard asked for this and it is the reason the
// fleet form is a separate verb from the per-repo one: a per-repo save affects
// the repository you are looking at, and a fleet save affects every repository
// that has not overridden the setting — which is most of them.
type FleetImpact struct {
	// Repos is how many repositories inherit the changed setting.
	Repos int `json:"repos"`
	// Reopened is how many completed rounds a reviewer change would requeue,
	// because their heads would suddenly be missing a required answer.
	Reopened int `json:"reopened"`
	// Overridden is how many repositories would NOT be affected, because they
	// have their own answer already.
	Overridden int      `json:"overridden"`
	Changes    []string `json:"changes"`
	Summary    string   `json:"summary"`
}

// FleetSettings reports the fleet's effective defaults.
func (s *Service) FleetSettings(ctx context.Context) (FleetView, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetView{}, err
	}
	return s.fleetViewOf(st), nil
}

func (s *Service) fleetViewOf(st State) FleetView {
	cfg := s.cfg.WithFleet(st.Fleet)
	view := FleetView{
		Recorded:       st.Fleet.UpdatedAt != nil,
		MinInterval:    cfg.MinInterval.String(),
		WeeklyLimit:    cfg.WeeklyReviewLimit,
		AutofixDefault: st.AutofixDefaultOn(),
		Reviewers:      []ReviewerDetail{},
		Sources:        map[string]string{},
	}
	from := func(key string, recorded bool) {
		if recorded {
			view.Sources[key] = "fleet"
			return
		}
		view.Sources[key] = "env"
	}
	from("reviewers", st.Fleet.SetCoBots || st.Fleet.SetRequired)
	from("min_interval", strings.TrimSpace(st.Fleet.MinInterval) != "")
	from("weekly_limit", st.Fleet.WeeklyLimit != nil)
	from("autofix_default", st.Fleet.AutofixDefault != nil)

	for _, r := range cfg.Reviewers {
		view.Reviewers = append(view.Reviewers, ReviewerDetail{
			Login: r.Login, Budget: string(r.Budget), Required: r.Required, Trigger: string(r.Trigger),
		})
	}
	if st.Fleet.UpdatedAt != nil {
		view.By = st.Fleet.By
		view.UpdatedAt = st.Fleet.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		view.Lagging = st.LaggingWriters(CapsFleetDefaults, s.clock().UTC())
	}
	return view
}

// FleetChange is a proposed edit. Every field is a pointer or a nil slice
// meaning "leave this one alone": a form that posts its whole state would
// otherwise overwrite a setting another host changed a second earlier.
type FleetChange struct {
	CoBots         []string `json:"cobots"`
	Required       []string `json:"required"`
	MinInterval    *string  `json:"min_interval"`
	WeeklyLimit    *int     `json:"weekly_limit"`
	AutofixDefault *bool    `json:"autofix_default"`
	// Clear drops the whole record, returning every setting to this host's env.
	Clear bool `json:"clear"`
}

// PreviewFleet reports what a change would do without making it.
func (s *Service) PreviewFleet(ctx context.Context, change FleetChange) (FleetImpact, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetImpact{}, err
	}
	next, err := s.applyFleetChange(st, change)
	if err != nil {
		return FleetImpact{}, err
	}
	return s.fleetImpact(st, next), nil
}

// SetFleetSettings records the change and requeues whatever it invalidates.
func (s *Service) SetFleetSettings(ctx context.Context, change FleetChange) (FleetView, FleetImpact, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	next, err := s.applyFleetChange(st, change)
	if err != nil {
		return FleetView{}, FleetImpact{}, err
	}
	impact := s.fleetImpact(st, next)

	// Read before the write, for the same reason SetReviewers does: the requeue
	// may only touch live pull requests and the CAS closure cannot ask GitHub.
	open := map[string]map[int]bool{}
	if impact.Reopened > 0 {
		for _, repo := range s.reposFollowingFleet(st) {
			prs, err := s.openPRs(ctx, repo)
			if err != nil {
				return FleetView{}, FleetImpact{}, err
			}
			open[repo] = prs
		}
	}

	now := s.clock().UTC()
	written, err := s.store.Update(ctx, func(st *State) error {
		before := map[string]Config{}
		for _, repo := range s.reposFollowingFleet(*st) {
			before[repo] = s.cfgFor(*st, repo)
		}
		if change.Clear {
			if st.Fleet.UpdatedAt == nil {
				return ErrNoChange
			}
			st.Fleet = FleetDefaults{}
		} else {
			applied, aerr := s.applyFleetChange(*st, change)
			if aerr != nil {
				return aerr
			}
			st.SetFleetDefaults(applied, s.cfg.Host, now)
		}
		for repo, was := range before {
			s.reopenForChangedReviewers(st, repo, was, s.cfgFor(*st, repo), open[repo])
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return FleetView{}, FleetImpact{}, err
	}
	if err == nil {
		s.sync(ctx, written)
	} else {
		written, _, err = s.store.Load(ctx)
		if err != nil {
			return FleetView{}, FleetImpact{}, err
		}
	}
	return s.fleetViewOf(written), impact, nil
}

// applyFleetChange folds a change onto the current record, validating it. It
// does not write, so preview and save cannot disagree about what a change means.
func (s *Service) applyFleetChange(st State, change FleetChange) (FleetDefaults, error) {
	if change.Clear {
		return FleetDefaults{}, nil
	}
	fd := st.Fleet
	if change.CoBots != nil {
		resolved, err := resolveCoBotLogins(change.CoBots)
		if err != nil {
			return fd, err
		}
		fd.CoBots, fd.SetCoBots = resolved, true
	}
	if change.Required != nil {
		if len(change.Required) == 0 {
			return fd, errors.New("the required set cannot be empty: a round that gates on nobody converges before any reviewer runs")
		}
		resolved, err := resolveRequiredLogins(change.Required, s.cfg.Bot)
		if err != nil {
			return fd, err
		}
		fd.Required, fd.SetRequired = resolved, true
	}
	if change.MinInterval != nil {
		text := strings.TrimSpace(*change.MinInterval)
		if text == "" {
			fd.MinInterval = ""
		} else {
			d, err := time.ParseDuration(text)
			if err != nil {
				return fd, fmt.Errorf("min interval: %w", err)
			}
			if d < 0 {
				return fd, errors.New("min interval cannot be negative")
			}
			// The pacing floor is the fleet's protection against spending the
			// account faster than the vendor will refill it. A very small one is
			// legal but worth refusing to set by accident.
			if d > 0 && d < 5*time.Second {
				return fd, errors.New("min interval below 5s would fire faster than any review completes")
			}
			fd.MinInterval = d.String()
		}
	}
	if change.WeeklyLimit != nil {
		if *change.WeeklyLimit < 0 {
			return fd, errors.New("weekly limit cannot be negative")
		}
		limit := *change.WeeklyLimit
		fd.WeeklyLimit = &limit
	}
	if change.AutofixDefault != nil {
		on := *change.AutofixDefault
		fd.AutofixDefault = &on
	}
	return fd, nil
}

// fleetImpact describes, in product terms, what moving from st to next would do.
func (s *Service) fleetImpact(st State, next FleetDefaults) FleetImpact {
	after := st
	after.Fleet = next
	impact := FleetImpact{Changes: []string{}}

	following := s.reposFollowingFleet(st)
	impact.Repos = len(following)
	for repo := range st.Repos {
		if ov, ok := st.RepoOverride(repo); ok && (ov.SetCoBots || ov.SetRequired) {
			impact.Overridden++
		}
	}

	beforeCfg, afterCfg := s.cfg.WithFleet(st.Fleet), s.cfg.WithFleet(next)
	// Which reviewers RUN and which of them GATE are separate questions and
	// both are changes: turning a bot off stops its findings arriving at all,
	// which is not the same as it merely no longer holding the round open.
	beforeCo := beforeCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
	afterCo := afterCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
	if !sameLogins(beforeCo, afterCo) {
		impact.Changes = append(impact.Changes, fmt.Sprintf("co-reviewers running: %s → %s",
			shortBots(beforeCo), shortBots(afterCo)))
	}
	if !sameLogins(beforeCfg.RequiredBots, afterCfg.RequiredBots) {
		impact.Changes = append(impact.Changes, fmt.Sprintf("required reviewers: %s → %s",
			shortBots(beforeCfg.RequiredBots), shortBots(afterCfg.RequiredBots)))
	}
	if beforeCfg.MinInterval != afterCfg.MinInterval {
		impact.Changes = append(impact.Changes,
			fmt.Sprintf("pacing: %s → %s", beforeCfg.MinInterval, afterCfg.MinInterval))
	}
	if beforeCfg.WeeklyReviewLimit != afterCfg.WeeklyReviewLimit {
		impact.Changes = append(impact.Changes,
			fmt.Sprintf("weekly limit: %d → %d", beforeCfg.WeeklyReviewLimit, afterCfg.WeeklyReviewLimit))
	}
	if st.AutofixDefaultOn() != after.AutofixDefaultOn() {
		impact.Changes = append(impact.Changes,
			fmt.Sprintf("autofix default: %s → %s", onOff(st.AutofixDefaultOn()), onOff(after.AutofixDefaultOn())))
	}

	// A completed round is the "this head was reviewed" marker. Requiring a
	// reviewer it never had means that marker is now wrong, and the round has to
	// be reopened — which is the consequence worth stating before the click.
	for _, repo := range following {
		wasCfg, isCfg := s.cfgFor(st, repo), s.cfgFor(after, repo)
		wasCo := wasCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
		isCo := isCfg.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
		// reopenForChangedReviewers requeues on EITHER list changing, so the
		// count has to ask the same question or it under-reports the very
		// consequence it exists to warn about.
		if sameLogins(wasCfg.RequiredBots, isCfg.RequiredBots) && sameLogins(wasCo, isCo) {
			continue
		}
		for _, r := range st.Rounds {
			if NormalizeRepo(r.Repo) == repo && r.Phase == PhaseCompleted {
				impact.Reopened++
			}
		}
	}

	switch {
	case len(impact.Changes) == 0:
		impact.Summary = "nothing would change"
	case impact.Reopened > 0:
		impact.Summary = fmt.Sprintf("affects %d repositories following the fleet; %d completed round(s) would be reopened and reviewed again",
			impact.Repos, impact.Reopened)
	default:
		impact.Summary = fmt.Sprintf("affects %d repositories following the fleet; no round would be reopened", impact.Repos)
	}
	if impact.Overridden > 0 {
		impact.Summary += fmt.Sprintf(" (%d with their own reviewer override are unaffected)", impact.Overridden)
	}
	return impact
}

// reposFollowingFleet is every repository crq knows about that has no reviewer
// override of its own — the ones a fleet default actually reaches.
func (s *Service) reposFollowingFleet(st State) []string {
	seen := map[string]bool{}
	var out []string
	add := func(repo string) {
		repo = NormalizeRepo(repo)
		if repo == "" || seen[repo] {
			return
		}
		if ov, ok := st.RepoOverride(repo); ok && (ov.SetCoBots || ov.SetRequired) {
			return
		}
		seen[repo] = true
		out = append(out, repo)
	}
	for repo := range s.cfg.AllowRepos {
		add(repo)
	}
	for _, repo := range st.EnrolledRepos() {
		add(repo)
	}
	for _, r := range st.Rounds {
		add(r.Repo)
	}
	sort.Strings(out)
	return out
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// shortBots renders a login list the way a person reads it.
func shortBots(logins []string) string {
	if len(logins) == 0 {
		return "none"
	}
	out := make([]string, 0, len(logins))
	for _, l := range logins {
		out = append(out, dialect.NormalizeBotName(l))
	}
	return strings.Join(out, ", ")
}
