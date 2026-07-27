package crq

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// FleetSetting is one policy as `crq config` reports it: what the fleet
// records, what this host would use on its own, and which of the two is in
// effect.
type FleetSetting struct {
	Key    string `json:"key"`
	Doc    string `json:"doc"`
	Env    string `json:"env"`
	Value  string `json:"value"`
	Source string `json:"source"` // "fleet" or "host"
	// HostValue is what this machine's own environment says, reported only when
	// it differs from what is in force. It is the divergence an operator needs
	// to see: a setting they changed on this box that the fleet is overriding.
	HostValue *string `json:"host_value,omitempty"`
	// Error is why a recorded value was not applied, when this binary could not
	// read it.
	Error string `json:"error,omitempty"`
	// Lagging names active queue drivers that cannot enforce fleet policy.
	Lagging []string `json:"lagging_hosts,omitempty"`
}

// FleetConfig reports every fleet setting, in force and as this host has it.
func (s *Service) FleetConfig(ctx context.Context) ([]FleetSetting, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	settings := fleetSettings()
	effective := s.fleetCfg(st)
	lagging := st.LaggingWriters(CapsFleetPolicy, s.clock())
	out := make([]FleetSetting, 0, len(settings))
	for _, key := range FleetKeys() {
		setting := settings[key]
		item := FleetSetting{
			Key: key, Doc: setting.Doc, Env: setting.Env,
			Value: setting.Show(effective), Source: "host", Lagging: lagging,
		}
		if recorded, ok := st.FleetValue(key); ok {
			item.Source = "fleet"
			if err := ValidateFleetSetting(key, recorded); err != nil {
				item.Source, item.Error = "host", err.Error()
			}
		}
		if host := setting.Show(s.cfg); host != item.Value {
			item.HostValue = &host
		}
		out = append(out, item)
	}
	return out, nil
}

// SetFleetConfig records one setting for every host, refusing a value this
// binary cannot read: the ref is shared, so a value only some hosts can parse
// breaks the fleet from the inside.
func (s *Service) SetFleetConfig(ctx context.Context, key, value string) error {
	if err := ValidateFleetSetting(key, value); err != nil {
		return err
	}
	open, err := s.prepareFleetReviewerChange(ctx, key)
	if err != nil {
		return err
	}
	_, err = s.updateFleet(ctx, func(st *State) error {
		if err := s.requireFleetCapableDrivers(st); err != nil {
			return err
		}
		before := s.fleetCfg(*st)
		if current, ok := st.FleetValue(key); ok && current == value {
			return ErrNoChange
		}
		st.SetFleetValue(key, value)
		s.reconcileFleetChange(st, before, s.fleetCfg(*st), open)
		return nil
	})
	return err
}

// UnsetFleetConfig returns one setting to whatever each host says, reporting
// whether the fleet had an opinion to drop.
func (s *Service) UnsetFleetConfig(ctx context.Context, key string) (bool, error) {
	if _, ok := fleetSettings()[key]; !ok {
		return false, fmt.Errorf("unknown setting %q", key)
	}
	open, err := s.prepareFleetReviewerChange(ctx, key)
	if err != nil {
		return false, err
	}
	dropped := false
	_, err = s.updateFleet(ctx, func(st *State) error {
		if err := s.requireFleetCapableDrivers(st); err != nil {
			return err
		}
		before := s.fleetCfg(*st)
		dropped = st.UnsetFleetValue(key)
		if !dropped {
			return ErrNoChange
		}
		s.reconcileFleetChange(st, before, s.fleetCfg(*st), open)
		return nil
	})
	return dropped, err
}

// SeedFleetConfig records this host's settings as the fleet's, for the ones the
// fleet has no answer for yet.
//
// It is how an existing setup adopts this without retyping it: run it once on
// the machine whose configuration is the one you mean. Settings the fleet has
// already recorded are left alone — seeding twice from two machines would
// otherwise make the last one to run the winner, which is the divergence this
// whole mechanism exists to end.
func (s *Service) SeedFleetConfig(ctx context.Context) ([]string, error) {
	settings := fleetSettings()
	var seeded []string
	initial, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	var open map[string]map[int]bool
	if _, ok := initial.FleetValue("required-bots"); !ok {
		open, err = s.openFleetPRs(ctx, initial)
		if err != nil {
			return nil, err
		}
	}
	_, err = s.updateFleet(ctx, func(st *State) error {
		if err := s.requireFleetCapableDrivers(st); err != nil {
			return err
		}
		before := s.fleetCfg(*st)
		seeded = seeded[:0]
		for _, key := range FleetKeys() {
			if _, ok := st.FleetValue(key); ok {
				continue
			}
			st.SetFleetValue(key, settings[key].Show(s.cfg))
			seeded = append(seeded, key)
		}
		if len(seeded) == 0 {
			return ErrNoChange
		}
		s.reconcileFleetChange(st, before, s.fleetCfg(*st), open)
		return nil
	})
	return seeded, err
}

