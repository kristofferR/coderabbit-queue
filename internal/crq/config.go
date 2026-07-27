package crq

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

const Version = "2.0.0-dev"

// The GitHub transport tags its User-Agent with the crq version. Version lives
// in this package, so the wiring stays here now that the gh alias layer is gone.
func init() { ghapi.UserAgent = "crq/" + Version }

type Config struct {
	GateRepo       string
	DashboardIssue int
	CalibrationPR  int
	Scope          []string
	AllowRepos     map[string]bool
	ExcludeRepos   map[string]bool
	// SkipAuthors lists PR authors autoreview never enqueues (normalized: lowercase,
	// no "[bot]" suffix). Defaults to dependabot; set CRQ_AUTOREVIEW_SKIP_AUTHORS=""
	// to review bot PRs too. Manual `crq review` is unaffected.
	SkipAuthors map[string]bool
	// SkipMarker suppresses fleet auto-review when present in a PR body.
	// Manual `crq loop` remains unaffected so an explicit review can override it.
	SkipMarker    string
	StateRef      string
	Bot           string
	RequiredBots  []string
	FeedbackBots  []string
	ReviewCommand string
	// CoBots are the enabled co-reviewer bots (CRQ_COBOTS + per-bot
	// CRQ_COBOT_<NAME>_* keys). An entry exists for every wanted or required
	// co-reviewer; required ones are already folded into RequiredBots.
	CoBots []CoBotConfig
	// Reviewers is the single description of who reviews and what they cost.
	// Bot / RequiredBots / FeedbackBots / CoBots above are DERIVED from it and
	// kept only so existing consumers keep compiling; new code should read this.
	Reviewers []Reviewer
	// WatchInterval paces `crq watch`; DispatchCommand is the fix session it
	// runs with --dispatch, argv-style; DispatchMaxAttempts bounds dispatches per
	// head so a fix that keeps not working stops.
	WatchInterval       time.Duration
	DispatchCommand     []string
	DispatchMaxAttempts int
	// DispatchConcurrency caps concurrent fix sessions. 0 (the default) means no
	// cap: fixing findings spends no account quota, so it does not belong in a
	// queue. It is a resource valve for a machine that cannot take the load.
	DispatchConcurrency int
	// WorkspaceRoot holds crq's own mirrors and worktrees. Read here rather than
	// from the process environment, so a value in ~/.config/crq/env — the
	// documented place for crq settings — is actually used.
	WorkspaceRoot string
	// WorkDir is the checkout the local-work probe inspects. Empty means the
	// process's own directory, which is what an agent running crq from its
	// working copy means. Set programmatically by a caller working in a
	// worktree it made, since that caller is not standing in one.
	WorkDir           string
	RateLimitCommand  string
	RateLimitMarker   string
	CalibrationMarker string
	ReviewDoneMarker  string
	// CompletionMarker identifies the bot's reply to a processed review command
	// (CodeRabbit: "Review finished."). Feedback uses it to count a command
	// round that produced no review object toward convergence.
	CompletionMarker    string
	Host                string
	Timezone            string
	MinInterval         time.Duration
	InflightTimeout     time.Duration
	PollInterval        time.Duration
	WaitTimeout         time.Duration
	CalibrationTTL      time.Duration
	RateLimitFallback   time.Duration
	AutoReviewPoll      time.Duration
	AutoReviewMaxScan   int
	LeaderTTL           time.Duration
	FiredMax            int
	NoOpen              bool
	DryRun              bool
	FeedbackWaitTimeout time.Duration
	// SettleWindow keeps a converged loop polling briefly before it exits 0, so
	// a trailing review wave (a Codex auto-review of the just-pushed head, a
	// CodeRabbit review following its comment shells) is caught by crq instead
	// of by a human re-checking the PR. 0 disables.
	SettleWindow time.Duration
	// RateLimitCoDegrade degrades an account-blocked round to co-reviewers only
	// (return their findings promptly, keep CodeRabbit queued for the window)
	// instead of waiting the block out. CRQ_RL_CO_DEGRADE, default on.
	RateLimitCoDegrade bool
}

