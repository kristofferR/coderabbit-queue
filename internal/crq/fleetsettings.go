package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	// Overriding names the repositories that have their own answer, so a fleet
	// default can say who it does NOT reach. A count with no names is a number
	// you cannot act on.
	Overriding []string `json:"overriding,omitempty"`

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
	// "env" and "default" are different answers and the page must not merge
	// them: telling someone a value comes from their env file, when nothing in
	// that file mentions it, sends them looking for a line that is not there.
	host := s.cfg.Env()
	from := func(key string, recorded bool, envKeys ...string) {
		switch {
		case recorded:
			view.Sources[key] = "fleet"
		default:
			view.Sources[key] = "default"
			for _, ek := range envKeys {
				if strings.TrimSpace(host[ek]) != "" {
					view.Sources[key] = "env"
					break
				}
			}
		}
	}
	from("reviewers", st.Fleet.SetCoBots || st.Fleet.SetRequired, "CRQ_COBOTS", "CRQ_REQUIRED_BOTS")
	from("min_interval", strings.TrimSpace(st.Fleet.MinInterval) != "", "CRQ_MIN_INTERVAL")
	from("weekly_limit", st.Fleet.WeeklyLimit != nil, "CRQ_WEEKLY_LIMIT")
	from("autofix_default", st.Fleet.AutofixDefault != nil)

	for _, r := range cfg.Reviewers {
		view.Reviewers = append(view.Reviewers, ReviewerDetail{
			Login: r.Login, Budget: string(r.Budget), Required: r.Required, Trigger: string(r.Trigger),
		})
	}
	for repo := range st.Repos {
		if ov, ok := st.RepoOverride(repo); ok && (ov.SetCoBots || ov.SetRequired || ov.PrimaryOff) {
			view.Overriding = append(view.Overriding, repo)
		}
	}
	sort.Strings(view.Overriding)
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
	// Unset* removes one typed field from the record, handing that setting back
	// to each host's env. "Leave this alone" and "the fleet has no answer" are
	// different instructions, and a nil pointer or slice can only express the
	// first — so unsetting a co-reviewer list used to report success and change
	// nothing.
	UnsetCoBots      bool `json:"unset_cobots,omitempty"`
	UnsetRequired    bool `json:"unset_required,omitempty"`
	UnsetMinInterval bool `json:"unset_min_interval,omitempty"`
	UnsetWeeklyLimit bool `json:"unset_weekly_limit,omitempty"`
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
	switch {
	case change.UnsetCoBots:
		fd.CoBots, fd.SetCoBots = nil, false
	case change.CoBots != nil:
		resolved, err := resolveCoBotLogins(change.CoBots)
		if err != nil {
			return fd, err
		}
		fd.CoBots, fd.SetCoBots = resolved, true
	}
	switch {
	case change.UnsetRequired:
		fd.Required, fd.SetRequired = nil, false
	case change.Required != nil:
		if len(change.Required) == 0 {
			return fd, errors.New("the required set cannot be empty: a round that gates on nobody converges before any reviewer runs")
		}
		resolved, err := resolveRequiredLogins(change.Required, s.cfg.Bot)
		if err != nil {
			return fd, err
		}
		fd.Required, fd.SetRequired = resolved, true
	}
	if change.UnsetMinInterval {
		fd.MinInterval = ""
	}
	if change.UnsetWeeklyLimit {
		fd.WeeklyLimit = nil
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
		// The same question reopenForChangedReviewers asks, or the preview
		// warns about work that will not happen: only ADDING a reviewer
		// invalidates a finished round, so a narrowing reopens nothing.
		if !addedReviewers(wasCfg, isCfg, wasCo, isCo) {
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
		// A repository turned off follows nothing. It still has an enrollment
		// record and it still has completed rounds, so it reached this list twice
		// over — and a fleet reviewer change then requeued its open pull requests
		// for Pump to fire, spending quota in a repository somebody explicitly
		// stopped. An "off" that an unrelated setting can undo is not an off.
		if rec, ok := st.Enrollment(repo); ok && !rec.Enabled {
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

// AdoptedSetting is one value moved from this host's environment into the
// fleet record.
type AdoptedSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Skipped says why a setting was left alone, when it was.
	Skipped string `json:"skipped,omitempty"`
}

// AdoptEnv records this host's settings for the whole fleet.
//
// It exists because crq predates the dashboard: a fleet configured before any
// of this has every answer in one machine's env file, and the dashboard says
// "env" beside all of them — which is true, and useless. Adopting copies those
// values into the shared record, so they become the fleet's answer and every
// host reads the same one.
//
// Only settings that CAN be fleet-wide are taken. Identity (which repository
// holds the queue) and per-host values (paths, this machine's name, the fix
// agent's binary) are reported as skipped rather than silently dropped, since
// "why is that one still env" is the obvious next question.
//
// Values equal to the default are skipped too: recording them would pin a
// default that a later crq might improve, and pin it invisibly.
func (s *Service) AdoptEnv(ctx context.Context, dryRun bool) ([]AdoptedSetting, error) {
	host := s.cfg.Env()
	defaults, err := BuildConfig(map[string]string{})
	if err != nil {
		return nil, err
	}
	defaultEnv := map[string]string{}
	for _, k := range EnvKeys() {
		defaultEnv[k.Key] = defaultValueOf(defaults, k.Key)
	}

	var adopted []AdoptedSetting
	take := map[string]string{}
	for _, k := range EnvKeys() {
		value := strings.TrimSpace(host[k.Key])
		switch {
		case k.Identity:
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "identity: it says where the queue lives, not how it behaves"})
		case k.PerHost:
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "per-host: recording one machine's answer would break the others"})
		case value == "":
			// Nothing set here, so there is nothing of this host's to adopt.
		case value == defaultEnv[k.Key]:
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value,
				Skipped: "same as the default: recording it would pin today's default invisibly"})
		default:
			take[k.Key] = value
			adopted = append(adopted, AdoptedSetting{Key: k.Key, Value: value})
		}
	}
	if dryRun || len(take) == 0 {
		return adopted, nil
	}

	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		fd := st.Fleet
		changed := false
		set := func(key, value string) {
			// A record already there was set deliberately and outranks a value
			// this host happens to carry.
			if fd.Env == nil {
				fd.Env = map[string]string{}
			}
			if _, exists := fd.Env[key]; exists {
				return
			}
			fd.Env[key] = value
			changed = true
		}
		for key, value := range take {
			// Four settings have a typed home with their own validation and
			// impact preview. Adopting them into the generic map as well would
			// give one setting two places to live, one of them shadowed.
			switch key {
			case "CRQ_COBOTS":
				if !fd.SetCoBots {
					if logins, err := resolveCoBotLogins(splitCommas(value)); err == nil {
						fd.CoBots, fd.SetCoBots, changed = logins, true, true
					}
				}
			case "CRQ_REQUIRED_BOTS":
				if !fd.SetRequired {
					if logins, err := resolveRequiredLogins(splitCommas(value), s.cfg.Bot); err == nil {
						fd.Required, fd.SetRequired, changed = logins, true, true
					}
				}
			case "CRQ_MIN_INTERVAL":
				if fd.MinInterval == "" {
					fd.MinInterval, changed = value, true
				}
			case "CRQ_WEEKLY_LIMIT":
				if fd.WeeklyLimit == nil {
					if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
						fd.WeeklyLimit, changed = &n, true
					}
				}
			default:
				set(key, value)
			}
		}
		if !changed {
			return ErrNoChange
		}
		st.SetFleetDefaults(fd, s.cfg.Host, now)
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return nil, err
	}
	if err == nil {
		s.sync(ctx, st)
	}
	return adopted, nil
}

