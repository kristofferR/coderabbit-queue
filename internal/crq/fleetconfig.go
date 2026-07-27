package crq

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// fleetSetting is one policy the whole fleet shares: how to read it from the
// state ref onto a Config, and how to render what a host is currently using.
//
// The registry below is the definition of "fleet policy". A setting listed here
// stops being something each machine answers for itself; one that is not listed
// stays local, and the split is deliberate — see FleetKeys.
type fleetSetting struct {
	// Doc is what the setting means, for `crq config`.
	Doc string
	// Env names the variable it used to come from, so an operator can find it.
	Env string
	// Apply writes value onto cfg, rejecting a value it cannot parse. A bad
	// value must fail where it is SET, but it is validated here too: the state
	// ref is shared, and a host reading a value it cannot use has to say so
	// rather than silently keep its own.
	Apply func(cfg *Config, value string) error
	// Show renders what cfg is currently using, for comparison.
	Show func(cfg Config) string
}

// fleetSettings is every setting that belongs to the fleet rather than to a
// machine.
//
// What is NOT here matters as much. Three kinds stay local: where the state
// lives (CRQ_REPO, CRQ_ISSUE, CRQ_STATE_REF — a host cannot read fleet config
// until it knows where to look), credentials, and what this machine can
// physically do (which fix agent is installed, where its disk is, how many
// sessions it can take). Everything else is policy, and policy the hosts
// disagree about is how a repository ends up excluded on one and reviewed by
// another with nothing to say so.
func fleetSettings() map[string]fleetSetting {
	return map[string]fleetSetting{
		"scope": {
			Doc: "owners crq scans when no repository list is set",
			Env: "CRQ_SCOPE",
			Apply: func(cfg *Config, v string) error {
				cfg.Scope = splitList(v)
				return nil
			},
			Show: func(cfg Config) string { return strings.Join(cfg.Scope, ",") },
		},
		"repos": {
			Doc: "the repositories crq reviews and watches",
			Env: "CRQ_REPOS",
			Apply: func(cfg *Config, v string) error {
				cfg.AllowRepos = repoSet(v)
				return nil
			},
			Show: func(cfg Config) string { return strings.Join(sortedRepoKeys(cfg.AllowRepos), ",") },
		},
		"exclude": {
			Doc: "repositories crq never reviews, watches or fixes",
			Env: "CRQ_EXCLUDE",
			Apply: func(cfg *Config, v string) error {
				cfg.ExcludeRepos = repoSet(v)
				return nil
			},
			Show: func(cfg Config) string { return strings.Join(sortedRepoKeys(cfg.ExcludeRepos), ",") },
		},
		"required-bots": {
			Doc: "logins that must review a head before a round converges",
			Env: "CRQ_REQUIRED_BOTS",
			Apply: func(cfg *Config, v string) error {
				cfg.RequiredBots = splitList(v)
				return nil
			},
			Show: func(cfg Config) string { return strings.Join(cfg.RequiredBots, ",") },
		},
		"min-interval": {
			Doc: "floor between two metered reviews",
			Env: "CRQ_MIN_INTERVAL",
			Apply: func(cfg *Config, v string) error {
				d, err := parseFleetDuration(v)
				cfg.MinInterval = d
				return err
			},
			Show: func(cfg Config) string { return cfg.MinInterval.String() },
		},
		"rate-limit-fallback": {
			Doc: "how long to wait when a rate-limit notice states no window",
			Env: "CRQ_RL_FALLBACK",
			Apply: func(cfg *Config, v string) error {
				d, err := parseFleetDuration(v)
				cfg.RateLimitFallback = d
				return err
			},
			Show: func(cfg Config) string { return cfg.RateLimitFallback.String() },
		},
		"calibrate-ttl": {
			Doc: "how long a calibration answer counts as current",
			Env: "CRQ_CALIBRATE_TTL",
			Apply: func(cfg *Config, v string) error {
				d, err := parseFleetDuration(v)
				cfg.CalibrationTTL = d
				return err
			},
			Show: func(cfg Config) string { return cfg.CalibrationTTL.String() },
		},
		"settle": {
			Doc: "quiet window before a PR counts as converged",
			Env: "CRQ_SETTLE",
			Apply: func(cfg *Config, v string) error {
				d, err := parseFleetDuration(v)
				cfg.SettleWindow = d
				return err
			},
			Show: func(cfg Config) string { return cfg.SettleWindow.String() },
		},
		"skip-marker": {
			Doc: "text in a PR body that opts it out of review (empty disables the opt-out)",
			Env: "CRQ_AUTOREVIEW_SKIP_MARKER",
			Apply: func(cfg *Config, v string) error {
				cfg.SkipMarker = v
				return nil
			},
			Show: func(cfg Config) string { return cfg.SkipMarker },
		},
	}
}

// FleetKeys lists every setting the fleet owns, in a stable order.
func FleetKeys() []string {
	settings := fleetSettings()
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// parseFleetDuration rejects what a Go duration cannot express, and refuses a
// negative one: every setting here is a window, and a negative window is a
// setting that would make crq act immediately, for ever.
func parseFleetDuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("not a duration (try 90s, 15m, 2h): %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must not be negative, got %s", d)
	}
	return d, nil
}

// ValidateFleetSetting reports whether key is a fleet setting and value is one
// it can hold. Set fails on a bad value rather than writing it: the state ref
// is shared, so a value only one host can parse is a value that breaks the
// fleet from the inside.
func ValidateFleetSetting(key, value string) error {
	setting, ok := fleetSettings()[key]
	if !ok {
		return fmt.Errorf("unknown setting %q (try one of: %s)", key, strings.Join(FleetKeys(), ", "))
	}
	probe := Config{}
	return setting.Apply(&probe, value)
}

// applyFleet overlays the fleet's recorded policy onto a host's configuration.
//
// Recorded wins. The environment is what a host uses until the fleet has an
// opinion — which is what makes adopting this safe on a machine at a time, and
// what `crq config seed` writes from.
//
// A value this binary cannot parse is LEFT to the host rather than applied
// half-way: a newer crq may have widened what it accepts, and acting on a
// partially-read policy is worse than acting on the one already in hand. The
// disagreement surfaces in `crq doctor`, not silently here.
func applyFleet(cfg Config, fleet map[string]string, warn func(string)) Config {
	settings := fleetSettings()
	for _, key := range sortedKeys(fleet) {
		setting, ok := settings[key]
		if !ok {
			if warn != nil {
				warn(fmt.Sprintf("fleet setting %q is not one this crq understands; ignoring it", key))
			}
			continue
		}
		candidate := cfg
		if err := setting.Apply(&candidate, fleet[key]); err != nil {
			if warn != nil {
				warn(fmt.Sprintf("fleet setting %q: %v; keeping this host's value", key, err))
			}
			continue
		}
		cfg = candidate
	}
	return cfg
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedRepoKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
