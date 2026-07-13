package crq

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const Version = "2.0.0-dev"

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
	SkipMarker        string
	StateRef          string
	Bot               string
	RequiredBots      []string
	FeedbackBots      []string
	ReviewCommand     string
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
}

func LoadConfig() (Config, error) {
	env := map[string]string{}
	configPath := os.Getenv("CRQ_CONFIG")
	if configPath == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			configPath = filepath.Join(home, ".config", "crq", "env")
		}
	}
	if configPath != "" {
		values, err := readEnvFile(configPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
		for k, v := range values {
			env[k] = v
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
	cfg := Config{
		GateRepo:            env["CRQ_REPO"],
		DashboardIssue:      intEnv(env, "CRQ_ISSUE", 0),
		CalibrationPR:       intEnv(env, "CRQ_CAL_PR", 0),
		Scope:               listEnv(env, "CRQ_SCOPE", ownerOf(env["CRQ_REPO"])),
		AllowRepos:          repoSet(env["CRQ_REPOS"]),
		ExcludeRepos:        repoSet(env["CRQ_EXCLUDE"]),
		SkipAuthors:         authorSet(stringEnvAllowEmpty(env, "CRQ_AUTOREVIEW_SKIP_AUTHORS", "dependabot[bot]")),
		SkipMarker:          stringEnvAllowEmpty(env, "CRQ_AUTOREVIEW_SKIP_MARKER", "<!-- crq:skip-autoreview -->"),
		StateRef:            stringEnv(env, "CRQ_STATE_REF", "crq-state"),
		Bot:                 bot,
		RequiredBots:        requiredBots,
		FeedbackBots:        listEnv(env, "CRQ_FEEDBACK_BOTS", strings.Join(unionBots(requiredBots, extraFeedbackBots), ",")),
		ReviewCommand:       stringEnv(env, "CRQ_REVIEW_CMD", "@coderabbitai review"),
		RateLimitCommand:    stringEnv(env, "CRQ_RATELIMIT_CMD", "@coderabbitai rate limit"),
		RateLimitMarker:     stringEnv(env, "CRQ_RL_MARKER", "rate limited by coderabbit.ai"),
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
		NoOpen:              env["CRQ_NO_OPEN"] != "",
		DryRun:              env["CRQ_DRY_RUN"] == "1",
		FeedbackWaitTimeout: durationEnv(env, "CRQ_FEEDBACK_WAIT_TIMEOUT", 20*time.Minute),
	}
	if len(cfg.Scope) == 0 && cfg.GateRepo != "" {
		cfg.Scope = []string{ownerOf(cfg.GateRepo)}
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

// extraFeedbackBots are review bots whose findings crq surfaces on top of the
// required bots, without gating convergence on them. Codex is the motivating
// case: it reviews but isn't "required" (crq neither fires nor waits for it), so
// its findings would otherwise be silently dropped. This is deliberately just
// Codex — CodeRabbit (or any configured reviewer) already enters the feedback
// set via RequiredBots, so listing it here too would wrongly surface CodeRabbit
// findings even when crq is configured for a different reviewer.
var extraFeedbackBots = []string{"chatgpt-codex-connector[bot]"}

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
			key := normalizeBotName(item)
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
		item = normalizeBotName(strings.ToLower(strings.TrimSpace(item)))
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
