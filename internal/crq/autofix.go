package crq

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// fixPrompt is what a dispatched session is told. It is embedded so there is one
// copy: the file in examples/ is the same bytes crq installs, and a rule learned
// the hard way cannot drift between them.
//
//go:embed dispatch/fix-prompt.txt
var fixPrompt string

// HostOnlyAutofixWarning labels a preview built from this host's own
// configuration because fleet state could not be read. Shared so every path
// that can only offer that answer says the same thing about it.
const HostOnlyAutofixWarning = "GitHub fleet state is unavailable; this host-only preview may differ from an authenticated install"

// AutofixInstall describes what an install would do, so --dry-run can print it and
// the result can be reported.
type AutofixInstall struct {
	Platform     string `json:"platform"`
	Prompt       string `json:"prompt"`
	Wrapper      string `json:"wrapper"`
	Unit         string `json:"unit"`
	LogDir       string `json:"log_dir"`
	Workspace    string `json:"workspace,omitempty"`
	Agent        string `json:"agent"`
	PolicySource string `json:"policy_source"`
	Warning      string `json:"warning,omitempty"`
	// Invocation is the exact command the wrapper runs, so --dry-run shows what
	// the fix session will actually be — including the model, which is the
	// agent's business and not crq's to hardcode.
	Invocation string   `json:"invocation,omitempty"`
	Repos      []string `json:"repos"`
	Commands   []string `json:"commands"`
	// Retire is the pre-rename watcher this install shuts down, when one is
	// still on disk. Empty when there is nothing to retire.
	Retire  string `json:"retire,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Started bool   `json:"started,omitempty"`
}

// InstallAutofix sets up unattended autofix: the prompt, a wrapper, a
// service definition, and whatever the platform needs to keep it running across
// a logout.
//
// It exists because the alternative was a README asking somebody to copy three
// files, remember `loginctl enable-linger`, and get the environment right — and
// a setup people get wrong is a setup that silently does nothing, which is the
// failure this whole feature is about.
func (s *Service) InstallAutofix(ctx context.Context, agent string, agentArgs []string, repos []string, dryRun bool) (AutofixInstall, error) {
	effectiveDryRun := dryRun || s.cfg.DryRun
	st, _, err := s.store.Load(ctx)
	if err != nil {
		// A dry run is documented as a preview, so a state ref this host cannot
		// read downgrades it to the host's own plan rather than failing — the
		// warning is what keeps it from being mistaken for the fleet's. A real
		// install has no such fallback: writing a unit from policy the fleet may
		// disagree with is the divergence this whole mechanism exists to end.
		if !effectiveDryRun {
			return AutofixInstall{}, err
		}
		plan, perr := AutofixPlan(s.cfg, agent, agentArgs, repos, true)
		if perr != nil {
			return AutofixInstall{}, perr
		}
		plan.PolicySource = "host"
		plan.Warning = HostOnlyAutofixWarning
		return plan, nil
	}
	effective := s.fleetCfg(st)
	plan, err := AutofixPlan(effective, agent, agentArgs, repos, effectiveDryRun)
	plan.PolicySource = "fleet"
	if err != nil || effectiveDryRun {
		return plan, err
	}
	return s.applyAutofix(ctx, plan, s.autofixFallbackConfig(repos), repos)
}

// autofixFallbackConfig is what the unit should carry for settings the fleet may
// later unset. Fleet policy is overlaid from state at runtime; baking today's
// effective values into the service would make them survive after that policy
// was removed. Explicit install repositories remain this host's chosen fallback.
func (s *Service) autofixFallbackConfig(repos []string) Config {
	cfg := s.cfg
	if len(repos) == 0 {
		return cfg
	}
	cfg.AllowRepos = make(map[string]bool, len(repos))
	for _, repo := range repos {
		cfg.AllowRepos[repo] = true
	}
	return cfg
}

// AutofixPlan computes what an install WOULD write and run, from configuration
// alone.
//
// Separate from applying it because `crq autofix install --dry-run` is documented
// as a preview and must work for somebody who has not authenticated yet: the
// plan reads no GitHub state, so requiring a token to see it turned the one
// command for inspecting the setup into another thing to set up first.
func AutofixPlan(cfg Config, agent string, agentArgs []string, repos []string, dryRun bool) (AutofixInstall, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return AutofixInstall{}, err
	}
	if agent = strings.TrimSpace(agent); agent == "" {
		agent, err = exec.LookPath("claude")
		if err != nil {
			return AutofixInstall{}, fmt.Errorf("no fix agent found: pass --agent <path> (tried \"claude\" on PATH)")
		}
	} else {
		// A typo here is not a smaller mistake than a missing default: the install
		// would report success and every dispatch would fail to start a session,
		// which is the silent nothing this command exists to prevent.
		resolved, lerr := exec.LookPath(agent)
		if lerr != nil {
			return AutofixInstall{}, fmt.Errorf("fix agent %q cannot be run: %w", agent, lerr)
		}
		agent = resolved
	}
	agent, err = filepath.Abs(agent)
	if err != nil {
		return AutofixInstall{}, fmt.Errorf("resolving fix agent %q: %w", agent, err)
	}
	if len(repos) == 0 {
		// Sorted, because the plan is what --dry-run shows and what the unit
		// records: map order would make two identical installs disagree.
		repos = sortedRepoList(cfg.AllowRepos)
	}
	if len(repos) == 0 {
		return AutofixInstall{}, fmt.Errorf("no repositories to watch: pass them, or set CRQ_REPOS")
	}

	// The service writes its output here. systemd refuses to start a unit whose
	// StandardOutput path cannot be opened (209/STDOUT), so the directory has to
	// exist before the unit does — a service that will not start is exactly the
	// silent nothing this command exists to prevent.
	logDir := filepath.Join(home, ".local", "state", "crq")
	plan := AutofixInstall{
		Platform: runtime.GOOS,
		LogDir:   logDir,
		Prompt:   filepath.Join(home, ".local", "share", "crq", "fix-prompt.txt"),
		Wrapper:  filepath.Join(home, ".local", "bin", "crq-autofix"),
		Agent:    agent,
		Repos:    repos,
		DryRun:   dryRun,
	}
	if root := strings.TrimSpace(cfg.WorkspaceRoot); root != "" {
		plan.Workspace, err = filepath.Abs(root)
		if err != nil {
			return plan, fmt.Errorf("resolving autofix workspace %q: %w", root, err)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		plan.Unit = filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-autofix.plist")
		plan.Retire = legacyWatcherUnit(filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-drain.plist"))
		plan.Commands = []string{
			"launchctl bootout gui/$(id -u)/no.kristofferr.crq-autofix",
			"launchctl bootstrap gui/$(id -u) " + plan.Unit,
		}
		if plan.Retire != "" {
			// Both halves: bootout stops it now, disable keeps launchd from
			// bootstrapping the plist again at the next login.
			plan.Commands = append([]string{
				"launchctl disable gui/$(id -u)/no.kristofferr.crq-drain",
				"launchctl bootout gui/$(id -u)/no.kristofferr.crq-drain",
			}, plan.Commands...)
		}
	default:
		plan.Unit = filepath.Join(home, ".config", "systemd", "user", "crq-autofix.service")
		plan.Retire = legacyWatcherUnit(filepath.Join(home, ".config", "systemd", "user", "crq-drain.service"))
		plan.Commands = []string{
			"loginctl enable-linger " + os.Getenv("USER"),
			"systemctl --user daemon-reload",
			"systemctl --user enable crq-autofix",
			// restart, not "enable --now": --now does nothing to a unit that is
			// already running, so reinstalling with a different agent or prompt
			// silently kept the old one going. An install that appears to succeed
			// and changes nothing is the worst of both.
			"systemctl --user restart crq-autofix",
		}
		if plan.Retire != "" {
			plan.Commands = append([]string{"systemctl --user disable --now crq-drain"}, plan.Commands...)
		}
	}
	invocation, err := agentInvocation(agent, plan.Prompt, agentArgs)
	if err != nil {
		return plan, err
	}
	plan.Invocation = invocation

	return plan, nil
}

// applyAutofix writes the plan to disk and starts the service.
func (s *Service) applyAutofix(ctx context.Context, plan AutofixInstall, cfg Config, repos []string) (AutofixInstall, error) {
	invocation, logDir := plan.Invocation, plan.LogDir
	self, err := os.Executable()
	if err != nil {
		return plan, err
	}
	if err := autofixCanAuthenticate(ctx); err != nil {
		return plan, err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return plan, err
	}

	// Known agents receive their non-interactive flags. Any other executable is
	// treated as a self-contained prompt-taking wrapper.
	wrapper := autofixWrapper(self, invocation, repos)

	for _, f := range []struct {
		path string
		body string
		mode os.FileMode
	}{
		{plan.Prompt, fixPrompt, 0o644},
		{plan.Wrapper, wrapper, 0o755},
		{plan.Unit, autofixUnitFor(cfg, plan), 0o644},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return plan, err
		}
		if err := writeAutofixFile(f.path, f.body, f.mode); err != nil {
			return plan, err
		}
	}

	// The files are the durable part, and a machine without systemd/launchd still
	// gets something runnable by hand — but a failure here means the autofix service is not
	// running, and reporting `started` for that is the same silent nothing this
	// command exists to prevent. Say which command failed, and let the caller see
	// the paths that were written.
	var failed []string
	commandArgs := autofixCommandArgs(plan)
	if len(plan.Commands) != len(commandArgs) {
		return plan, errors.New("autofix start commands are incomplete")
	}
	for i, line := range plan.Commands {
		args := commandArgs[i]
		if len(args) == 0 {
			return plan, fmt.Errorf("autofix start command %q has no executable", line)
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		output, err := cmd.CombinedOutput()
		if err != nil && launchdJobAbsent(line, output) {
			continue
		}
		if err != nil {
			detail := err.Error()
			if text := strings.TrimSpace(string(output)); text != "" {
				detail += ": " + text
			}
			if s.log != nil {
				s.log.Printf("autofix install: %s: %s", line, detail)
			}
			failed = append(failed, fmt.Sprintf("%s: %s", line, detail))
		}
	}
	if len(failed) > 0 {
		return plan, fmt.Errorf("the autofix files are installed, but starting it failed — run these by hand: %s",
			strings.Join(failed, "; "))
	}
	plan.Started = true
	return plan, nil
}

func autofixWrapper(self, invocation string, repos []string) string {
	watchArgs := make([]string, 0, len(repos))
	for _, repo := range repos {
		watchArgs = append(watchArgs, shellQuote(repo))
	}
	watch := strings.Join(watchArgs, " ")
	if watch != "" {
		watch += " "
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
# Installed by "crq autofix install". Runs autofix: crq decides, and a
# fix session is started for each PR that needs one.
set -uo pipefail
exec %s watch %s-- %s
`, shellQuote(self), watch, invocation)
}

