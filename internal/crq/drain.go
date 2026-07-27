package crq

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// fixPrompt is what a dispatched session is told. It is embedded so there is one
// copy: the file in examples/ is the same bytes crq installs, and a rule learned
// the hard way cannot drift between them.
//
//go:embed dispatch/fix-prompt.txt
var fixPrompt string

// DrainInstall describes what an install would do, so --dry-run can print it and
// the result can be reported.
type DrainInstall struct {
	Platform string   `json:"platform"`
	Prompt   string   `json:"prompt"`
	Wrapper  string   `json:"wrapper"`
	Unit     string   `json:"unit"`
	LogDir   string   `json:"log_dir"`
	Agent    string   `json:"agent"`
	Repos    []string `json:"repos"`
	Commands []string `json:"commands"`
	DryRun   bool     `json:"dry_run,omitempty"`
	Started  bool     `json:"started,omitempty"`
}

// InstallDrain sets up the unattended review drain: the prompt, a wrapper, a
// service definition, and whatever the platform needs to keep it running across
// a logout.
//
// It exists because the alternative was a README asking somebody to copy three
// files, remember `loginctl enable-linger`, and get the environment right — and
// a setup people get wrong is a setup that silently does nothing, which is the
// failure this whole feature is about.
func (s *Service) InstallDrain(ctx context.Context, agent string, repos []string, dryRun bool) (DrainInstall, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return DrainInstall{}, err
	}
	if agent = strings.TrimSpace(agent); agent == "" {
		agent, err = exec.LookPath("claude")
		if err != nil {
			return DrainInstall{}, fmt.Errorf("no fix agent found: pass --agent <path> (tried \"claude\" on PATH)")
		}
	} else {
		// A typo here is not a smaller mistake than a missing default: the install
		// would report success and every dispatch would fail to start a session,
		// which is the silent nothing this command exists to prevent.
		resolved, lerr := exec.LookPath(agent)
		if lerr != nil {
			return DrainInstall{}, fmt.Errorf("fix agent %q cannot be run: %w", agent, lerr)
		}
		agent = resolved
	}
	if len(repos) == 0 {
		for repo := range s.cfg.AllowRepos {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		return DrainInstall{}, fmt.Errorf("no repositories to watch: pass them, or set CRQ_REPOS")
	}

	self, err := os.Executable()
	if err != nil {
		return DrainInstall{}, err
	}
	// The service writes its output here. systemd refuses to start a unit whose
	// StandardOutput path cannot be opened (209/STDOUT), so the directory has to
	// exist before the unit does — a service that will not start is exactly the
	// silent nothing this command exists to prevent.
	logDir := filepath.Join(home, ".local", "state", "crq")
	plan := DrainInstall{
		Platform: runtime.GOOS,
		LogDir:   logDir,
		Prompt:   filepath.Join(home, ".local", "share", "crq", "fix-prompt.txt"),
		Wrapper:  filepath.Join(home, ".local", "bin", "crq-drain"),
		Agent:    agent,
		Repos:    repos,
		DryRun:   dryRun,
	}
	switch runtime.GOOS {
	case "darwin":
		plan.Unit = filepath.Join(home, "Library", "LaunchAgents", "no.kristofferr.crq-drain.plist")
		plan.Commands = []string{
			"launchctl bootout gui/$(id -u)/no.kristofferr.crq-drain",
			"launchctl bootstrap gui/$(id -u) " + plan.Unit,
		}
	default:
		plan.Unit = filepath.Join(home, ".config", "systemd", "user", "crq-drain.service")
		plan.Commands = []string{
			"loginctl enable-linger " + os.Getenv("USER"),
			"systemctl --user daemon-reload",
			"systemctl --user enable --now crq-drain",
		}
	}
	if dryRun {
		return plan, nil
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return plan, err
	}

	// The agent is invoked with Claude Code's own flags, so --agent takes a
	// Claude-compatible executable — a wrapper, or a build of it — and not an
	// agent with a different CLI. Anything else needs its own wrapper script;
	// `crq watch --dispatch -- <cmd>...` runs whatever argv it is given.
	//
	// --output-format stream-json --verbose makes the session write as it works.
	// Without it the log stays empty until the session ENDS, so a session that
	// hangs or dies mid-way leaves a zero-byte file — which is the same "output
	// went nowhere" problem, just with a file to show for it.
	wrapper := fmt.Sprintf(`#!/usr/bin/env bash
# Installed by "crq drain install". Runs the review drain: crq decides, and a
# fix session is started for each PR that needs one.
set -uo pipefail
exec %q watch --dispatch -- \
  %q -p "$(cat %q)" \
  --permission-mode bypassPermissions \
  --output-format stream-json --verbose
`, self, agent, plan.Prompt)

	for _, f := range []struct {
		path string
		body string
		mode os.FileMode
	}{
		{plan.Prompt, fixPrompt, 0o644},
		{plan.Wrapper, wrapper, 0o755},
		{plan.Unit, s.drainUnit(plan), 0o644},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return plan, err
		}
		if err := os.WriteFile(f.path, []byte(f.body), f.mode); err != nil {
			return plan, err
		}
	}

	// The files are the durable part, and a machine without systemd/launchd still
	// gets something runnable by hand — but a failure here means the drain is not
	// running, and reporting `started` for that is the same silent nothing this
	// command exists to prevent. Say which command failed, and let the caller see
	// the paths that were written.
	var failed []string
	for _, line := range plan.Commands {
		parts := strings.Fields(strings.ReplaceAll(line, "$(id -u)", currentUID()))
		if len(parts) == 0 {
			continue
		}
		if strings.Contains(line, "$USER") {
			parts[len(parts)-1] = os.Getenv("USER")
		}
		cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
		if err := cmd.Run(); err != nil {
			if s.log != nil {
				s.log.Printf("drain install: %s: %v", line, err)
			}
			failed = append(failed, fmt.Sprintf("%s: %v", line, err))
		}
	}
	if len(failed) > 0 {
		return plan, fmt.Errorf("the drain files are installed, but starting it failed — run these by hand: %s",
			strings.Join(failed, "; "))
	}
	plan.Started = true
	return plan, nil
}

