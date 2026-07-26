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
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
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
	gh, err := ghapi.NewGitHub(ctx)
	if err != nil {
		fatal(err)
		return 1
	}
	gh.SetLogger(stderrLogger{})
	store := crq.NewGitStateStore(cfg, gh, stderrLogger{})
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
	case "next":
		if bad, found := unknownFlag(args[1:], "--wait"); found {
			fatal(fmt.Errorf("unknown flag %s (usage: crq next <repo> <pr> [--wait])", bad))
			return 1
		}
		repo, pr, ok := repoPR(positional(args[1:]))
		if !ok {
			fatal(errors.New("usage: crq next <repo> <pr> [--wait]"))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		var report crq.NextReport
		var err error
		if hasFlag(args[1:], "--wait") {
			report, err = service.NextWaiting(ctx, repo, pr)
		} else {
			report, err = service.Next(ctx, repo, pr)
		}
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(report)
		// The action field is the whole contract: exit 0 for every action so a
		// caller never has to interpret two things at once.
		return 0
	case "wait":
		repo, pr, ok := repoPR(args[1:])
		if !ok {
			fatal(errors.New("usage: crq wait <repo> <pr>"))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		report, err := service.WaitForAction(ctx, repo, pr)
		if err != nil {
			fatal(err)
			return 1
		}
		printJSON(report)
		// Same contract as next: the action is the answer, the exit code is not.
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
			fatal(errors.New("usage: crq resolve <thread-id> [<thread-id>...]"))
			return 1
		}
		if len(threads) == 0 {
			fatal(errors.New("usage: crq resolve <thread-id> [<thread-id>...]"))
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
			fatal(errors.New(`usage: crq decline <thread-id> [<thread-id>...] --reason "<why>" [--keep-open]`))
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
		printJSON(state.Account)
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
  crq next <repo> <pr>             ask what to do next about a PR (the agent loop)
  crq wait <repo> <pr>             block until there IS something to do, then say what
  crq loop <repo> <pr>             queue one PR review round, then emit JSON feedback
  crq autoreview                   keep open PRs reviewed through the same queue
  crq status                       show the queue, in-flight review, and quota state

DRIVING A PR REVIEW
  Call crq next, do exactly what .action says, call it again. That is the whole loop.

    fix      fix .findings[], validate, then crq resolve (or crq decline) each thread
    hold     do NOT push: a required reviewer is pending; call again at .recheck_after
    push     the head is released — commit and push your fixes once
    wait     nothing to do; call again at .recheck_after
    done     converged
    blocked  needs a human; .reason says why

  crq next is non-blocking and idempotent, so nothing is lost if it is interrupted.
  On wait or hold, hand the delay to crq wait — run it as your harness's background
  task and let its EXIT wake you. It owns nothing, so being killed costs only itself.
  Never invent a delay of your own, and never post @coderabbitai review directly.

USAGE
  crq init                         initialize state in CRQ_REPO
  crq next <repo> <pr> [--wait]    emit the single next action as JSON (--wait blocks)
  crq wait <repo> <pr>             block until actionable, then emit that action as JSON
  crq loop <repo> <pr>             coordinated trigger -> wait -> JSON feedback/convergence
  crq feedback <repo> <pr>         emit normalized actionable review findings as JSON
  crq resolve <thread-id> [<thread-id>...]
                                   resolve addressed GitHub review threads
  crq decline <thread-id> [...] --reason "<why>" [--keep-open]
                                   reply on a thread to record why a finding is declined
                                   (resolves it; --keep-open leaves it open)
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
  next: always 0 on success — read .action, not the exit code
  loop: 0 converged/no actionable findings/skipped, 10 actionable feedback, 2 timeout

Configure with environment variables or ~/.config/crq/env. CRQ_REPO points at the gate repo.
For a compact machine-readable contract, read llms.txt in this repository.
Use "crq help <command>" for command-specific guidance.
`)
}

func commandHelp(command string) {
	switch command {
	case "next":
		fmt.Print(`crq next <repo> <pr> [--wait]

The agent loop, in one command: crq answers what to do next about this PR, you do
exactly that, then you call it again. Non-blocking, idempotent, and it advances the
queue by one step as a side effect — so a PR outside the autoreview fleet still
progresses, and an interrupted caller loses nothing by calling again.

Always exits 0 on success. Read .action; the exit code carries no information.

  fix      .findings[] are actionable for the current head. Fix them, validate, then
           crq resolve each addressed .thread_id (or crq decline with a reason).
  hold     You have work to land but a required reviewer has not answered for this
           head. Do NOT commit or push — that restarts the review. Resolving threads
           is still fine. Call again at .recheck_after.
  push     The head is released. Commit and push the accumulated fixes once.
  wait     Nothing to do until .recheck_after.
  done     Converged: no findings, every required reviewer answered.
  blocked  Needs a human (.reason says why, e.g. the PR was closed).

Fields:
  action            the instruction — the entire contract
  reason            why, in one line
  recheck_after     when to call again (hold and wait). crq computes this from the
                    account-quota window, the round's retry cooldown and the poll
                    interval. Never substitute a delay of your own.
  pending[]         required reviewers with no evidence for this head
  findings[]        actionable feedback (fix); same shape as crq feedback
  local_work        whether crq saw changes the PR head lacks — what separates push
                    from done. Run crq next inside the repository checkout so this
                    is accurate; local_work_reason says when it could not be.

--wait blocks through the states you cannot act on and returns the first actionable
instruction. It shares one code path with the non-blocking form.

Never post @coderabbitai review directly; crq is the only trigger.
`)
	case "wait":
		fmt.Print(`crq wait <repo> <pr>

Block until there is something to DO about this PR, then print that instruction
(the same JSON as crq next) and exit 0.

This is how an ephemeral agent waits. Run it as your harness's background task and
end your turn: its EXIT is the wake event, so you burn no tokens idling and invent
no delay. It holds no round, so if it is killed the round is untouched — just run
it again, or call crq next.

It is read-only in the steady state, but NOT unconditionally: if nothing is
advancing this PR (no round for the head, or no daemon holding the leader lease)
it drives the queue itself rather than wait for nobody, which can request a
review and spend account quota.

It returns on fix, push, done and blocked. wait and hold are the two states it
waits THROUGH, because they mean "come back later" and that is its whole job.

While idle it watches the shared state ref with a conditional request, which costs
no rate-limit quota, and re-evaluates when the queue moves. If no autoreview daemon
holds the leader lease it drives the queue itself instead, which works but spends
more of the shared budget — run the daemon.
`)
	case "loop":
		fmt.Print(`crq loop <repo> <pr>

Review round primitive for humans and agents. crq coordinates the review trigger,
waits for real feedback on the current PR head, and emits one JSON report to stdout.
It returns unresolved findings before queueing a new round, so agents must drain
current feedback before waiting for another review.

Exit codes:
  0   converged, no actionable findings, or skipped because there is nothing to review
  10  actionable findings returned in .findings[]
  2   timed out waiting for feedback

Loop contract:
  # Start one review round:
  crq loop owner/repo 123 > crq-feedback.json
  # if exit 10 (the round may still have pending reviewers):
  #   inspect .findings[]
  #   fix only still-valid findings
  #   run project validation
  #   resolve each addressed .thread_id immediately after its local fix
  #   if any .reviewed_by value is false: HOLD THE HEAD
  #     do not commit or push yet
  #     keep the queued review alive; repeat crq feedback with the same CRQ_REQUIRED_BOTS
  #   after every required bot is true:
  #     fix and resolve any remaining findings
  #     commit and push the combined fixes once
  #   only after the held head advances, call crq loop for the next round

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
		fmt.Print(`crq resolve <thread-id> [<thread-id>...]

Resolve only GitHub review threads that were actually addressed by the latest fix.
Leave declined, stale, incorrect, or deferred findings unresolved.

Thread IDs come from .findings[].thread_id in crq loop/feedback output.
`)
	case "decline":
		fmt.Print(`crq decline <thread-id> [<thread-id>...] --reason "<why>" [--keep-open]

Record on the PR why a finding is being declined: posts the reason as a reply on
each review thread. Use this instead of silently leaving a finding unaddressed, so
the next reviewer (and CodeRabbit) can see the decision.

Declining RESOLVES the thread, because crq reads GitHub's resolution state: a
thread left open keeps its finding actionable, so crq next would repeat "fix"
forever and never reach push or done. The disagreement is not lost — if the bot
replies contesting the decline, crq re-surfaces that reply as its own finding.

Pass --keep-open to leave it unresolved anyway (an on-the-record disagreement you
intend to keep working). Thread IDs come from .findings[].thread_id.
`)
	case "autoreview", "auto":
		fmt.Print(`crq autoreview [--once] [--no-incremental]

Keep open PRs in CRQ_SCOPE reviewed, using the same account-wide queue and quota.
Run only one long-lived autoreview daemon. Manual crq loop calls share its idempotent
queue entry, so they re-attach to the same wait instead of firing a duplicate review.

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
  2   come back later: a timeout, or the CodeRabbit account is rate-limited
      (.status is "rate_limited", with .retry_after and .error_type)

The local CLI spends the same account quota as PR reviews, so a local block is
evidence about that shared quota — read .retry_after rather than re-running.

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

// hasFlag reports whether args contains the exact flag.
func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// unknownFlag returns the first flag in args that is not in allowed.
//
// positional() drops anything starting with "-", so without this a mistyped
// --wiat silently ran the non-blocking form: the caller gets a plausible report
// and waits forever for a blocking call that never happened. A typo must fail.
func unknownFlag(args []string, allowed ...string) (string, bool) {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		known := false
		for _, candidate := range allowed {
			if arg == candidate {
				known = true
				break
			}
		}
		if !known {
			return arg, true
		}
	}
	return "", false
}

// positional drops flag arguments so repoPR sees only <repo> <pr>.
func positional(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
		}
	}
	return out
}

func parseResolveArgs(args []string) ([]string, bool) {
	threads, _, _, ok := parseThreadCommand(args, false)
	return threads, ok
}

func parseDeclineArgs(args []string) (threads []string, reason string, resolve, ok bool) {
	return parseThreadCommand(args, true)
}

// parseThreadCommand parses the shape shared by `crq resolve` and `crq decline`:
// any number of thread IDs, written bare or behind --thread.
//
// Thread node IDs are globally unique, so the <repo> <pr> this command used to
// demand never identified anything — ResolveThreads discarded them. They are
// still accepted so existing call sites keep working, and dropped here. Taking
// IDs bare is what lets a caller clear a whole round in one process instead of
// one subprocess per thread.
//
// An unrecognized flag is an error rather than a positional: a typo like
// `--resove` must fail loudly, not silently become a thread ID.
func parseThreadCommand(args []string, allowReason bool) (threads []string, reason string, resolve, ok bool) {
	// Declining resolves the thread unless the caller asks to keep it open.
	// Leaving it open by default made `crq next` repeat `fix` forever: crq keys
	// off GitHub's resolution state, so a documented decline cleared nothing and
	// the loop could never reach push or done. A bot that disagrees still gets
	// heard — a contested reply is re-surfaced as its own finding.
	resolve = allowReason
	var keepOpen bool
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--thread":
			if i+1 >= len(args) {
				return nil, "", false, false
			}
			threads = append(threads, args[i+1])
			i++
		case allowReason && arg == "--reason":
			if i+1 >= len(args) {
				return nil, "", false, false
			}
			reason = args[i+1]
			i++
		case allowReason && arg == "--resolve":
			// Kept as an accepted no-op: resolving is now the default, and
			// failing on a flag that used to be required would break callers.
			resolve = true
		case allowReason && arg == "--keep-open":
			keepOpen = true
		case strings.HasPrefix(arg, "-"):
			return nil, "", false, false
		default:
			positional = append(positional, arg)
		}
	}
	if keepOpen {
		resolve = false
	}
	return append(threads, dropLegacyTarget(positional)...), reason, resolve, true
}

// dropLegacyTarget removes a leading "owner/repo" plus its PR number — the
// arguments these commands used to require — leaving only thread IDs.
func dropLegacyTarget(positional []string) []string {
	if len(positional) >= 2 && strings.Contains(positional[0], "/") {
		if _, err := strconv.Atoi(positional[1]); err == nil {
			return positional[2:]
		}
	}
	return positional
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
			"crq resolve <thread-id>",
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