func writeAutofixFile(path, body string, mode os.FileMode) error {
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func autofixCommandArgs(plan AutofixInstall) [][]string {
	if plan.Platform == "darwin" {
		domain := "gui/" + currentUID()
		args := [][]string{
			{"launchctl", "bootout", domain + "/no.kristofferr.crq-autofix"},
			{"launchctl", "bootstrap", domain, plan.Unit},
		}
		if plan.Retire != "" {
			args = append([][]string{
				{"launchctl", "disable", domain + "/no.kristofferr.crq-drain"},
				{"launchctl", "bootout", domain + "/no.kristofferr.crq-drain"},
			}, args...)
		}
		return args
	}
	args := [][]string{
		{"loginctl", "enable-linger", os.Getenv("USER")},
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "crq-autofix"},
		{"systemctl", "--user", "restart", "crq-autofix"},
	}
	if plan.Retire != "" {
		args = append([][]string{{"systemctl", "--user", "disable", "--now", "crq-drain"}}, args...)
	}
	return args
}

// legacyWatcherUnit reports the pre-rename watcher's unit, if it is still there.
//
// The rename is a hard break in the state and the CLI, but a break in naming
// stops nothing that is already running: a host that ran `crq drain install`
// keeps an enabled crq-drain unit, and installing autofix beside it leaves two
// watchers scanning the same fleet and racing each other's dispatch claims.
// Detected from disk rather than attempted blindly, so a first install neither
// runs a pointless command nor has to read failure text to know it was benign.
func legacyWatcherUnit(path string) string {
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func currentUID() string { return fmt.Sprint(os.Getuid()) }

// launchctl bootout is the replacement half of a launchd reinstall. The job
// does not exist on the first install, which is already the desired state; only
// that explicit response is benign. Bootstrap failures and every other bootout
// failure still fail the install.
func launchdJobAbsent(command string, output []byte) bool {
	if !strings.HasPrefix(strings.TrimSpace(command), "launchctl bootout ") {
		return false
	}
	text := strings.ToLower(string(output))
	return strings.Contains(text, "no such process") || strings.Contains(text, "could not find service")
}

// autofixCanAuthenticate reports whether the SERVICE will find a GitHub
// credential — which is not the same question as whether this shell has one.
//
// crq resolves a token from GITHUB_TOKEN/GH_TOKEN or `gh auth token`, and the
// unit inherits none of this shell's variables. A token exported in a profile
// therefore authenticates the install and nothing afterwards: every pass fails
// to read a pull request while the install reports Started, which is the silent
// nothing this command exists to prevent. Writing the token into the unit is not
// the answer — that file is readable by every local user — so the credential has
// to be one the service can resolve itself.
func autofixCanAuthenticate(ctx context.Context) error {
	if path := ConfigPath(); path != "" {
		values, err := readEnvFile(path)
		if err == nil {
			for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
				if strings.TrimSpace(values[key]) != "" {
					return nil
				}
			}
		}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	// Cleared, because gh reads them too: this shell's token would answer for the
	// service and hide the very gap being checked.
	cmd.Env = append(os.Environ(), "GITHUB_TOKEN=", "GH_TOKEN=")
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return nil
	}
	return fmt.Errorf("the autofix service would have no GitHub credential: a service does not inherit this shell's GITHUB_TOKEN/GH_TOKEN. Run 'gh auth login', or put the token in %s, then install again", ConfigPath())
}

