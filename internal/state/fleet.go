package state

import (
	"sort"
	"strings"
	"time"
)

// Fleet is the policy every host in the fleet shares, keyed by setting name.
//
// It lives HERE for the reason the per-repository overrides do, one level up:
// a daemon on one machine and an agent on another reading different
// configurations while writing one shared state ref is a class of divergence
// worth not having. Per-host environment files diverge the moment somebody
// edits one — a repository excluded on the laptop and reviewed by the server,
// a rate-limit window one host respects and another does not — and nothing
// says so, because each host is behaving correctly according to what it can
// see.
//
// A flat map rather than a struct, on purpose. One JSON member means an older
// binary round-trips the whole thing rather than dropping settings it does not
// know. Newly interpreted decision-changing keys still require a writer
// capability bump so an active older driver cannot ignore them.
type Fleet map[string]string

// FleetValue returns a fleet setting and whether it is recorded. Absent means
// "the fleet has no opinion", which is what lets a host fall back to its own
// environment and to crq's defaults.
func (s *State) FleetValue(key string) (string, bool) {
	value, ok := s.FleetConfig[key]
	return value, ok
}

// SetFleetValue records a fleet setting, replacing any earlier value.
func (s *State) SetFleetValue(key, value string) {
	if s.FleetConfig == nil {
		s.FleetConfig = Fleet{}
	}
	s.FleetConfig[key] = value
}

// UnsetFleetValue drops a setting, returning the fleet to per-host defaults for
// it, and reports whether one was there.
func (s *State) UnsetFleetValue(key string) bool {
	if _, ok := s.FleetConfig[key]; !ok {
		return false
	}
	delete(s.FleetConfig, key)
	return true
}

// FleetKeys lists the recorded settings in a stable order.
func (s *State) FleetKeys() []string {
	keys := make([]string, 0, len(s.FleetConfig))
	for key := range s.FleetConfig {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// withFleet applies the state-backed settings the state package itself renders.
// The full policy is interpreted by crq; MinInterval also shapes queue
// readiness in dashboards/status, including GitStateStore's background sync.
//
// Trimmed the way crq's own fleet parser trims, because the recorded value is
// whatever an operator typed: a stored " 5m " that queue decisions honour must
// not leave the dashboard and status line advertising this host's interval.
func (c StoreConfig) withFleet(s State) StoreConfig {
	if value, ok := s.FleetValue("min-interval"); ok {
		if interval, err := time.ParseDuration(strings.TrimSpace(value)); err == nil && interval >= 0 {
			c.MinInterval = interval
		}
	}
	return c
}