// updateFleet runs the exact same mutation for a preview and a real update.
// Dry-run differs only at the persistence boundary, so validation, capability
// fences and reconciliation cannot drift from the command it previews.
func (s *Service) updateFleet(ctx context.Context, mutate func(*State) error) (State, error) {
	if !s.cfg.DryRun {
		return s.store.Update(ctx, mutate)
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return State{}, err
	}
	err = mutate(&st)
	if errors.Is(err, ErrNoChange) {
		err = nil
	}
	return st, err
}

// FleetDivergence lists the settings whose value on this host differs from what
// the fleet records, for `crq doctor`.
//
// A host that quietly disagrees is the failure this is about: everything looks
// healthy on both machines, and a repository is excluded on one and reviewed by
// the other. Naming the variable is the point — the remedy is to delete it from
// this host's environment.
func (s *Service) FleetDivergence(ctx context.Context) ([]string, error) {
	items, err := s.FleetConfig(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, item := range items {
		switch {
		case item.Error != "":
			out = append(out, fmt.Sprintf("%s: the fleet's value is one this crq cannot read (%s); using this host's %q",
				item.Key, item.Error, item.Value))
		case item.HostValue != nil && s.cfg.ExplicitFleetEnv[item.Env]:
			out = append(out, fmt.Sprintf("%s is %q for the fleet, but %s is set to %q on this host; remove it or run crq config set %s",
				item.Key, item.Value, item.Env, *item.HostValue, item.Key))
		}
	}
	return out, nil
}

func (s *Service) requireFleetCapableDrivers(st *State) error {
	if lagging := st.LaggingWriters(CapsFleetPolicy, s.clock()); len(lagging) > 0 {
		return fmt.Errorf("cannot activate fleet policy while queue drivers lack fleet-policy support: %v", lagging)
	}
	return nil
}

func (s *Service) prepareFleetReviewerChange(ctx context.Context, key string) (map[string]map[int]bool, error) {
	if key != "required-bots" {
		return nil, nil
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	return s.openFleetPRs(ctx, st)
}

func (s *Service) openFleetPRs(ctx context.Context, st State) (map[string]map[int]bool, error) {
	repos := map[string]bool{}
	for _, round := range st.Rounds {
		repos[NormalizeRepo(round.Repo)] = true
	}
	open := make(map[string]map[int]bool, len(repos))
	for repo := range repos {
		pulls, err := s.openPRs(ctx, repo)
		if err != nil {
			if !inaccessibleRepoLookup(err) {
				return nil, err
			}
			// Completed rounds are permanent dedup markers, so repositories that
			// were deleted or became inaccessible remain in state indefinitely.
			// Treat their PRs as closed for this reconciliation: the rounds are
			// marked and will reopen if a later enqueue proves the PR is live.
			if s.log != nil {
				s.log.Printf("warning: reviewer change could not inspect historical repository %s: %v", repo, err)
			}
			continue
		}
		open[repo] = pulls
	}
	return open, nil
}

func inaccessibleRepoLookup(err error) bool {
	if ghapi.IsThrottled(err) {
		return false
	}
	if errors.Is(err, ghapi.ErrNotFound) {
		return true
	}
	var apiErr *ghapi.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden
}

func (s *Service) reconcileFleetChange(st *State, before, after Config, open map[string]map[int]bool) {
	if strings.Join(before.Scope, ",") != strings.Join(after.Scope, ",") {
		st.Account = AccountQuota{
			Scope:  strings.Join(after.Scope, ","),
			Source: "fleet scope changed",
		}
	}
	for _, round := range st.Rounds {
		if round.Phase != PhaseQueued && round.Phase != PhaseAwaitingRetry {
			continue
		}
		if !after.ExcludeRepos[NormalizeRepo(round.Repo)] {
			continue
		}
		st.EndRound(round.Repo, round.PR, "repository excluded by fleet policy")
		releaseSlot(st, QueueKey(round.Repo, round.PR))
	}
	s.reopenForFleetReviewerChange(st, before, after, open)
}

func (s *Service) reopenForFleetReviewerChange(st *State, before, after Config, open map[string]map[int]bool) {
	if sameLogins(before.RequiredBots, after.RequiredBots) {
		return
	}
	repos := map[string]bool{}
	for _, round := range st.Rounds {
		repos[NormalizeRepo(round.Repo)] = true
	}
	for repo := range repos {
		ov, _ := st.RepoOverride(repo)
		s.reopenForChangedReviewers(st, repo, before.ForRepo(ov), after.ForRepo(ov), open[repo])
	}
}