// autofixPath is the PATH the service runs with: this shell's, plus wherever crq
// and the agent were found.
//
// A service manager hands a unit its own minimal PATH, which on launchd is four
// system directories. The wrapper and the agent are absolute, but the session
// they start shells out to git, gh, go and crq — and a Homebrew or ~/.local/bin
// install is invisible from there, so every fix would fail at its first command.
// The installing shell's PATH is the one where those tools were just resolved.
func autofixPath(plan AutofixInstall) string {
	seen := map[string]bool{}
	var dirs []string
	add := func(dir string) {
		if dir == "" || dir == "." || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	paths := []string{plan.Wrapper, plan.Agent}
	if self, err := os.Executable(); err == nil {
		paths = append(paths, self) // the session runs `crq resolve` itself
	}
	for _, path := range paths {
		add(filepath.Dir(path))
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		add(dir)
	}
	if len(dirs) == 0 {
		return "/usr/local/bin:/usr/bin:/bin"
	}
	return strings.Join(dirs, string(filepath.ListSeparator))
}

// autofixEnv is the environment the service runs with.
//
// The service manager hands a unit its own environment, not this shell's, and
// then crq reads the configuration file itself. So the unit has to carry two
// things: the path of the file the install actually read, and the effective
// value of every setting autofix cannot work without — those may have come
// from this shell alone, and there they would be lost. An install that gets this
// wrong still reports Started, and the watcher then loads a different queue, or
// none.
func (s *Service) autofixEnv(plan AutofixInstall) map[string]string {
	return autofixEnvFor(s.cfg, plan)
}

func autofixEnvFor(cfg Config, plan AutofixInstall) map[string]string {
	// Only an operator's OWN list travels. When it was derived from who reviews,
	// writing the currently derived logins out makes the service read them back
	// as an explicit choice, frozen: a later fleet `cobots` change that enables
	// an optional reviewer would then never reach the surfaced set, and a round
	// could converge without ever showing that bot's findings. Empty is the
	// environment's "unset" — the same reasoning, and the same encoding, as the
	// implicit co-reviewer trigger below.
	feedbackBots := ""
	if cfg.FeedbackBotsExplicit {
		feedbackBots = strings.Join(cfg.FeedbackBots, ",")
	}
	env := map[string]string{
		"CRQ_REPOS": strings.Join(sortedRepoList(cfg.AllowRepos), ","),
		// The denylist travels with the allowlist. Carrying one and not the other
		// installs a service that watches a repository the operator excluded.
		"CRQ_EXCLUDE":                strings.Join(sortedRepoList(cfg.ExcludeRepos), ","),
		"CRQ_SCOPE":                  strings.Join(cfg.Scope, ","),
		"CRQ_WATCH_INTERVAL":         cfg.WatchInterval.String(),
		"CRQ_DISPATCH_MAX_ATTEMPTS":  fmt.Sprint(cfg.DispatchMaxAttempts),
		"CRQ_DISPATCH_CONCURRENCY":   fmt.Sprint(cfg.DispatchConcurrency),
		"CRQ_DISPATCH_FORKS":         strconv.FormatBool(cfg.DispatchForks),
		"CRQ_AUTOREVIEW_SKIP_MARKER": cfg.SkipMarker,
		"CRQ_BOT":                    cfg.Bot,
		"CRQ_REQUIRED_BOTS":          strings.Join(cfg.RequiredBots, ","),
		"CRQ_FEEDBACK_BOTS":          feedbackBots,
		"CRQ_REVIEW_CMD":             cfg.ReviewCommand,
		"CRQ_RATELIMIT_CMD":          cfg.RateLimitCommand,
		"CRQ_RL_MARKER":              cfg.RateLimitMarker,
		"CRQ_CAL_REPLY_MARKER":       cfg.CalibrationMarker,
		"CRQ_REVIEW_DONE_MARKER":     cfg.ReviewDoneMarker,
		"CRQ_COMPLETION_MARKER":      cfg.CompletionMarker,
		"CRQ_MIN_INTERVAL":           cfg.MinInterval.String(),
		"CRQ_INFLIGHT_TIMEOUT":       cfg.InflightTimeout.String(),
		"CRQ_POLL":                   cfg.PollInterval.String(),
		"CRQ_FEEDBACK_WAIT_TIMEOUT":  cfg.FeedbackWaitTimeout.String(),
		"CRQ_SETTLE":                 cfg.SettleWindow.String(),
		// The quota timings belong here for the same reason as every other
		// setting: the service does not inherit the shell that installed it. A
		// deliberately longer fallback set only in that shell was folded into
		// this config, written into no unit, and then silently replaced by the
		// default — so the watcher retried a review command earlier than
		// configured for every account block it could not parse a window from.
		"CRQ_CALIBRATE_TTL": cfg.CalibrationTTL.String(),
		"CRQ_RL_FALLBACK":   cfg.RateLimitFallback.String(),
		"PATH":              autofixPath(plan),
	}
	if cfg.RateLimitCoDegrade {
		env["CRQ_RL_CO_DEGRADE"] = "1"
	} else {
		env["CRQ_RL_CO_DEGRADE"] = "0"
	}
	coNames := make([]string, 0, len(cfg.CoBots))
	for _, co := range cfg.CoBots {
		coNames = append(coNames, co.Name)
	}
	written := map[string]bool{}
	writeCoBot := func(co CoBotConfig) {
		key := strings.ToUpper(co.Name)
		if written[key] {
			return
		}
		written[key] = true
		prefix := "CRQ_COBOT_" + key
		env[prefix+"_CMD"] = co.Command
		// The explicitness bit has to survive the install, not just the mode. An
		// implicit trigger is the registry default for how the bot is REQUIRED,
		// recomputed whenever that changes; writing it out as a value makes the
		// service read it back as an operator's explicit choice, and a later
		// fleet `required-bots` change can then no longer promote the bot to its
		// required trigger — leaving a required reviewer that is never commanded
		// and a round that waits for it forever. Empty is the environment's
		// "unset", and it is written rather than omitted so an inherited variable
		// cannot supply an explicitness the installing host did not have.
		trigger := ""
		if co.TriggerExplicit {
			trigger = string(co.Trigger)
		}
		env[prefix+"_TRIGGER"] = trigger
		env[prefix+"_REQUIRED"] = strconv.FormatBool(co.Required)
		env[prefix+"_GRACE"] = co.SelfHealGrace.String()
	}
	// The enabled entries first: they carry the requiredness this host resolved,
	// and with it the trigger mode that requiredness implies.
	for _, co := range cfg.CoBots {
		writeCoBot(co)
	}
	// Then the registry-wide fallbacks for the bots this host has switched off.
	// A command, trigger or grace set for one of those is still the value this
	// host would drive it with the moment something enables it — a per-repo
	// reviewer override, or a fleet `cobots` change that names the bot without
	// naming its per-bot keys. Carrying only the enabled set left the service
	// running that bot on registry defaults while the installing shell's preview
	// showed the host's own.
	for _, co := range cfg.KnownCoBots {
		writeCoBot(co)
	}
	// Explicitly carry an empty set: omitting this key would re-enable every
	// default co-reviewer when the service starts.
	env["CRQ_COBOTS"] = strings.Join(coNames, ",")
	// A workspace supplied by the installing shell is already folded into cfg,
	// but the service does not inherit that shell. Preserve the effective value
	// or dispatch silently falls back to another filesystem.
	workspace := plan.Workspace
	if workspace == "" {
		workspace = cfg.WorkspaceRoot
	}
	if workspace != "" {
		env["CRQ_WORKSPACE"] = workspace
	}
	if path := ConfigPath(); path != "" {
		env["CRQ_CONFIG"] = path
	}
	// The queue's identity: the repository holding the state ref, the ref itself,
	// the dashboard, and the PR the account quota is probed on. Without the first
	// two, the watcher cannot even load state — or loads a queue nobody else is
	// using, which looks exactly like an idle fleet.
	if cfg.GateRepo != "" {
		env["CRQ_REPO"] = cfg.GateRepo
	}
	if cfg.StateRef != "" {
		env["CRQ_STATE_REF"] = cfg.StateRef
	}
	if cfg.DashboardIssue > 0 {
		env["CRQ_ISSUE"] = fmt.Sprint(cfg.DashboardIssue)
	}
	if cfg.CalibrationPR > 0 {
		env["CRQ_CAL_PR"] = fmt.Sprint(cfg.CalibrationPR)
	}
	return env
}

// autofixUnit renders the platform's service definition.
func (s *Service) autofixUnit(plan AutofixInstall) string {
	return autofixUnitFor(s.cfg, plan)
}

func autofixUnitFor(cfg Config, plan AutofixInstall) string {
	env := autofixEnvFor(cfg, plan)
	// Sorted: the unit is a file on disk that a re-install rewrites, and map order
	// would make every rewrite a different file for the same configuration.
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	logDir := plan.LogDir
	if plan.Platform == "darwin" {
		var entries strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&entries, "\t\t<key>%s</key><string>%s</string>\n",
				html.EscapeString(k), html.EscapeString(env[k]))
		}
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>no.kristofferr.crq-autofix</string>
	<key>ProgramArguments</key><array><string>%s</string></array>
	<key>EnvironmentVariables</key><dict>
%s	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s/autofix.log</string>
	<key>StandardErrorPath</key><string>%s/autofix.err</string>
</dict></plist>
`, html.EscapeString(plan.Wrapper), entries.String(),
			html.EscapeString(logDir), html.EscapeString(logDir))
	}
	var lines strings.Builder
	for _, k := range keys {
		// Quote the whole assignment. systemd otherwise tokenizes whitespace in
		// CRQ_CONFIG, PATH, commands, and markers into separate assignments.
		//
		// Double '%' first: systemd expands specifiers in Environment= values, so
		// a '%' in a path or a review command would be eaten or fail to load the
		// unit. Unlike ExecStart, '$' is NOT expanded here, so it stays literal.
		fmt.Fprintf(&lines, "Environment=%s\n", strconv.Quote(strings.ReplaceAll(k+"="+env[k], "%", "%%")))
	}
	return fmt.Sprintf(`[Unit]
Description=crq autofix (watch + dispatch fix sessions)
After=network-online.target

[Service]
Type=simple
%sExecStart=%s
Restart=always
RestartSec=20
StandardOutput=append:%s/autofix.log
StandardError=append:%s/autofix.err

[Install]
WantedBy=default.target
`, lines.String(), systemdExecWord(plan.Wrapper), logDir, logDir)
}

// systemdExecWord makes one literal word for an ExecStart command line.
// Doubling '$' and '%' suppresses systemd's environment and specifier
// expansion; strconv.Quote handles whitespace, quotes and control characters.
func systemdExecWord(word string) string {
	word = strings.NewReplacer("$", "$$", "%", "%%").Replace(word)
	return strconv.Quote(word)
}

// agentInvocation renders the shell words that run one fix session.
//
// crq knows how to CALL the agents it ships support for, and nothing about which
// model they should use — that belongs in the agent's own configuration or in
// --agent-args, not baked into a queue. Both known agents are given the two
// things a session needs: the prompt, and permission to act without a human
// there to approve each step.
func agentInvocation(agent, promptPath string, extra []string) (string, error) {
	quoted := make([]string, 0, len(extra))
	for _, a := range extra {
		quoted = append(quoted, shellQuote(a))
	}
	args := strings.Join(quoted, " ")
	switch filepath.Base(agent) {
	case "claude":
		// stream-json so the session log fills as it works rather than only at
		// the end, where a hung session would leave an empty file.
		return fmt.Sprintf(`%s -p "$(cat %s)" --permission-mode bypassPermissions --output-format stream-json --verbose %s`,
			shellQuote(agent), shellQuote(promptPath), args), nil
	case "codex":
		// exec is codex's non-interactive form; the prompt is its final
		// positional argument. --skip-git-repo-check because the session runs in
		// a detached worktree crq created.
		return fmt.Sprintf(`%s exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox %s "$(cat %s)"`,
			shellQuote(agent), args, shellQuote(promptPath)), nil
	default:
		return fmt.Sprintf(`%s %s "$(cat %s)"`, shellQuote(agent), args, shellQuote(promptPath)), nil
	}
}

// shellQuote makes one literal POSIX shell word. The generated wrapper is Bash,
// so single quotes prevent parameter, command and backtick expansion; an
// embedded single quote is represented by ending and reopening the quoted word.
func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'"'"'`) + "'"
}

// sortedRepoList renders a repo set for a unit file, in a stable order: the
// unit is a file a re-install rewrites, and map order would make every rewrite
// a different file for the same configuration.
func sortedRepoList(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for repo := range set {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}
