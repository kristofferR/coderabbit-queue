package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/crq"
)

type stderrLogger struct{}

func (stderrLogger) Printf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "crq: "+format+"\n", args...)
}

func main() {
	code := run(context.Background(), os.Args[1:])
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage()
		return 0
	}
	if args[0] != "help" && len(args) > 1 && isHelpArg(args[1]) {
		commandHelp(args[0])
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) > 1 {
			commandHelp(args[1])
			return 0
		}
		usage()
		return 0
	case "version", "-v", "--version":
		fmt.Printf("crq %s\n", crq.Version)
		return 0
	case "doctor":
		report := doctor(ctx)
		printJSON(report)
		if report.Ready {
			return 0
		}
		return 1
	case "preflight":
		return preflight(ctx, args[1:])
	}

	cfg, err := crq.LoadConfig()
	if err != nil {
		fatal(err)
		return 1
	}
	gh, err := crq.NewGitHub(ctx)
	if err != nil {
		fatal(err)
		return 1
	}
	gh.SetLogger(stderrLogger{})
	store := crq.NewGitStateStore(cfg, gh)
	service := crq.NewService(cfg, gh, store, stderrLogger{})

	switch args[0] {
	case "init":
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		result, err := crq.Init(ctx, cfg, gh, store)
		if err != nil {
			fatal(err)
			return 1
		}
		fmt.Printf("# Add these to %s (or your shell profile):\n", configPath())
		fmt.Printf("export CRQ_REPO=%q\n", result.GateRepo)
		fmt.Printf("export CRQ_ISSUE=%q\n", strconv.Itoa(result.DashboardIssue))
		if result.CalibrationPR > 0 {
			fmt.Printf("export CRQ_CAL_PR=%q\n", strconv.Itoa(result.CalibrationPR))
		}
		fmt.Printf("export CRQ_SCOPE=%q\n", strings.Join(cfg.Scope, ","))
		fmt.Printf("export CRQ_STATE_REF=%q\n", result.StateRef)
		return 0
	case "status":
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		_, dashboard, err := service.Status(ctx)
		if err != nil {
			fatal(err)
			return 1
		}
		fmt.Print(dashboard)
		return 0
	case "feedback":
		repo, pr, ok := repoPR(args[1:])
		if !ok {
			fatal(errors.New("usage: crq feedback <repo> <pr>"))
			return 1
		}
		report, err := service.Feedback(ctx, repo, pr)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(report)
		return 0
	case "loop":
		repo, pr, ok := repoPR(args[1:])
		if !ok {
			fatal(errors.New("usage: crq loop <repo> <pr>"))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		report, code, err := service.Loop(ctx, repo, pr)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(report)
		return code
	case "resolve":
		threads, ok := parseResolveArgs(args[1:])
		if !ok {
			fatal(errors.New("usage: crq resolve <repo> <pr> --thread <id> [--thread <id>...]"))
			return 1
		}
		if len(threads) == 0 {
			fatal(errors.New("usage: crq resolve <repo> <pr> --thread <id> [--thread <id>...]"))
			return 1
		}
		result, err := service.ResolveThreads(ctx, threads)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(result)
		return 0
	case "decline":
		threads, reason, resolve, ok := parseDeclineArgs(args[1:])
		if !ok || len(threads) == 0 || strings.TrimSpace(reason) == "" {
			fatal(errors.New(`usage: crq decline <repo> <pr> --thread <id> [--thread <id>...] --reason "<why>" [--resolve]`))
			return 1
		}
		result, err := service.DeclineThreads(ctx, threads, reason, resolve)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(result)
		return 0
	case "autoreview", "auto":
		fs := flag.NewFlagSet("autoreview", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		once := fs.Bool("once", false, "run one pass")
		noIncremental := fs.Bool("no-incremental", false, "review each PR once only")
		if err := fs.Parse(args[1:]); err != nil {
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		if err := service.AutoReview(ctx, crq.AutoOptions{Once: *once, Incremental: !*noIncremental}); err != nil {
			fatal(err)
			return 1
		}
		return 0
	case "cancel":
		repo, pr, ok := repoPR(args[1:])
		if !ok {
			fatal(errors.New("usage: crq cancel <repo> <pr>"))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		if err := service.Cancel(ctx, repo, pr); err != nil {
			fatal(err)
			return 1
		}
		printJSON(map[string]any{"status": "cancelled", "repo": crq.NormalizeRepo(repo), "pr": pr})
		return 0
	case "debug":
		return debug(ctx, service, store, cfg, args[1:])
	default:
		fatal(fmt.Errorf("unknown command: %s (try 'crq help')", args[0]))
		return 1
	}
}

func debug(ctx context.Context, service *crq.Service, store crq.StateStore, cfg crq.Config, args []string) int {
	if len(args) == 0 {
		fatal(errors.New("usage: crq debug <enqueue|pump|refresh|state>"))
		return 1
	}
	if err := cfg.RequireState(); err != nil {
		fatal(err)
		return 1
	}
	switch args[0] {
	case "enqueue":
		repo, pr, ok := repoPR(args[1:])
		if !ok {
			fatal(errors.New("usage: crq debug enqueue <repo> <pr>"))
			return 1
		}
		result, err := service.Enqueue(ctx, repo, pr)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(result)
		return 0
	case "pump":
		result, err := service.Pump(ctx)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(result)
		return 0
	case "refresh":
		state, err := service.RefreshQuota(ctx)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(state.Blocked)
		return 0
	case "state":
		state, _, err := store.Load(ctx)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(state)
		return 0
	default:
		fatal(fmt.Errorf("unknown debug command: %s", args[0]))
		return 1
	}
}

func usage() {
	fmt.Print(`crq - CodeRabbit review queue for humans and automation

QUEUE WORKFLOWS
  crq loop <repo> <pr>             queue one PR review round, then emit JSON feedback
  crq autoreview                   keep open PRs reviewed through the same queue
  crq status                       show the queue, in-flight review, and quota state

ONE PR ROUND
  1. Run: crq loop <repo> <pr> > crq-feedback.json
  2. If exit 10, read .findings[], fix only valid findings, validate, commit, push.
  3. Resolve only addressed threads: crq resolve <repo> <pr> --thread <thread_id>
  4. Repeat crq loop until exit 0. Never post @coderabbitai review directly.

USAGE
  crq init                         initialize state in CRQ_REPO
  crq loop <repo> <pr>             coordinated trigger -> wait -> JSON feedback/convergence
  crq feedback <repo> <pr>         emit normalized actionable review findings as JSON
  crq resolve <repo> <pr> --thread <id> [...]
                                   resolve addressed GitHub review threads
  crq decline <repo> <pr> --thread <id> [...] --reason "<why>" [--resolve]
                                   reply on a thread to record why a finding is declined
  crq autoreview [--once] [--no-incremental]
                                   keep open PRs reviewed, rate-coordinated
  crq preflight [--type all|committed|uncommitted] [--base <branch>]
                                   local CodeRabbit CLI pre-push review as JSON
  crq doctor                       emit JSON readiness report for agents and humans
  crq status                       print the dashboard
  crq cancel <repo> <pr>           remove queued/in-flight state for a PR
  crq debug <enqueue|pump|refresh|state>
                                   maintenance tools; not for normal review loops

EXIT CODES
  loop: 0 converged/no actionable findings/skipped, 10 actionable feedback, 2 timeout

Configure with environment variables or ~/.config/crq/env. CRQ_REPO points at the gate repo.
For a compact machine-readable contract, read llms.txt in this repository.
Use "crq help <command>" for command-specific guidance.
`)
}

func commandHelp(command string) {
	switch command {
	case "loop":
		fmt.Print(`crq loop <repo> <pr>

Review round primitive for humans and agents. crq coordinates the review trigger,
waits for real feedback on the current PR head, and emits one JSON report to stdout.

Exit codes:
  0   converged, no actionable findings, or skipped because there is nothing to review
  10  actionable findings returned in .findings[]
  2   timed out waiting for feedback

Loop contract:
  crq loop owner/repo 123 > crq-feedback.json
  # if exit 10:
  #   inspect .findings[]
  #   fix only still-valid findings
  #   run project validation
  #   commit and push
  #   resolve addressed .thread_id values with crq resolve
  #   call crq loop again

Never post @coderabbitai review directly; crq is the only trigger.
`)
	case "feedback":
		fmt.Print(`crq feedback <repo> <pr>

Emit current normalized feedback JSON without triggering a new review.

Important JSON fields:
  status       feedback | waiting | converged | skipped | timeout
  head         current PR head short SHA
  reviewed_by  map of required bot -> reviewed-current-head boolean
  findings[]   always an array; empty means no actionable findings found

Each finding has:
  id, bot, severity, path, line, title, body, source, url, commit
  thread_id when GitHub exposes an unresolved review thread

Sources include review_thread, review_comment, review_body, review_prompt, and issue_comment.
`)
	case "resolve":
		fmt.Print(`crq resolve <repo> <pr> --thread <id> [--thread <id>...]

Resolve only GitHub review threads that were actually addressed by the latest fix.
Leave declined, stale, incorrect, or deferred findings unresolved.

Thread IDs come from .findings[].thread_id in crq loop/feedback output.
`)
	case "decline":
		fmt.Print(`crq decline <repo> <pr> --thread <id> [--thread <id>...] --reason "<why>" [--resolve]

Record on the PR why a finding is being declined: posts the reason as a reply on
each review thread. Use this instead of silently leaving a finding unaddressed, so
the next reviewer (and CodeRabbit) can see the decision.

By default the thread stays unresolved (an on-the-record disagreement). Pass
--resolve to also close it ("won't fix"). Thread IDs come from .findings[].thread_id.
`)
	case "autoreview", "auto":
		fmt.Print(`crq autoreview [--once] [--no-incremental]

Keep open PRs in CRQ_SCOPE reviewed, using the same account-wide queue and quota.

  --once            scan once and exit
  --no-incremental  only review PRs that have never been reviewed by CodeRabbit

Use this instead of CodeRabbit native auto-review. Native auto-review must be off.
`)
	case "preflight":
		fmt.Print(`crq preflight [options]

Run the official local CodeRabbit CLI in --agent mode and normalize its JSON stream.
This reviews local git changes before pushing; it does not trigger GitHub PR review.

Options:
  --type all|committed|uncommitted  review scope (default: all)
  --base <branch>                   compare against a base branch
  --base-commit <commit>            compare against a base commit
  --dir <path>                      review a specific git repository directory
  --light                           request CodeRabbit's lighter local review policy
  --timeout <duration>              stop waiting after a Go duration, e.g. 30m
  --bin <path-or-name>              CodeRabbit CLI binary; defaults to cr/coderabbit

Exit codes:
  0   clean/no local findings
  10  local findings returned in .findings[]
  1   setup, auth, CLI, or parsing error
  2   timeout

Use crq loop for queued GitHub PR reviews.
`)
	case "init":
		fmt.Print(`crq init

Initialize crq state in CRQ_REPO. The gate repository must already exist.

Typical setup:
  gh repo create YOURUSER/crq-state --private --add-readme
  export CRQ_REPO=YOURUSER/crq-state
  crq init

Save the printed exports to ~/.config/crq/env on every machine or agent host.
`)
	case "doctor":
		fmt.Print(`crq doctor

Emit a JSON readiness report without mutating GitHub state.

Checks include:
  crq config needed for queued PR loops
  gh availability for GitHub API access
  optional CodeRabbit CLI availability for local pre-push review
  CODERABBIT_API_KEY presence for headless CodeRabbit CLI auth

Use this before a human-run loop, background watcher, or autonomous agent.
`)
	case "status":
		fmt.Print("crq status\n\nPrint the dashboard rendered from the CAS state ref.\n")
	case "cancel":
		fmt.Print("crq cancel <repo> <pr>\n\nRemove a PR from queued/in-flight crq state.\n")
	case "debug":
		fmt.Print(`crq debug <enqueue|pump|refresh|state>

Maintenance tools for diagnosis only. Human and agent review loops should use crq loop.
`)
	default:
		fmt.Printf("unknown help topic: %s\n\n", command)
		usage()
	}
}

func preflight(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	reviewType := fs.String("type", "all", "review type")
	base := fs.String("base", "", "base branch")
	baseCommit := fs.String("base-commit", "", "base commit")
	dir := fs.String("dir", "", "review directory")
	light := fs.Bool("light", false, "lighter local review")
	timeout := fs.Duration("timeout", 0, "timeout")
	binary := fs.String("bin", "", "CodeRabbit CLI binary")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	report, code, err := crq.Preflight(ctx, crq.PreflightOptions{
		Binary:     *binary,
		ReviewType: *reviewType,
		Base:       *base,
		BaseCommit: *baseCommit,
		Dir:        *dir,
		Light:      *light,
		Timeout:    *timeout,
		ExtraArgs:  fs.Args(),
	})
	printJSON(report)
	if err != nil {
		fatal(err)
	}
	return code
}

func repoPR(args []string) (string, int, bool) {
	if len(args) != 2 {
		return "", 0, false
	}
	pr, err := strconv.Atoi(args[1])
	if err != nil || pr <= 0 {
		return "", 0, false
	}
	return args[0], pr, true
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func parseResolveArgs(args []string) ([]string, bool) {
	var positional []string
	var threads []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--thread":
			if i+1 >= len(args) {
				return nil, false
			}
			threads = append(threads, args[i+1])
			i++
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 0 && len(positional) < 2 {
		return nil, false
	}
	if len(positional) > 2 {
		threads = append(threads, positional[2:]...)
	}
	return threads, true
}

func parseDeclineArgs(args []string) (threads []string, reason string, resolve, ok bool) {
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--thread":
			if i+1 >= len(args) {
				return nil, "", false, false
			}
			threads = append(threads, args[i+1])
			i++
		case "--reason":
			if i+1 >= len(args) {
				return nil, "", false, false
			}
			reason = args[i+1]
			i++
		case "--resolve":
			resolve = true
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 0 && len(positional) < 2 {
		return nil, "", false, false
	}
	if len(positional) > 2 {
		threads = append(threads, positional[2:]...)
	}
	return threads, reason, resolve, true
}

func printJSON(value any) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(b))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "crq: %v\n", err)
}

type doctorReport struct {
	Status          string              `json:"status"`
	Version         string              `json:"version"`
	Ready           bool                `json:"ready"`
	ConfigPath      string              `json:"config_path"`
	Config          doctorConfig        `json:"config"`
	GitHub          doctorGitHub        `json:"github"`
	CodeRabbitCLI   doctorCodeRabbitCLI `json:"coderabbit_cli"`
	Tools           map[string]toolInfo `json:"tools"`
	Environment     doctorEnvironment   `json:"environment"`
	AgentCommands   []string            `json:"agent_commands"`
	Recommendations []string            `json:"recommendations"`
}

type doctorConfig struct {
	GateRepo       string   `json:"gate_repo,omitempty"`
	DashboardIssue int      `json:"dashboard_issue,omitempty"`
	CalibrationPR  int      `json:"calibration_pr,omitempty"`
	Scope          []string `json:"scope"`
	StateRef       string   `json:"state_ref"`
	Complete       bool     `json:"complete"`
}

type doctorEnvironment struct {
	CodeRabbitAPIKey bool `json:"coderabbit_api_key"`
}

type doctorGitHub struct {
	Authenticated bool   `json:"authenticated"`
	Error         string `json:"error,omitempty"`
}

type doctorCodeRabbitCLI struct {
	Authenticated bool   `json:"authenticated"`
	AuthType      string `json:"auth_type,omitempty"`
	Provider      string `json:"provider,omitempty"`
	CurrentOrg    string `json:"current_org,omitempty"`
	Error         string `json:"error,omitempty"`
}

type toolInfo struct {
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

func doctor(ctx context.Context) doctorReport {
	cfg, err := crq.LoadConfig()
	if err != nil {
		cfg = crq.Config{}
	}
	tools := map[string]toolInfo{
		"gh":         checkTool(ctx, "gh", "--version"),
		"cr":         checkTool(ctx, "cr", "--version"),
		"coderabbit": checkTool(ctx, "coderabbit", "--version"),
	}
	codeRabbitCLI := checkCodeRabbitAuth(ctx, tools)
	report := doctorReport{
		Status:     "doctor",
		Version:    crq.Version,
		ConfigPath: configPath(),
		Config: doctorConfig{
			GateRepo:       cfg.GateRepo,
			DashboardIssue: cfg.DashboardIssue,
			CalibrationPR:  cfg.CalibrationPR,
			Scope:          cfg.Scope,
			StateRef:       cfg.StateRef,
			Complete:       cfg.GateRepo != "" && cfg.DashboardIssue > 0,
		},
		Tools:         tools,
		GitHub:        checkGitHubAuth(ctx, tools["gh"].Found),
		CodeRabbitCLI: codeRabbitCLI,
		Environment: doctorEnvironment{
			CodeRabbitAPIKey: os.Getenv("CODERABBIT_API_KEY") != "",
		},
		AgentCommands: []string{
			"crq preflight --type uncommitted",
			"crq loop <repo> <pr>",
			"crq feedback <repo> <pr>",
			"crq resolve <repo> <pr> --thread <thread_id>",
			"crq autoreview --once",
		},
		Recommendations: []string{},
	}
	if report.Config.Scope == nil {
		report.Config.Scope = []string{}
	}
	// crq authenticates via GITHUB_TOKEN/GH_TOKEN or the gh CLI, so either path
	// counts as GitHub-ready.
	tokenPresent := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != "" || strings.TrimSpace(os.Getenv("GH_TOKEN")) != ""
	githubReady := report.GitHub.Authenticated || tokenPresent
	report.Ready = report.Config.Complete && githubReady
	if !report.Config.Complete {
		report.Recommendations = append(report.Recommendations, "run crq init and save the printed exports to "+configPath())
	}
	if !githubReady {
		if !report.Tools["gh"].Found {
			report.Recommendations = append(report.Recommendations, "set GITHUB_TOKEN/GH_TOKEN or install GitHub CLI and run gh auth login")
		} else {
			report.Recommendations = append(report.Recommendations, "authenticate GitHub CLI with gh auth login (or set GITHUB_TOKEN/GH_TOKEN)")
		}
	}
	if !report.Tools["cr"].Found && !report.Tools["coderabbit"].Found {
		report.Recommendations = append(report.Recommendations, "optional: install CodeRabbit CLI for local pre-push review with cr review --agent")
	}
	if (report.Tools["cr"].Found || report.Tools["coderabbit"].Found) && !report.Environment.CodeRabbitAPIKey && !report.CodeRabbitCLI.Authenticated {
		report.Recommendations = append(report.Recommendations, "optional: set CODERABBIT_API_KEY or run coderabbit auth login for headless local reviews")
	}
	return report
}

func checkCodeRabbitAuth(ctx context.Context, tools map[string]toolInfo) doctorCodeRabbitCLI {
	binary := ""
	if tools["cr"].Found {
		binary = tools["cr"].Path
	} else if tools["coderabbit"].Found {
		binary = tools["coderabbit"].Path
	}
	if binary == "" {
		return doctorCodeRabbitCLI{Authenticated: false, Error: "CodeRabbit CLI not found"}
	}
	toolCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(toolCtx, binary, "auth", "status", "--agent")
	out, err := cmd.CombinedOutput()
	if toolCtx.Err() != nil {
		return doctorCodeRabbitCLI{Authenticated: false, Error: toolCtx.Err().Error()}
	}
	if err != nil {
		return doctorCodeRabbitCLI{Authenticated: false, Error: firstLine(string(out))}
	}
	var payload struct {
		Authenticated bool   `json:"authenticated"`
		AuthType      string `json:"authType"`
		Provider      string `json:"provider"`
		CurrentOrg    struct {
			Name string `json:"name"`
		} `json:"currentOrg"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return doctorCodeRabbitCLI{Authenticated: false, Error: "failed to parse coderabbit auth status"}
	}
	return doctorCodeRabbitCLI{
		Authenticated: payload.Authenticated,
		AuthType:      payload.AuthType,
		Provider:      payload.Provider,
		CurrentOrg:    payload.CurrentOrg.Name,
	}
}

func checkGitHubAuth(ctx context.Context, ghFound bool) doctorGitHub {
	if !ghFound {
		return doctorGitHub{Authenticated: false, Error: "gh not found"}
	}
	toolCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(toolCtx, "gh", "auth", "status")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return doctorGitHub{Authenticated: true}
	}
	msg := firstLine(string(out))
	if msg == "" {
		msg = strings.TrimSpace(err.Error())
	}
	if toolCtx.Err() != nil {
		msg = toolCtx.Err().Error()
	}
	return doctorGitHub{Authenticated: false, Error: msg}
}

func checkTool(ctx context.Context, name string, args ...string) toolInfo {
	path, err := exec.LookPath(name)
	if err != nil {
		return toolInfo{Found: false, Error: "not found"}
	}
	toolCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(toolCtx, path, args...)
	out, err := cmd.CombinedOutput()
	info := toolInfo{Found: true, Path: path}
	if err != nil {
		info.Error = strings.TrimSpace(err.Error())
		if toolCtx.Err() != nil {
			info.Error = toolCtx.Err().Error()
		}
	}
	info.Version = firstLine(string(out))
	return info
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	line, _, _ := strings.Cut(value, "\n")
	return strings.TrimSpace(line)
}

func configPath() string {
	if v := os.Getenv("CRQ_CONFIG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return "~/.config/crq/env"
	}
	return home + "/.config/crq/env"
}