// ConfigPath is the file crq reads its settings from: CRQ_CONFIG, or
// ~/.config/crq/env. Empty when neither can be resolved.
//
// Exported because a service unit has to be pointed at the SAME file the install
// read. A drain that loads a different configuration is one that watches nothing
// while reporting itself started.
func ConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("CRQ_CONFIG")); path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "crq", "env")
}

func LoadConfig() (Config, error) {
	env := map[string]string{}
	configPath := ConfigPath()
	if configPath != "" {
		values, err := readEnvFile(configPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
		for k, v := range values {
			env[k] = v
		}
		// The GitHub credential is resolved from the PROCESS environment
		// (internal/gh, and `git` through it), not from this map — so a token
		// configured here would authenticate nothing. Exporting it is what lets a
		// service unit carry no secret of its own: point it at this file.
		for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
			if v := strings.TrimSpace(values[key]); v != "" && os.Getenv(key) == "" {
				os.Setenv(key, v)
			}
		}
	}
	for _, e := range os.Environ() {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			env[k] = v
		}
	}

	host, _ := os.Hostname()
	bot := stringEnv(env, "CRQ_BOT", "coderabbitai[bot]")
	requiredBots := listEnv(env, "CRQ_REQUIRED_BOTS", bot)
	coBots, err := parseCoBots(env, requiredBots)
	if err != nil {
		return Config{}, err
	}
	// A required co-reviewer gates convergence via RequiredBots membership —
	// that list stays the single source of required-ness.
	for _, cb := range coBots {
		if cb.Required {
			requiredBots = unionBots(requiredBots, []string{cb.Login})
		}
	}
	// CRQ_BOT may name a registry bot — pointing crq at Codex as the primary is a
	// real configuration. It is then the primary and must not ALSO be driven as a
	// co-reviewer: DecideFire posts its review command and fireCoOnly would post
	// its co-reviewer trigger, asking the same reviewer twice.
	//
	// Silenced, not removed. The registry entry is also where that bot's wording
	// and check-run hooks come from (classifierCoReviewers, coChecksRelevant both
	// walk CoBots), so dropping it would cost the primary its evidence: a Codex
	// clean summary would read as a generic no-action event, and a check-only
	// result would not be fetched at all — crq would fire and then time out with
	// current-head evidence sitting in front of it. Every trigger post goes
	// through engine.DecideCoPost, which posts nothing for a never trigger.
	coBots = silenceTrigger(coBots, bot)
	// Enabled co-reviewers surface findings without gating: their logins join
	// the feedback set unless CRQ_FEEDBACK_BOTS overrides explicitly.
	coLogins := make([]string, 0, len(coBots))
	for _, cb := range coBots {
		coLogins = append(coLogins, cb.Login)
	}
	cfg := Config{
		GateRepo:            env["CRQ_REPO"],
		DashboardIssue:      intEnv(env, "CRQ_ISSUE", 0),
		CalibrationPR:       intEnv(env, "CRQ_CAL_PR", 0),
		Scope:               listEnv(env, "CRQ_SCOPE", ownerOf(env["CRQ_REPO"])),
		AllowRepos:          repoSet(env["CRQ_REPOS"]),
		ExcludeRepos:        repoSet(env["CRQ_EXCLUDE"]),
		SkipAuthors:         authorSet(stringEnvAllowEmpty(env, "CRQ_AUTOREVIEW_SKIP_AUTHORS", "dependabot[bot]")),
		SkipMarker:          stringEnvAllowEmpty(env, "CRQ_AUTOREVIEW_SKIP_MARKER", "<!-- crq:skip-autoreview -->"),
		StateRef:            stringEnv(env, "CRQ_STATE_REF", "crq-state-v3"),
		Bot:                 bot,
		RequiredBots:        requiredBots,
		CoBots:              coBots,
		FeedbackBots:        listEnv(env, "CRQ_FEEDBACK_BOTS", strings.Join(unionBots(requiredBots, coLogins), ",")),
		ReviewCommand:       stringEnv(env, "CRQ_REVIEW_CMD", "@coderabbitai review"),
		RateLimitCommand:    stringEnv(env, "CRQ_RATELIMIT_CMD", dialect.DefaultRateLimitCommand),
		RateLimitMarker:     stringEnv(env, "CRQ_RL_MARKER", dialect.DefaultRateLimitMarker),
		CalibrationMarker:   stringEnv(env, "CRQ_CAL_REPLY_MARKER", "auto-generated reply by CodeRabbit"),
		ReviewDoneMarker:    stringEnv(env, "CRQ_REVIEW_DONE_MARKER", "summarize by coderabbit.ai"),
		CompletionMarker:    stringEnvAllowEmpty(env, "CRQ_COMPLETION_MARKER", "Review finished"),
		Host:                stringEnv(env, "CRQ_HOST", host),
		Timezone:            env["CRQ_TZ"],
		MinInterval:         durationEnv(env, "CRQ_MIN_INTERVAL", 90*time.Second),
		InflightTimeout:     durationEnv(env, "CRQ_INFLIGHT_TIMEOUT", 15*time.Minute),
		PollInterval:        durationEnv(env, "CRQ_POLL", 15*time.Second),
		WaitTimeout:         durationEnv(env, "CRQ_WAIT_TIMEOUT", 0),
		CalibrationTTL:      durationEnv(env, "CRQ_CALIBRATE_TTL", 2*time.Minute),
		RateLimitFallback:   durationEnv(env, "CRQ_RL_FALLBACK", 15*time.Minute),
		AutoReviewPoll:      durationEnv(env, "CRQ_AUTOREVIEW_POLL", time.Minute),
		AutoReviewMaxScan:   intEnv(env, "CRQ_AUTOREVIEW_MAX_SCAN", 400),
		LeaderTTL:           durationEnv(env, "CRQ_LEADER_TTL", 3*time.Minute),
		FiredMax:            intEnv(env, "CRQ_FIRED_MAX", 500),
		WatchInterval:       durationEnv(env, "CRQ_WATCH_INTERVAL", 2*time.Minute),
		DispatchCommand:     splitArgv(env["CRQ_DISPATCH_CMD"]),
		DispatchMaxAttempts: intEnv(env, "CRQ_DISPATCH_MAX_ATTEMPTS", 3),
		DispatchConcurrency: intEnv(env, "CRQ_DISPATCH_CONCURRENCY", 0),
		WorkspaceRoot:       env["CRQ_WORKSPACE"],
		NoOpen:              env["CRQ_NO_OPEN"] != "",
		DryRun:              env["CRQ_DRY_RUN"] == "1",
		FeedbackWaitTimeout: durationEnv(env, "CRQ_FEEDBACK_WAIT_TIMEOUT", 20*time.Minute),
		SettleWindow:        durationEnv(env, "CRQ_SETTLE", 90*time.Second),

		RateLimitCoDegrade: stringEnv(env, "CRQ_RL_CO_DEGRADE", stringEnv(env, "CRQ_RL_CODEX_DEGRADE", "1")) != "0",
	}
	if len(cfg.Scope) == 0 && cfg.GateRepo != "" {
		cfg.Scope = []string{ownerOf(cfg.GateRepo)}
	}
	// Built here, after the command is resolved, because the primary's trigger is
	// part of describing it.
	cfg.Reviewers = buildReviewers(cfg.Bot, cfg.ReviewCommand, requiredBots, coBots)
	// The legacy lists are now VIEWS of cfg.Reviewers rather than parallel
	// parses, so they cannot answer differently from it. An explicit
	// CRQ_FEEDBACK_BOTS still wins: it is the one list an operator may widen
	// beyond who reviews (to surface a bot's findings without waiting for it).
	cfg.RequiredBots = cfg.reviewerLogins(func(r Reviewer) bool { return r.Required })
	if _, explicit := env["CRQ_FEEDBACK_BOTS"]; !explicit {
		// Everyone except a primary the operator deliberately left out of
		// CRQ_REQUIRED_BOTS. That omission is how you say "do not wait for
		// CodeRabbit here", and surfacing its findings anyway would put the round
		// back to work over a reviewer nobody asked for.
		cfg.FeedbackBots = cfg.reviewerLogins(func(r Reviewer) bool { return r.Required || !r.Metered() })
	}
	return cfg, nil
}