func currentUID() string { return fmt.Sprint(os.Getuid()) }

// drainPath is the PATH the service runs with: this shell's, plus wherever crq
// and the agent were found.
//
// A service manager hands a unit its own minimal PATH, which on launchd is four
// system directories. The wrapper and the agent are absolute, but the session
// they start shells out to git, gh, go and crq — and a Homebrew or ~/.local/bin
// install is invisible from there, so every fix would fail at its first command.
// The installing shell's PATH is the one where those tools were just resolved.
func drainPath(plan DrainInstall) string {
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

// drainUnit renders the platform's service definition.
func (s *Service) drainUnit(plan DrainInstall) string {
	env := map[string]string{
		"CRQ_REPOS":                 strings.Join(plan.Repos, ","),
		"CRQ_WATCH_INTERVAL":        s.cfg.WatchInterval.String(),
		"CRQ_DISPATCH_MAX_ATTEMPTS": fmt.Sprint(s.cfg.DispatchMaxAttempts),
		"CRQ_DISPATCH_CONCURRENCY":  fmt.Sprint(s.cfg.DispatchConcurrency),
		"PATH":                      drainPath(plan),
	}
	logDir := plan.LogDir
	if plan.Platform == "darwin" {
		var entries strings.Builder
		for k, v := range env {
			fmt.Fprintf(&entries, "\t\t<key>%s</key><string>%s</string>\n", k, v)
		}
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>no.kristofferr.crq-drain</string>
	<key>ProgramArguments</key><array><string>%s</string></array>
	<key>EnvironmentVariables</key><dict>
%s	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s/drain.log</string>
	<key>StandardErrorPath</key><string>%s/drain.err</string>
</dict></plist>
`, plan.Wrapper, entries.String(), logDir, logDir)
	}
	var lines strings.Builder
	for k, v := range env {
		fmt.Fprintf(&lines, "Environment=%s=%s\n", k, v)
	}
	return fmt.Sprintf(`[Unit]
Description=crq review drain (watch + dispatch fix sessions)
After=network-online.target

[Service]
Type=simple
%sExecStart=%s
Restart=always
RestartSec=20
StandardOutput=append:%s/drain.log
StandardError=append:%s/drain.err

[Install]
WantedBy=default.target
`, lines.String(), plan.Wrapper, logDir, logDir)
}
