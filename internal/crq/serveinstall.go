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

// ServeInstall is the plan for keeping the dashboard running across a logout
// and a reboot.
//
// Deliberately much thinner than the autofix install beside it. That one bakes
// the whole fleet configuration into the unit's environment because a fix
// session's behaviour has to be pinned at install time. The dashboard has no
// such need: it reads the same config file every other command reads, so the
// unit carries a path and nothing else — and editing ~/.config/crq/env then
// changes the dashboard by restarting it, not by reinstalling it.
type ServeInstall struct {
	Platform string `json:"platform"`
	Unit     string `json:"unit"`
	LogDir   string `json:"log_dir"`
	Binary   string `json:"binary"`
	Addr     string `json:"addr"`
	Config   string `json:"config,omitempty"`
	// ReadOnly installs a dashboard that refuses every write, for pointing at a
	// fleet you do not administer.
	ReadOnly bool     `json:"read_only,omitempty"`
	Commands []string `json:"commands"`
	DryRun   bool     `json:"dry_run,omitempty"`
	Started  bool     `json:"started,omitempty"`
}

// InstallServe writes the service definition for `crq serve` and starts it.
func (s *Service) InstallServe(ctx context.Context, addr string, readOnly, dryRun bool) (ServeInstall, error) {
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
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:7777"
	}

	// systemd refuses to start a unit whose StandardOutput path cannot be
	// opened, so the directory has to exist before the unit does.
	logDir := filepath.Join(home, ".local", "state", "crq")
	plan := ServeInstall{
		Platform: runtime.GOOS,
		LogDir:   logDir,
		Binary:   self,
		Addr:     addr,
		Config:   ConfigPath(),
		ReadOnly: readOnly,
		DryRun:   dryRun,
	}
	switch runtime.GOOS {
	case "darwin":
		plan.Unit = filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-serve.plist")
		plan.Commands = []string{
			"launchctl bootout gui/$(id -u)/no.kristofferr.crq-serve",
			"launchctl bootstrap gui/$(id -u) " + plan.Unit,
		}
	default:
		plan.Unit = filepath.Join(home, ".config", "systemd", "user", "crq-serve.service")
		plan.Commands = []string{
			"loginctl enable-linger " + os.Getenv("USER"),
			"systemctl --user daemon-reload",
			"systemctl --user enable crq-serve",
			// restart rather than "enable --now", which does nothing to a unit
			// that is already running: reinstalling on a different address would
			// otherwise report success and keep serving the old one.
			"systemctl --user restart crq-serve",
		}
	}
	if dryRun {
		return plan, nil
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
			{"launchctl", "bootout", domain + "/no.kristofferr.crq-serve"},
			{"launchctl", "bootstrap", domain, plan.Unit},
		}
	}
	return [][]string{
		{"loginctl", "enable-linger", os.Getenv("USER")},
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "crq-serve"},
		{"systemctl", "--user", "restart", "crq-serve"},
	}
}

// serveArgv is the command the service runs.
func serveArgv(plan ServeInstall) []string {
	argv := []string{plan.Binary, "serve", "--addr", plan.Addr}
	if plan.ReadOnly {
		argv = append(argv, "--read-only")
	}
	return argv
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
	<key>Label</key><string>no.kristofferr.crq-serve</string>
	<key>ProgramArguments</key><array>%s</array>
	<key>EnvironmentVariables</key><dict>
%s	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s/serve.log</string>
	<key>StandardErrorPath</key><string>%s/serve.err</string>
</dict></plist>
`, argv.String(), entries.String(), html.EscapeString(plan.LogDir), html.EscapeString(plan.LogDir))
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
Description=crq dashboard (crq serve)
After=network-online.target

[Service]
Type=simple
%sExecStart=%s
Restart=always
RestartSec=5
StandardOutput=append:%s/serve.log
StandardError=append:%s/serve.err

[Install]
WantedBy=default.target
`, lines.String(), strings.Join(words, " "), plan.LogDir, plan.LogDir)
}