func (c Config) RequireState() error {
	if c.GateRepo == "" {
		return errors.New("CRQ_REPO is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

func (c Config) RequireDashboard() error {
	if err := c.RequireState(); err != nil {
		return err
	}
	if c.DashboardIssue <= 0 {
		return errors.New("CRQ_ISSUE is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if unquoted, err := strconv.Unquote(v); err == nil {
				v = unquoted
			} else if v[0] == '\'' && v[len(v)-1] == '\'' {
				v = v[1 : len(v)-1]
			}
		}
		out[k] = v
	}
	return out, scanner.Err()
}

func stringEnv(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok && v != "" {
		return v
	}
	return fallback
}

func stringEnvAllowEmpty(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok {
		return v
	}
	return fallback
}

func intEnv(env map[string]string, key string, fallback int) int {
	v := env[key]
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func durationEnv(env map[string]string, key string, fallback time.Duration) time.Duration {
	v := env[key]
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Second
}

func listEnv(env map[string]string, key, fallback string) []string {
	value := env[key]
	if value == "" {
		value = fallback
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// CoBotConfig is one enabled co-reviewer: "wanted" (findings surface, dynamic
// gate engages when the bot is observed active) unless Required, which gates
// convergence via RequiredBots membership. Trigger and SelfHealGrace shape
// when crq may post Command (see engine.DecideCoPost).
type CoBotConfig struct {
	Login         string
	Name          string
	Command       string
	Trigger       engine.TriggerMode
	Required      bool
	SelfHealGrace time.Duration
}

// parseCoBots resolves the enabled co-reviewers from CRQ_COBOTS (default all
// known; explicitly empty disables all) plus the per-bot CRQ_COBOT_<NAME>_*
// keys. A co-reviewer login listed in CRQ_REQUIRED_BOTS is required+enabled
// even when CRQ_COBOTS omits it. Codex keeps its historical defaults: trigger
// `always` iff required (else never), command from CRQ_CODEX_CMD as the
// legacy alias of CRQ_COBOT_CODEX_CMD. Bugbot/Macroscope default to
// `selfheal` — they auto-review pushes, so crq only nudges one that went
// silent on a head it should have covered.
func parseCoBots(env map[string]string, requiredBots []string) ([]CoBotConfig, error) {
	enabled := map[string]bool{}
	var unknown []string
	for _, item := range splitList(stringEnvAllowEmpty(env, "CRQ_COBOTS", "codex,bugbot,macroscope")) {
		co, ok := dialect.CoReviewerByName(item)
		if !ok {
			// Refuse rather than skip: silently dropping a typo disables the
			// co-reviewer the operator asked for, and the symptom (a bot that
			// never runs) looks nothing like its cause.
			unknown = append(unknown, item)
			continue
		}
		enabled[co.Name] = true
	}
	if len(unknown) > 0 {
		known := make([]string, 0, 3)
		for _, co := range dialect.KnownCoReviewers() {
			known = append(known, co.Name)
		}
		return nil, fmt.Errorf("CRQ_COBOTS: unknown co-reviewer %s (known: %s)",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	requiredSet := map[string]bool{}
	for _, bot := range requiredBots {
		requiredSet[dialect.NormalizeBotName(strings.TrimSpace(bot))] = true
	}
	var out []CoBotConfig
	for _, co := range dialect.KnownCoReviewers() {
		key := strings.ToUpper(co.Name)
		required := boolEnv(env, "CRQ_COBOT_"+key+"_REQUIRED", false) || requiredSet[dialect.NormalizeBotName(co.Login)]
		if !enabled[co.Name] && !required {
			continue
		}
		// Uniform across bots: the per-bot key wins, then the registry's legacy
		// alias if it declares one, then its default command. No bot is named
		// here — the policy travels as registry metadata.
		command := co.Command
		if v, ok := env["CRQ_COBOT_"+key+"_CMD"]; ok {
			command = v
		} else if co.LegacyCommandEnv != "" {
			command = stringEnvAllowEmpty(env, co.LegacyCommandEnv, command)
		}
		command = strings.TrimSpace(command)
		trigger := triggerMode(co.DefaultTrigger, engine.TriggerSelfHeal)
		if required && co.RequiredTrigger != "" {
			trigger = triggerMode(co.RequiredTrigger, trigger)
		}
		switch v := engine.TriggerMode(strings.ToLower(strings.TrimSpace(env["CRQ_COBOT_"+key+"_TRIGGER"]))); v {
		case engine.TriggerNever, engine.TriggerSelfHeal, engine.TriggerAlways:
			trigger = v
		}
		if command == "" {
			// No trigger command means crq can never post one, whatever the mode.
			trigger = engine.TriggerNever
		}
		out = append(out, CoBotConfig{
			Login:         co.Login,
			Name:          co.Name,
			Command:       command,
			Trigger:       trigger,
			Required:      required,
			SelfHealGrace: durationEnv(env, "CRQ_COBOT_"+key+"_GRACE", 10*time.Minute),
		})
	}
	return out, nil
}

// triggerMode converts a registry trigger string to the engine mode, falling
// back when a bot declares none.
func triggerMode(name string, fallback engine.TriggerMode) engine.TriggerMode {
	switch m := engine.TriggerMode(strings.ToLower(strings.TrimSpace(name))); m {
	case engine.TriggerNever, engine.TriggerSelfHeal, engine.TriggerAlways:
		return m
	}
	return fallback
}

// splitList splits a comma-separated list, dropping blanks (an all-blank or
// empty value yields nil — unlike listEnv it never falls back).
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func boolEnv(env map[string]string, key string, fallback bool) bool {
	v := strings.TrimSpace(env[key])
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// unionBots concatenates bot lists, dropping blanks and case-insensitively
// de-duplicating on the normalized login (so "coderabbitai" and
// "coderabbitai[bot]" collapse to one), preserving first-seen order.
func unionBots(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			key := dialect.NormalizeBotName(item)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
		}
	}
	return out
}

func repoSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = NormalizeRepo(item)
		if item != "" {
			set[item] = true
		}
	}
	return set
}

// authorSet normalizes a comma-separated login list the same way scan results
// are matched: lowercase with the "[bot]" suffix stripped, so "dependabot",
// "Dependabot" and "dependabot[bot]" all name the same author.
func authorSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = dialect.NormalizeBotName(strings.ToLower(strings.TrimSpace(item)))
		if item != "" {
			set[item] = true
		}
	}
	return set
}

func ownerOf(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

// splitArgv splits a configured command into argv on whitespace, keeping
// quoted runs together: 'claude -p "fix these findings"' is three arguments,
// not five with stray quote characters in them.
//
// Quoting is ALL it understands. It is deliberately not a shell: a dispatch
// command runs directly, so nothing here expands a variable, a glob, or a pipe
// that the operator did not write. Both quote styles behave the same way, and
// an unclosed quote simply runs to the end rather than failing a config load
// over it.
func splitArgv(value string) []string {
	var argv []string
	var arg strings.Builder
	quote := rune(0)
	quoted := false // "" is an argument, even though it contributes no characters
	for _, r := range value {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, quoted = r, true
		case quote == 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			if arg.Len() > 0 || quoted {
				argv = append(argv, arg.String())
				arg.Reset()
				quoted = false
			}
		default:
			arg.WriteRune(r)
		}
	}
	if arg.Len() > 0 || quoted {
		argv = append(argv, arg.String())
	}
	return argv
}
