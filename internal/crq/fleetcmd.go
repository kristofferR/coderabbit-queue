package crq

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	// Unknown marks a setting the fleet records that this binary has no notion
	// of — written by a newer crq. Nothing is in force for it here, so Value and
	// Env are empty and only the key is worth reporting.
	Unknown bool `json:"unknown,omitempty"`
	// Lagging names active queue drivers that cannot enforce fleet policy.
	Lagging []string `json:"lagging_hosts,omitempty"`
}

// FleetConfig reports every fleet setting, in force and as this host has it —
// including the ones only a newer crq knows. Reporting just the keys this binary
// understands would hide exactly the case that matters: a recorded setting this
// process silently ignores, while `crq config` and `crq doctor` call the host
// fully in step with the fleet.
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
	for _, key := range st.FleetKeys() {
		if _, known := settings[key]; known {
			continue
		}
		out = append(out, FleetSetting{
			Key:     key,
			Unknown: true,
			Source:  "host",
			Error:   "not a setting this crq understands; it is being ignored on this host",
			Lagging: lagging,
		})
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
	open, err := s.prepareFleetReviewerChange(ctx, key, func(st State) bool {
		current, recorded := st.FleetValue(key)
		return !recorded || current != value
	})
	if err != nil {
		return err
	}
	_, err = s.updateFleet(ctx, func(st *State) error {
		if err := s.requireFleetCapableDrivers(st); err != nil {
			return err
		}
		before := s.fleetCfg(*st)
		current, recorded := st.FleetValue(key)
		if recorded && current == value {
			return ErrNoChange
		}
		st.SetFleetValue(key, value)
		change := fleetChange{before: before, after: s.fleetCfg(*st)}
		if !recorded {
			change.adopted = []string{key}
		}
		return s.reconcileFleetChange(st, change, open)
	})
	return err
}

// UnsetFleetConfig returns one setting to whatever each host says, reporting
// whether the fleet had an opinion to drop.
//
// A key this binary does not know is refused only when the fleet has not
// recorded it either. Dropping one it HAS recorded is the remedy `crq doctor`
// names for a setting a newer crq wrote and this host ignores, and refusing it
// would leave that key unremovable from every host but the newest — but only
// while no newer binary is driving the queue, see requireNoAdvancedDrivers.
func (s *Service) UnsetFleetConfig(ctx context.Context, key string) (bool, error) {
	known := false
	if _, ok := fleetSettings()[key]; ok {
		known = true
	}
	open, err := s.prepareFleetReviewerChange(ctx, key, func(st State) bool {
		_, recorded := st.FleetValue(key)
		return recorded
	})
	if err != nil {
		return false, err
	}
	dropped := false
	_, err = s.updateFleet(ctx, func(st *State) error {
		if _, recorded := st.FleetValue(key); !known && !recorded {
			return fmt.Errorf("unknown setting %q", key)
		}
		if err := s.requireFleetCapableDrivers(st); err != nil {
			return err
		}
		if !known {
			if err := s.requireNoAdvancedDrivers(st, key); err != nil {
				return err
			}
		}
		before := s.fleetCfg(*st)
		dropped = st.UnsetFleetValue(key)
		if !dropped {
			return ErrNoChange
		}
		return s.reconcileFleetChange(st, fleetChange{before: before, after: s.fleetCfg(*st)}, open)
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
	rendered := make(map[string]string, len(settings))
	for _, key := range FleetKeys() {
		rendered[key] = settings[key].Show(s.cfg)
	}
	var seeded []string
	initial, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	var open map[string]map[int]bool
	if seedsReviewers(initial) {
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
			if err := ValidateFleetSetting(key, rendered[key]); err != nil {
				return fmt.Errorf("cannot seed %s from this host: %w", key, err)
			}
			seeded = append(seeded, key)
		}
		for _, key := range seeded {
			st.SetFleetValue(key, rendered[key])
		}
		if len(seeded) == 0 {
			return ErrNoChange
		}
		return s.reconcileFleetChange(st, fleetChange{before: before, after: s.fleetCfg(*st), adopted: seeded}, open)
	})
	return seeded, err
}

