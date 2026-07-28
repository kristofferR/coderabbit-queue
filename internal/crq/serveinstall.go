package crq

import (
	"context"
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

// ServeInstall is the plan for keeping a crq service running across a logout
// and a reboot. It covers both the dashboard and the review daemon: they are
// the same shape of thing — one long-running crq subcommand — and the only
// differences are the unit name and the arguments.
//
// Deliberately much thinner than the autofix install beside it. That one bakes
// the whole fleet configuration into the unit's environment because a fix
// session's behaviour has to be pinned at install time. The dashboard has no
// such need: it reads the same config file every other command reads, so the
// unit carries a path and nothing else — and editing ~/.config/crq/env then
// changes the dashboard by restarting it, not by reinstalling it.
type ServeInstall struct {
	// Service is the crq subcommand this unit runs: "serve" or "autoreview".
	Service  string `json:"service"`
	Platform string `json:"platform"`
	Unit     string `json:"unit"`
	LogDir   string `json:"log_dir"`
	Binary   string `json:"binary"`
	Addr     string `json:"addr"`
	Config   string `json:"config,omitempty"`
	// ReadOnly installs a dashboard that refuses every write, for pointing at a
	// fleet you do not administer.
	ReadOnly bool `json:"read_only,omitempty"`
	// SkipAuthCheck installs without proving the service can authenticate. Same
	// escape hatch as the autofix install: a macOS host reached over SSH cannot
	// read the GUI session's keychain, so an expired token and a perfectly good
	// one look identical from there.
	SkipAuthCheck bool     `json:"skip_auth_check,omitempty"`
	Commands      []string `json:"commands"`
	DryRun        bool     `json:"dry_run,omitempty"`
	Started       bool     `json:"started,omitempty"`
}

// InstallServe writes the service definition for `crq serve` and starts it.
func (s *Service) InstallServe(ctx context.Context, addr string, readOnly, dryRun, skipAuth bool) (ServeInstall, error) {
	return s.installUnit(ctx, "serve", addr, readOnly, dryRun, skipAuth)
}

// InstallAutoReview writes the service definition for `crq autoreview` and
// starts it — the daemon that finds pull requests needing a review and fires
// the queue.
//
// Which host runs it is a real choice: it takes the leader lease, so the fleet
// only fires while that machine is awake. A laptop that sleeps is the wrong
// host for it, and nothing about the queue says so until reviews quietly stop.
func (s *Service) InstallAutoReview(ctx context.Context, dryRun, skipAuth bool) (ServeInstall, error) {
	return s.installUnit(ctx, "autoreview", "", false, dryRun, skipAuth)
}

func (s *Service) installUnit(ctx context.Context, service, addr string, readOnly, dryRun, skipAuth bool) (ServeInstall, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ServeInstall{}, fmt.Errorf("resolving home directory: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return ServeInstall{}, fmt.Errorf("resolving the crq binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if service == "serve" && strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:7777"
	}

	// systemd refuses to start a unit whose StandardOutput path cannot be
	// opened, so the directory has to exist before the unit does.
	logDir := filepath.Join(home, ".local", "state", "crq")
	plan := ServeInstall{
		Service:       service,
		Platform:      runtime.GOOS,
		LogDir:        logDir,
		Binary:        self,
		Addr:          addr,
		Config:        ConfigPath(),
		ReadOnly:      readOnly,
		SkipAuthCheck: skipAuth,
		DryRun:        dryRun,
	}
	switch runtime.GOOS {
	case "darwin":
		plan.Unit = filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-"+service+".plist")
		plan.Commands = []string{
			"launchctl bootout gui/$(id -u)/no.kristofferr.crq-" + service,
			"launchctl bootstrap gui/$(id -u) " + plan.Unit,
		}
	default:
		plan.Unit = filepath.Join(home, ".config", "systemd", "user", "crq-"+service+".service")
		plan.Commands = []string{
			"loginctl enable-linger " + os.Getenv("USER"),
			"systemctl --user daemon-reload",
			"systemctl --user enable crq-" + service,
			// restart rather than "enable --now", which does nothing to a unit
			// that is already running: reinstalling on a different address would
			// otherwise report success and keep serving the old one.
			"systemctl --user restart crq-" + service,
		}
	}
	if dryRun {
		return plan, nil
	}

	// Both units read GitHub on every pass and neither inherits this shell's
	// variables, so the same check the autofix install makes belongs here.
	// Without it `systemctl restart` succeeds, the install prints Started, and
	// the process then fails its state reads for ever — no reviews, no
	// dashboard, and nothing that says why.
	if !skipAuth {
		if err := serviceCanAuthenticate(ctx, service); err != nil {
			return plan, err
		}
	}

	for _, dir := range []string{logDir, filepath.Dir(plan.Unit)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return plan, err
		}
	}
	if err := writeAutofixFile(plan.Unit, serveUnitBody(plan), 0o644); err != nil {
		return plan, err
	}

	var failed []string
	for i, line := range plan.Commands {
		args := serveCommandArgs(plan)[i]
		if len(args) == 0 {
			return plan, fmt.Errorf("serve start command %q has no executable", line)
		}
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil && launchdJobAbsent(line, output) {
			continue
		}
		if err != nil {
			detail := err.Error()
			if text := strings.TrimSpace(string(output)); text != "" {
				detail += ": " + text
			}
			failed = append(failed, fmt.Sprintf("%s: %s", line, detail))
		}
	}
	if len(failed) > 0 {
		// The unit file is the durable part and is already written, so say what
		// to run rather than pretending the dashboard is up.
		return plan, fmt.Errorf("%s is written, but starting it failed — run these by hand: %s",
			plan.Unit, strings.Join(failed, "; "))
	}
	plan.Started = true
	return plan, nil
}

