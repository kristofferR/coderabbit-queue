package crq

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
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
	settings := map[string]fleetSetting{
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
				repos, err := fleetRepoSet(v)
				cfg.AllowRepos = repos
				return err
			},
			Show: func(cfg Config) string { return strings.Join(sortedSetKeys(cfg.AllowRepos), ",") },
		},
		"exclude": {
			Doc: "repositories crq never reviews, watches or fixes",
			Env: "CRQ_EXCLUDE",
			Apply: func(cfg *Config, v string) error {
				repos, err := fleetRepoSet(v)
				cfg.ExcludeRepos = repos
				return err
			},
			Show: func(cfg Config) string { return strings.Join(sortedSetKeys(cfg.ExcludeRepos), ",") },
		},
		"required-bots": {
			Doc: "logins that must review a head before a round converges",
			Env: "CRQ_REQUIRED_BOTS",
			Apply: func(cfg *Config, v string) error {
				cfg.RequiredBots = splitList(v)
				if len(cfg.RequiredBots) == 0 {
					return fmt.Errorf("at least one required bot is needed")
				}
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
		"inflight-timeout": {
			Doc: "how long a metered review command may remain unanswered",
			Env: "CRQ_INFLIGHT_TIMEOUT",
			Apply: func(cfg *Config, v string) error {
				d, err := parseFleetDuration(v)
				cfg.InflightTimeout = d
				return err
			},
			Show: func(cfg Config) string { return cfg.InflightTimeout.String() },
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
		"skip-authors": {
			Doc: "PR authors autoreview never enqueues (empty reviews every author)",
			Env: "CRQ_AUTOREVIEW_SKIP_AUTHORS",
			Apply: func(cfg *Config, v string) error {
				cfg.SkipAuthors = authorSet(v)
				return nil
			},
			Show: func(cfg Config) string { return strings.Join(sortedSetKeys(cfg.SkipAuthors), ",") },
		},
		"cobots": {
			Doc: "co-reviewers crq surfaces and triggers (empty disables all)",
			Env: "CRQ_COBOTS",
			Apply: func(cfg *Config, v string) error {
				names, err := fleetCoBotNames(v)
				if err != nil {
					return err
				}
				*cfg = cfg.ForRepo(RepoReviewers{CoBots: names, SetCoBots: true})
				return nil
			},
			Show: func(cfg Config) string {
				names := make([]string, 0, len(cfg.CoBots))
				for _, cb := range cfg.CoBots {
					names = append(names, cb.Name)
				}
				return strings.Join(names, ",")
			},
		},
		"rate-limit-co-degrade": {
			Doc: "run co-reviewer-only rounds while the CodeRabbit account is blocked",
			Env: "CRQ_RL_CO_DEGRADE",
			Apply: func(cfg *Config, v string) error {
				on, err := parseFleetBool(v)
				if err != nil {
					return err
				}
				cfg.RateLimitCoDegrade = on
				return nil
			},
			Show: func(cfg Config) string { return fleetBool(cfg.RateLimitCoDegrade) },
		},
	}
	// Every co-reviewer's own policy, one family per bot. Nothing here names a
	// bot: the registry is what says which exist, so a new co-reviewer arrives
	// with its fleet settings already in place.
	for _, co := range dialect.KnownCoReviewers() {
		for key, setting := range coBotFleetSettings(co) {
			settings[key] = setting
		}
	}
	return settings
}

// coBotFleetSettings is how the fleet drives one co-reviewer: whether crq posts
// its command, what it posts, and how long a self-heal nudge waits first.
//
// Required-ness is deliberately NOT here — required-bots owns that list, and two
// settings answering the same question is how they end up disagreeing.
func coBotFleetSettings(co dialect.CoReviewer) map[string]fleetSetting {
	name := co.Name
	env := "CRQ_COBOT_" + strings.ToUpper(name)
	return map[string]fleetSetting{
		"cobot-" + name + "-trigger": {
			Doc: "when crq posts " + name + "'s command: never, selfheal or always (empty leaves it to how the bot is required)",
			Env: env + "_TRIGGER",
			Apply: func(cfg *Config, v string) error {
				value := strings.ToLower(strings.TrimSpace(v))
				if value == "" {
					// The environment's "unset" — the registry default for how the
					// bot is required, recomputed rather than frozen.
					updateCoBot(cfg, name, func(cb *CoBotConfig) { cb.TriggerExplicit = false })
					return nil
				}
				mode := engine.TriggerMode(value)
				switch mode {
				case engine.TriggerNever, engine.TriggerSelfHeal, engine.TriggerAlways:
				default:
					return fmt.Errorf("trigger must be never, selfheal or always, got %q", v)
				}
				updateCoBot(cfg, name, func(cb *CoBotConfig) {
					cb.Trigger, cb.TriggerExplicit = mode, true
				})
				return nil
			},
			Show: func(cfg Config) string {
				if cb := coBotOf(cfg, name); cb.TriggerExplicit {
					return string(cb.Trigger)
				}
				return ""
			},
		},
		"cobot-" + name + "-cmd": {
			Doc: "the comment that triggers " + name + " (empty means crq never posts one)",
			Env: env + "_CMD",
			Apply: func(cfg *Config, v string) error {
				command := strings.TrimSpace(v)
				updateCoBot(cfg, name, func(cb *CoBotConfig) { cb.Command = command })
				return nil
			},
			Show: func(cfg Config) string { return coBotOf(cfg, name).Command },
		},
		"cobot-" + name + "-grace": {
			Doc: "how long a selfheal trigger waits for " + name + " to show up on its own",
			Env: env + "_GRACE",
			Apply: func(cfg *Config, v string) error {
				d, err := parseFleetDuration(v)
				if err != nil {
					return err
				}
				updateCoBot(cfg, name, func(cb *CoBotConfig) { cb.SelfHealGrace = d })
				return nil
			},
			Show: func(cfg Config) string { return coBotOf(cfg, name).SelfHealGrace.String() },
		},
	}
}

// coBotOf is how this host currently drives a co-reviewer: its enabled entry
// when it has one, otherwise what its environment resolved for a bot the fleet
// leaves disabled — which is still the configuration a repository override
// would pick it up with.
func coBotOf(cfg Config, name string) CoBotConfig {
	for _, cb := range cfg.CoBots {
		if strings.EqualFold(cb.Name, name) {
			return cb
		}
	}
	cb, _ := cfg.knownCoBot(name)
	return cb
}

// updateCoBot changes one co-reviewer wherever its configuration is held: the
// enabled set crq drives, and the registry-wide set a per-repo override draws
// from for a bot the fleet does not enable. Both are copied first — the caller's
// Config shares these slices, and a setting that fails to parse must leave it
// untouched.
func updateCoBot(cfg *Config, name string, mutate func(*CoBotConfig)) {
	cfg.CoBots = updateCoBotIn(cfg.CoBots, name, mutate)
	// The registry-wide set is what a bot this host never resolved is enabled
	// from — by the fleet's own cobots setting, or by a repository override — so
	// it has to hold the policy even when there is no entry to change yet.
	// Without this the fleet's answer would be silently dropped, which is the
	// failure the shared registry exists to prevent.
	if !hasCoBot(cfg.KnownCoBots, name) {
		if co, ok := dialect.CoReviewerByName(name); ok {
			cfg.KnownCoBots = append(append([]CoBotConfig(nil), cfg.KnownCoBots...), defaultCoBot(co, false))
		}
	}
	cfg.KnownCoBots = updateCoBotIn(cfg.KnownCoBots, name, mutate)
}

func hasCoBot(bots []CoBotConfig, name string) bool {
	for _, cb := range bots {
		if strings.EqualFold(cb.Name, name) {
			return true
		}
	}
	return false
}

func updateCoBotIn(bots []CoBotConfig, name string, mutate func(*CoBotConfig)) []CoBotConfig {
	out := append([]CoBotConfig(nil), bots...)
	for i, cb := range out {
		if !strings.EqualFold(cb.Name, name) {
			continue
		}
		mutate(&cb)
		// Re-derive what the change implies, exactly as the environment parse
		// does: an implicit trigger follows the registry default for how the bot
		// is required, and no command means crq can never post one.
		cb = reconcileTrigger(cb)
		if cb.Command == "" {
			cb.Trigger = engine.TriggerNever
		}
		out[i] = cb
	}
	return out
}

// fleetCoBotNames resolves a recorded co-reviewer list to registry names,
// refusing one this binary does not know rather than skipping it: a silently
// dropped bot looks exactly like one that is simply never triggered.
func fleetCoBotNames(value string) ([]string, error) {
	var names, unknown []string
	for _, item := range splitList(value) {
		co, ok := dialect.CoReviewerByName(item)
		if !ok {
			unknown = append(unknown, item)
			continue
		}
		names = append(names, co.Name)
	}
	if len(unknown) > 0 {
		known := make([]string, 0, 3)
		for _, co := range dialect.KnownCoReviewers() {
			known = append(known, co.Name)
		}
		return nil, fmt.Errorf("unknown co-reviewer %s (known: %s)",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return names, nil
}

// isReviewerFleetKey reports whether a setting reshapes who reviews, or how a
// co-reviewer is driven. Those are the ones whose derived views have to be
// rebuilt, and whose changes existing rounds may have to be reconciled against.
func isReviewerFleetKey(key string) bool {
	return key == "required-bots" || key == "cobots" || strings.HasPrefix(key, "cobot-")
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

// parseFleetBool reads the spellings the environment already accepts, so a
// seeded value and a hand-typed one mean the same thing.
func parseFleetBool(v string) (bool, error) {
	switch value := strings.ToLower(strings.TrimSpace(v)); value {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("not a switch (try 1 or 0): %w", err)
		}
		return b, nil
	}
}

func fleetBool(on bool) string {
	if on {
		return "1"
	}
	return "0"
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

func fleetRepoSet(value string) (map[string]bool, error) {
	repos := repoSet(value)
	for repo := range repos {
		if !validRepoSlug(repo) {
			return nil, fmt.Errorf("repo must be owner/name, got %q", repo)
		}
	}
	return repos, nil
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
	// The reviewer settings are the inputs to the derived reviewer views.
	// Applying one as a scalar without rebuilding those views can leave a
	// required co-reviewer disabled, carrying its optional trigger mode, or
	// still driven by the command this host was started with.
	if touchesReviewers(fleet) {
		// A primary that is also a registry bot is triggered as the primary;
		// asking it twice is the bug, and a fleet trigger for it must not undo
		// the silencing LoadConfig applied.
		cfg.CoBots = silenceTrigger(cfg.CoBots, cfg.Bot)
		enabled := make([]string, 0, len(cfg.CoBots))
		for _, cb := range cfg.CoBots {
			enabled = append(enabled, cb.Login)
		}
		cfg = cfg.ForRepo(RepoReviewers{
			CoBots:      enabled,
			SetCoBots:   true,
			Required:    cfg.RequiredBots,
			SetRequired: true,
		})
	}
	return cfg
}

func touchesReviewers(fleet map[string]string) bool {
	for key := range fleet {
		if isReviewerFleetKey(key) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSetKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