// updateFleet runs the exact same mutation for a preview and a real update.
// Dry-run differs only at the persistence boundary, so validation, capability
// fences and reconciliation cannot drift from the command it previews.
func (s *Service) updateFleet(ctx context.Context, mutate func(*State) error) (State, error) {
	changed := false
	apply := func(st *State) error {
		err := mutate(st)
		if err == nil {
			changed = true
		}
		return err
	}
	if !s.cfg.DryRun {
		st, err := s.store.Update(ctx, apply)
		if err == nil && changed {
			s.sync(ctx, st)
		}
		return st, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return State{}, err
	}
	err = apply(&st)
	if errors.Is(err, ErrNoChange) {
		err = nil
	}
	return st, err
}

// FleetDivergence lists the settings this host still answers for itself while
// the fleet records them, for `crq doctor`.
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
	settings := fleetSettings()
	for _, item := range items {
		// Every name the host is still feeding this policy from, not just the
		// canonical one: a legacy alias left behind is what the host falls back to
		// if the fleet key is ever unset.
		hostEnv := explicitEnv(s.cfg, settings[item.Key])
		switch {
		case item.Unknown:
			out = append(out, fmt.Sprintf("%s is recorded for the fleet but is not a setting this crq understands; upgrade crq on this host or run crq config unset %s",
				item.Key, item.Key))
		case item.Error != "":
			out = append(out, fmt.Sprintf("%s: the fleet's value is one this crq cannot read (%s); using this host's %q",
				item.Key, item.Error, item.Value))
		case item.Source == "fleet" && item.HostValue != nil && len(hostEnv) > 0:
			out = append(out, fmt.Sprintf("%s is %q for the fleet, but %s is set to %q on this host; remove it or run crq config set %s",
				item.Key, item.Value, strings.Join(hostEnv, ", "), *item.HostValue, item.Key))
		case item.Source == "fleet" && len(hostEnv) > 0:
			// A host copy that currently AGREES is still worth naming. Nothing
			// misbehaves while the fleet records the key — but `crq config unset`
			// hands the setting back to whatever each host says, and this host
			// then falls back to a value nobody remembers setting, diverging from
			// every host without it. That is the same failure, one command later,
			// and by then there is no recorded value left to compare against and
			// nothing here would report it.
			out = append(out, fmt.Sprintf("%s is %q for the fleet and %s is still set to that value on this host; remove it, or unsetting %s silently returns this host to its own copy",
				item.Key, item.Value, strings.Join(hostEnv, ", "), item.Key))
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

// requireNoAdvancedDrivers refuses to drop a setting this binary cannot
// interpret while a newer one is driving the queue.
//
// Removing a recorded key is the remedy `crq doctor` names for a setting a newer
// crq wrote, and it has to keep working — an unremovable key would be worse. But
// the capability fence above only asks for THIS binary's fixed CapsFleetPolicy,
// which a newer driver passes by definition, so nothing else stops an old CLI
// deleting a policy it can neither validate nor reconcile. If that key drives
// reviewers, exclusions or quota, the newer driver sees it vanish without the
// cleanup its removal calls for. The setting's own binary is the one that may
// drop it; here the remedy is to run the unset from an upgraded host.
func (s *Service) requireNoAdvancedDrivers(st *State, key string) error {
	if ahead := st.AdvancedWriters(WriterCaps, s.clock()); len(ahead) > 0 {
		return fmt.Errorf("cannot unset %q from this crq: it is not a setting this binary understands, and newer queue drivers are running: %v; upgrade crq on this host and unset it there",
			key, ahead)
	}
	return nil
}

// prepareFleetReviewerChange fetches the open PRs reconciliation needs, for the
// keys that can actually move a round.
//
// Only MEMBERSHIP asks that question. reopenForChangedReviewers compares the
// required and co-reviewer login sets and returns before touching this map when
// they match, and a co-reviewer's command, trigger mode or self-heal grace never
// moves either — so asking for the open PRs of every repository ever recorded in
// Rounds bought nothing, spent a REST lookup per historical repository, and made
// a purely local timing update fail whenever one of them was throttled.
//
// A membership key that is not actually moving asks it just as pointlessly, and
// `changes` is what settles that against the recorded value before anything is
// spent: re-recording the value the fleet already holds, or unsetting a key it
// never held, reconciles nothing, and without this an idempotent command still
// cost a lookup per historical repository and still failed under throttling. The
// recorded value is read again inside the CAS, so a concurrent write landing
// between the two leaves `open` nil rather than wrong — the same degraded case an
// inaccessible repository already produces, where a completed round is marked
// rather than reopened and the next enqueue that finds the PR alive reopens it.
func (s *Service) prepareFleetReviewerChange(ctx context.Context, key string, changes func(State) bool) (map[string]map[int]bool, error) {
	if !isReviewerMembershipFleetKey(key) {
		return nil, nil
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if !changes(st) {
		return nil, nil
	}
	return s.openFleetPRs(ctx, st)
}

// seedsReviewers reports whether seeding would record a reviewer setting the
// fleet has no answer for yet, which is the case where the effective reviewers
// can move and existing rounds have to be reconciled against them. Same
// membership-only question as prepareFleetReviewerChange, for the same reason.
func seedsReviewers(st State) bool {
	for _, key := range FleetKeys() {
		if !isReviewerMembershipFleetKey(key) {
			continue
		}
		if _, ok := st.FleetValue(key); !ok {
			return true
		}
	}
	return false
}

// fleetChange is one policy mutation as reconciliation sees it: the effective
// configuration before and after it, plus the keys the fleet had no answer for
// until now.
//
// Those adopted keys are why `before` cannot be taken at face value. fleetCfg
// fills an unrecorded setting from THIS host, so the first `crq config set` or
// `crq config seed` of a value this machine was already using renders before and
// after identical — while every other host may have been acting under a
// different one all along. For an adopted key the fleet-wide baseline is
// unknown, and reconciliation has to assume it differed rather than assume it
// matched.
type fleetChange struct {
	before, after Config
	adopted       []string
}

func (c fleetChange) adopts(key string) bool {
	for _, adopted := range c.adopted {
		if adopted == key {
			return true
		}
	}
	return false
}

// baseline is the pre-change configuration with each adopted key's host fallback
// removed, so a policy the fleet is only now recording is reconciled as the
// change it is for everyone else.
//
// A reviewer MEMBERSHIP key drops the whole reviewer set: another host may have
// completed a head without a bot the adoption makes fleet-required, and that
// host's completed round is the dedup marker that would keep the newly required
// bot from ever being triggered. It is conservative, not expensive — a reopened
// round the primary already answered dedupes at DecideFire's already-reviewed
// gate instead of buying a second review, and a repository that pins its own
// reviewers still decides its effective set, so its rounds compare equal and are
// left alone. Adopting `exclude` likewise drops the baseline exclusions, so a
// repository this host already skipped still has to pass the claimed-trigger
// refusal that the rest of the fleet never applied to it.
//
// The per-bot keys are deliberately not in that set. Adopting a co-reviewer's
// command, trigger mode or self-heal grace moves nobody in or out of the
// reviewer set, and reconciliation compares membership — so erasing the baseline
// for one would reopen every completed round in the fleet and force a self-heal
// trigger post on every open PR, for a timing value. Ordinary updates to the
// same key reopen nothing, and adoption has no more to reconcile than they do.
func (c fleetChange) baseline() Config {
	before := c.before
	for _, key := range c.adopted {
		if isReviewerMembershipFleetKey(key) {
			before.RequiredBots, before.CoBots, before.Reviewers = nil, nil, nil
		}
		if key == "exclude" {
			before.ExcludeRepos = nil
		}
	}
	return before
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

// reconcileFleetChange applies what a policy change invalidates — and refuses
// the change outright when there is a network effect it cannot take back.
func (s *Service) reconcileFleetChange(st *State, change fleetChange, open map[string]map[int]bool) error {
	before, after := change.baseline(), change.after
	if err := checkExcludedTriggerPosts(st, before, after); err != nil {
		return err
	}
	if fleetScopeMoved(st, change) {
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
		// A queued round holds no slot, so this releases one only when the round
		// it retires is the one that took it — which it never is here. An orphaned
		// hold at the same key stands for a metered command a previous round
		// posted and is left alone: excluding a repository stops the next review,
		// it does not answer the one already in flight.
		st.EndRound(round.Repo, round.PR, "repository excluded by fleet policy")
		releaseSlot(st, QueueKey(round.Repo, round.PR), round.Token)
	}
	s.reopenForFleetReviewerChange(st, before, after, open)
	return nil
}

// checkExcludedTriggerPosts refuses an exclusion that would race a trigger post
// already claimed for the repository being excluded.
//
// The reservation is a fire's commit point, but the command goes to GitHub
// after it: a round left reserved here is one whose post is already authorized,
// so retiring it would not stop the excluded repository receiving a review
// command — it would only destroy the record of quota the account is about to
// be charged for. As with a hold, the claim and this write are both CAS, which
// makes either ordering safe: an existing claim rejects the exclusion, and an
// exclusion recorded first stops the next fire.
func checkExcludedTriggerPosts(st *State, before, after Config) error {
	var blocked []string
	for _, round := range st.Rounds {
		repo := NormalizeRepo(round.Repo)
		if !after.ExcludeRepos[repo] || before.ExcludeRepos[repo] {
			continue
		}
		if triggerPostClaimed(&round) {
			blocked = append(blocked, QueueKey(round.Repo, round.PR))
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	sort.Strings(blocked)
	return fmt.Errorf("a review trigger is already being posted for %s; wait for it to finish before excluding it",
		strings.Join(blocked, ", "))
}

// fleetScopeMoved reports whether the recorded account quota belongs to an
// account other than the one the fleet now scans — which is what makes it
// meaningless rather than merely stale.
//
// Ordinarily that is just "did the scope change". Adopting the scope needs the
// other question: fleetCfg fills an unrecorded setting from THIS host, so the
// first `crq config set scope` compares equal to itself even when the fleet was
// scanning something else entirely. The quota's own recorded scope is the one
// piece of that account's identity that survived, so ask it instead.
func fleetScopeMoved(st *State, change fleetChange) bool {
	if change.adopts("scope") {
		return !sameFoldedSet(splitList(st.Account.Scope), change.after.Scope)
	}
	return !sameFoldedSet(change.before.Scope, change.after.Scope)
}

func sameFoldedSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, value := range a {
		seen[strings.ToLower(strings.TrimSpace(value))]++
	}
	for _, value := range b {
		key := strings.ToLower(strings.TrimSpace(value))
		seen[key]--
		if seen[key] < 0 {
			return false
		}
	}
	return true
}

// reopenForFleetReviewerChange requeues what a fleet reviewer change invalidates.
// Which rounds those are is decided per repository, against that repository's
// override — the fleet's required set is only half of the effective one, and a
// co-reviewer the fleet enables or disables moves it just as much.
func (s *Service) reopenForFleetReviewerChange(st *State, before, after Config, open map[string]map[int]bool) {
	repos := map[string]bool{}
	for _, round := range st.Rounds {
		repos[NormalizeRepo(round.Repo)] = true
	}
	for repo := range repos {
		ov, _ := st.RepoOverride(repo)
		s.reopenForChangedReviewers(st, repo, before.ForRepo(ov), after.ForRepo(ov), open[repo])
	}
}