func serveCommandArgs(plan ServeInstall) [][]string {
	if plan.Platform == "darwin" {
		domain := "gui/" + currentUID()
		return [][]string{
			{"launchctl", "bootout", domain + "/no.kristofferr.crq-" + plan.Service},
			{"launchctl", "bootstrap", domain, plan.Unit},
		}
	}
	return [][]string{
		{"loginctl", "enable-linger", os.Getenv("USER")},
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "crq-" + plan.Service},
		{"systemctl", "--user", "restart", "crq-" + plan.Service},
	}
}

// serveArgv is the command the service runs.
func serveArgv(plan ServeInstall) []string {
	if plan.Service != "serve" {
		return []string{plan.Binary, plan.Service}
	}
	argv := []string{plan.Binary, "serve", "--addr", plan.Addr}
	if plan.ReadOnly {
		argv = append(argv, "--read-only")
	}
	return argv
}

// unitDescription is what the service manager and `systemctl status` show.
func unitDescription(service string) string {
	if service == "autoreview" {
		return "crq autoreview (find pull requests needing a review and fire the queue)"
	}
	return "crq dashboard (crq serve)"
}

func serveUnitBody(plan ServeInstall) string {
	env := map[string]string{"HOME": os.Getenv("HOME")}
	if plan.Config != "" {
		env["CRQ_CONFIG"] = plan.Config
	}
	// A service manager hands a unit its own minimal PATH, and the dashboard
	// shells out to git for the state ref and to gh's credential helper.
	if path := os.Getenv("PATH"); path != "" {
		env["PATH"] = path
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if plan.Platform == "darwin" {
		var entries strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&entries, "\t\t<key>%s</key><string>%s</string>\n",
				html.EscapeString(k), html.EscapeString(env[k]))
		}
		var argv strings.Builder
		for _, a := range serveArgv(plan) {
			fmt.Fprintf(&argv, "<string>%s</string>", html.EscapeString(a))
		}
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>no.kristofferr.crq-%s</string>
	<key>ProgramArguments</key><array>%s</array>
	<key>EnvironmentVariables</key><dict>
%s	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s/%s.log</string>
	<key>StandardErrorPath</key><string>%s/%s.err</string>
</dict></plist>
`, html.EscapeString(plan.Service), argv.String(), entries.String(),
			html.EscapeString(plan.LogDir), html.EscapeString(plan.Service),
			html.EscapeString(plan.LogDir), html.EscapeString(plan.Service))
	}

	var lines strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&lines, "Environment=%s\n", strconv.Quote(strings.ReplaceAll(k+"="+env[k], "%", "%%")))
	}
	words := make([]string, 0, 4)
	for _, a := range serveArgv(plan) {
		words = append(words, systemdExecWord(a))
	}
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target

[Service]
Type=simple
%sExecStart=%s
Restart=always
RestartSec=5
StandardOutput=append:%s/%s.log
StandardError=append:%s/%s.err

[Install]
WantedBy=default.target
`, unitDescription(plan.Service), lines.String(), strings.Join(words, " "),
		plan.LogDir, plan.Service, plan.LogDir, plan.Service)
}