// defaultValueOf renders what a setting would be with nothing configured, so
// adoption can tell "this host chose this" from "this is just the default".
func defaultValueOf(defaults Config, key string) string {
	switch key {
	case "CRQ_MIN_INTERVAL":
		return defaults.MinInterval.String()
	case "CRQ_INFLIGHT_TIMEOUT":
		return defaults.InflightTimeout.String()
	case "CRQ_RL_FALLBACK":
		return defaults.RateLimitFallback.String()
	case "CRQ_WEEKLY_LIMIT":
		return fmt.Sprint(defaults.WeeklyReviewLimit)
	case "CRQ_AUTOREVIEW_POLL":
		return defaults.AutoReviewPoll.String()
	case "CRQ_AUTOREVIEW_MAX_SCAN":
		return fmt.Sprint(defaults.AutoReviewMaxScan)
	case "CRQ_LEADER_TTL":
		return defaults.LeaderTTL.String()
	case "CRQ_BOT":
		return defaults.Bot
	case "CRQ_REVIEW_CMD":
		return defaults.ReviewCommand
	case "CRQ_SETTLE":
		return defaults.SettleWindow.String()
	case "CRQ_FEEDBACK_WAIT_TIMEOUT":
		return defaults.FeedbackWaitTimeout.String()
	case "CRQ_WATCH_INTERVAL":
		return defaults.WatchInterval.String()
	case "CRQ_DISPATCH_MAX_ATTEMPTS":
		return fmt.Sprint(defaults.DispatchMaxAttempts)
	case "CRQ_AUTOREVIEW_SKIP_MARKER":
		return defaults.SkipMarker
	}
	return ""
}

