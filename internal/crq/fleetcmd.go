package crq

import (
	"context"
	"fmt"
	"os"
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
	HostValue string `json:"host_value,omitempty"`
	// Error is why a recorded value was not applied, when this binary could not
	// read it.
	Error string `json:"error,omitempty"`
}

// FleetConfig reports every fleet setting, in force and as this host has it.
func (s *Service) FleetConfig(ctx context.Context) ([]FleetSetting, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	settings := fleetSettings()
	effective := s.fleetCfg(st)
	out := make([]FleetSetting, 0, len(settings))
	for _, key := range FleetKeys() {
		setting := settings[key]
		item := FleetSetting{
			Key: key, Doc: setting.Doc, Env: setting.Env,
			Value: setting.Show(effective), Source: "host",
		}
		if recorded, ok := st.FleetValue(key); ok {
			item.Source = "fleet"
			if err := ValidateFleetSetting(key, recorded); err != nil {
				item.Source, item.Error = "host", err.Error()
			}
		}
		if host := setting.Show(s.cfg); host != item.Value {
			item.HostValue = host
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
	_, err := s.store.Update(ctx, func(st *State) error {
		st.SetFleetValue(key, value)
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
	dropped := false
	_, err := s.store.Update(ctx, func(st *State) error {
		dropped = st.UnsetFleetValue(key)
		if !dropped {
			return ErrNoChange
		}
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
	_, err := s.store.Update(ctx, func(st *State) error {
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
		return nil
	})
	return seeded, err
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
		case item.HostValue != "" && os.Getenv(item.Env) != "":
			out = append(out, fmt.Sprintf("%s is %q for the fleet, but %s is set to %q on this host; remove it or run crq config set %s",
				item.Key, item.Value, item.Env, item.HostValue, item.Key))
		}
	}
	return out, nil
}