// SetEnv records or clears one fleet setting.
func (s *Service) SetEnv(ctx context.Context, key, value string, clear bool) (FleetView, error) {
	key = strings.TrimSpace(key)
	if !fleetSettable(key) {
		if _, known := envKeyByName(key); known {
			return FleetView{}, fmt.Errorf("%s is not a fleet setting: it belongs to one machine, or says where the queue lives", key)
		}
		return FleetView{}, fmt.Errorf("%s is not a setting crq knows", key)
	}
	if !clear {
		if err := validateEnvValue(key, value); err != nil {
			return FleetView{}, err
		}
	}
	// Keys with a typed home go there, so one setting never lives in two places
	// with one of them silently shadowed by the other.
	if typedEnvKey(key) {
		change := FleetChange{}
		// Unset is its own instruction, not a change to some other value. Encoding
		// it as one meant a cleared list read as "leave it alone" and a cleared
		// weekly limit wrote 60 — so a host whose env said 90 never got it back.
		unset := clear || strings.TrimSpace(value) == ""
		switch key {
		case "CRQ_COBOTS":
			if unset {
				change.UnsetCoBots = true
			} else {
				change.CoBots = splitCommas(value)
			}
		case "CRQ_REQUIRED_BOTS":
			if unset {
				change.UnsetRequired = true
			} else {
				change.Required = splitCommas(value)
			}
		case "CRQ_MIN_INTERVAL":
			if unset {
				change.UnsetMinInterval = true
			} else {
				v := value
				change.MinInterval = &v
			}
		case "CRQ_WEEKLY_LIMIT":
			if unset {
				change.UnsetWeeklyLimit = true
			} else {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					return FleetView{}, fmt.Errorf("%s: %w", key, err)
				}
				change.WeeklyLimit = &n
			}
		}
		view, _, err := s.SetFleetSettings(ctx, change)
		return view, err
	}

	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		fd := st.Fleet
		if clear {
			if _, ok := fd.Env[key]; !ok {
				return ErrNoChange
			}
			delete(fd.Env, key)
		} else {
			if fd.Env == nil {
				fd.Env = map[string]string{}
			}
			if cur, ok := fd.Env[key]; ok && cur == value {
				return ErrNoChange
			}
			fd.Env[key] = value
		}
		st.SetFleetDefaults(fd, s.cfg.Host, now)
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return FleetView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else if st, _, err = s.store.Load(ctx); err != nil {
		return FleetView{}, err
	}
	return s.fleetViewOf(st), nil
}

// splitCommas is the list form every reviewer setting uses.
func splitCommas(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
