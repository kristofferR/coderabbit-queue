package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/crq"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type stderrLogger struct{}

func (stderrLogger) Printf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "crq: "+format+"\n", args...)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
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
	case "autofix":
		// A dry run is documented as a PREVIEW: it writes nothing and reads no
		// GitHub state. Deciding it here, before the authenticated client is
		// built, is what lets somebody inspect the setup before finishing it —
		// otherwise the one command for looking at the plan was itself another
		// thing to set up first. A real install still goes through the
		// authenticated path below.
		if opts, perr := parseAutofixArgs(args[1:]); perr == nil && opts.dryRun {
			cfg, cerr := crq.LoadConfig()
			if cerr != nil {
				fatal(cerr)
				return 1
			}
			plan, ierr := crq.AutofixPlan(cfg, opts.agent, crq.SplitArgv(opts.agentArgs), opts.repos, true)
			if ierr != nil {
				fatal(ierr)
				return 1
			}
			printJSON(plan)
			return 0
		}
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
		if bad, found := unknownFlag(args[1:], "--line"); found {
			fatal(fmt.Errorf("unknown flag %s (usage: crq status [--line])", bad))
			return 1
		}
		// status takes no target: silently ignoring one would let `crq status 41`
		// look like it reported on that PR.
		if extra := positional(args[1:]); len(extra) > 0 {
			fatal(fmt.Errorf("crq status takes no arguments, got %q (usage: crq status [--line])", extra[0]))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		state, dashboard, err := service.Status(ctx)
		if err != nil {
			fatal(err)
			return 1
		}
		if hasFlag(args[1:], "--line") {
			fmt.Println(crq.StatusLine(state, cfg))
			return 0
		}
		fmt.Print(dashboard)
		return 0
	case "feedback":
		repo, pr, err := target(ctx, service, args[1:], "crq feedback [<repo> <pr>]")
		if err != nil {
			fatal(err)
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
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		repo, pr, err := target(ctx, service, positional(args[1:]), "crq next [<repo> <pr>] [--wait]")
		if err != nil {
			fatal(err)
			return 1
		}
		var report crq.NextReport
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
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		repo, pr, err := target(ctx, service, args[1:], "crq wait [<repo> <pr>]")
		if err != nil {
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
		repo, pr, err := target(ctx, service, args[1:], "crq loop [<repo> <pr>]")
		if err != nil {
			fatal(err)
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
	case "reviewers":
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		return runReviewers(ctx, service, args[1:])
	case "threads":
		repo, pr, ok := repoPR(args[1:])
		if !ok {
			fatal(errors.New("usage: crq threads <repo> <pr>"))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		threads, terr := service.OpenThreads(ctx, repo, pr)
		if terr != nil {
			fatal(terr)
			return 1
		}
		printJSON(threads)
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
	case "autofix":
		switch sub := autofixSubcommand(args[1:]); sub {
		case "?":
			fatal(fmt.Errorf("unknown autofix subcommand %q (try: crq autofix, crq autofix on|off|default <repo>, crq autofix install)", args[1]))
			return 1
		case "", "list":
			if err := cfg.RequireState(); err != nil {
				fatal(err)
				return 1
			}
			settings, derr := service.AutofixSettings(ctx)
			if derr != nil {
				fatal(derr)
				return 1
			}
			printJSON(settings)
			return 0
		case "on", "off", "default":
			rest, reason, ok := parseAutofixReason(args[2:])
			if !ok || len(rest) != 1 {
				fatal(errors.New(`usage: crq autofix on|off|default <repo> [--reason "<why>"]`))
				return 1
			}
			if err := cfg.RequireState(); err != nil {
				fatal(err)
				return 1
			}
			if sub == "default" {
				cleared, cerr := service.ClearAutofixEnabled(ctx, rest[0])
				if cerr != nil {
					fatal(cerr)
					return 1
				}
				printJSON(map[string]any{"repo": crq.NormalizeRepo(rest[0]), "cleared": cleared, "enabled": true, "default": true})
				return 0
			}
			setting, serr := service.SetAutofixEnabled(ctx, rest[0], sub == "on", reason)
			if serr != nil {
				fatal(serr)
				return 1
			}
			printJSON(setting)
			return 0
		}
		opts, perr := parseAutofixArgs(args[1:])
		if perr != nil {
			fatal(perr)
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		plan, ierr := service.InstallAutofix(ctx, opts.agent, crq.SplitArgv(opts.agentArgs), opts.repos, opts.dryRun)
		if ierr != nil {
			fatal(ierr)
			return 1
		}
		printJSON(plan)
		return 0
	case "watch":
		fs := flag.NewFlagSet("watch", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		// Split on "--" BEFORE parsing: FlagSet.Parse consumes the terminator, so
		// looking for it in fs.Args() afterwards never finds it and the fix
		// command is silently read as a list of repositories.
		flagArgs, command := args[1:], []string(nil)
		for i, arg := range flagArgs {
			if arg == "--" {
				command = flagArgs[i+1:]
				// Three-index slice: without capping capacity, the flag half
				// still reaches into the command half, and anything that appends
				// to a slice derived from it — fs.Args(), which is exactly what
				// the repository list becomes — writes over the command.
				flagArgs = flagArgs[:i:i]
				break
			}
		}
		// Dispatch is the default. Keep both the standard boolean form
		// --dispatch=false and the more explicit --no-dispatch observer alias.
		dispatch := fs.Bool("dispatch", true, "start a fix session for a PR that needs one (default)")
		noDispatch := fs.Bool("no-dispatch", false, "observe only: report what each PR needs and fix nothing")
		once := fs.Bool("once", false, "run one pass and exit")
		interval := fs.Duration("interval", 0, "time between passes")
		attempts := fs.Int("max-attempts", 0, "dispatches allowed per head")
		concurrency := fs.Int("concurrency", 0, "cap on concurrent fix sessions (0 = no cap)")
		if err := fs.Parse(flagArgs); err != nil {
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		opts := crq.WatchOptions{
			Once:     *once,
			Interval: *interval, MaxAttempts: *attempts,
			Command:  command,
			Dispatch: watchDispatchOption(*dispatch, *noDispatch),
		}
		// Only when it was actually passed: `--concurrency 0` means "no cap" and
		// has to override a configured one, which an unset flag must not do.
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "concurrency" {
				opts.Concurrency = concurrency
			}
		})
		opts.Repos = fs.Args()
		enc := json.NewEncoder(os.Stdout)
		werr := service.Watch(ctx, opts, func(e crq.WatchEvent) error {
			return enc.Encode(e)
		})
		if werr != nil && !errors.Is(werr, context.Canceled) {
			fatal(werr)
			return 1
		}
		return 0
	case "hold", "unhold":
		if args[0] == "hold" && len(args[1:]) == 0 {
			if err := cfg.RequireState(); err != nil {
				fatal(err)
				return 1
			}
			holds, herr := service.Holds(ctx)
			if herr != nil {
				fatal(herr)
				return 1
			}
			printJSON(holds)
			return 0
		}
		rest, reason, ok := parseReasonArgs(args[1:])
		// Exactly two: `crq hold owner/repo 12 13` is a malformed administrative
		// command, and silently holding 12 is the wrong answer to it.
		if !ok || len(rest) != 2 || (args[0] == "unhold" && hasReasonArg(args[1:])) {
			fatal(errors.New(`usage: crq hold <repo> <pr> --reason "<why>" | crq unhold <repo> <pr>`))
			return 1
		}
		repo, pr, valid := repoPR(rest[:2])
		if !valid {
			fatal(fmt.Errorf("bad target %q %q", rest[0], rest[1]))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		var result crq.HoldResult
		var herr error
		if args[0] == "hold" {
			result, herr = service.Hold(ctx, repo, pr, reason)
		} else {
			result, herr = service.Unhold(ctx, repo, pr)
		}
		if herr != nil {
			fatal(herr)
			return 1
		}
		printJSON(result)
		return 0
	case "tidy":
		dryRun := hasFlag(args[1:], "--dry-run")
		repo, pr, ok := repoPR(positional(args[1:]))
		if !ok {
			fatal(errors.New("usage: crq tidy <repo> <pr> [--dry-run]"))
			return 1
		}
		if bad, found := unknownFlag(args[1:], "--dry-run"); found {
			fatal(fmt.Errorf("unknown flag %s (usage: crq tidy <repo> <pr> [--dry-run])", bad))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		result, terr := service.Tidy(ctx, repo, pr, dryRun)
		if terr != nil {
			fatal(terr)
			return 1
		}
		printJSON(result)
		return 0
	case "dismiss":
		rest, reason, ok := parseDismissArgs(args[1:])
		if !ok || strings.TrimSpace(reason) == "" {
			fatal(errors.New(`usage: crq dismiss <repo> <pr> <finding-id> [<finding-id>...] --reason "<why>"`))
			return 1
		}
		if len(rest) < 3 {
			fatal(errors.New(`usage: crq dismiss <repo> <pr> <finding-id> [<finding-id>...] --reason "<why>"`))
			return 1
		}
		// Same target validation as every other command, so a non-numeric or
		// non-positive PR fails the same way here as there.
		repo, pr, ok := repoPR(rest[:2])
		if !ok {
			fatal(fmt.Errorf("bad target %q %q (usage: crq dismiss <repo> <pr> <finding-id>... --reason \"<why>\")", rest[0], rest[1]))
			return 1
		}
		if err := cfg.RequireState(); err != nil {
			fatal(err)
			return 1
		}
		result, derr := service.Dismiss(ctx, repo, pr, rest[2:], reason)
		if derr != nil {
			fatal(derr)
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
		repo, pr, err := target(ctx, service, args[1:], "crq cancel [<repo> <pr>]")
		if err != nil {
			fatal(err)
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

func watchDispatchOption(dispatch, noDispatch bool) *bool {
	if dispatch && !noDispatch {
		return nil
	}
	off := false
	return &off
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
		repo, pr, err := target(ctx, service, args[1:], "crq debug enqueue [<repo> <pr>]")
		if err != nil {
			fatal(err)
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
  crq next [<repo> <pr>]           ask what to do next about a PR (the agent loop)
  crq wait [<repo> <pr>]           block until there IS something to do, then say what
  crq loop <repo> <pr>             queue one PR review round, then emit JSON feedback
  crq autoreview                   keep open PRs reviewed through the same queue
  crq status [--line]              show the queue, in-flight review, and quota state

DRIVING A PR REVIEW
  Call crq next, do exactly what .action says, call it again. That is the whole loop.

    fix      fix .findings[], validate, then crq resolve (or crq decline) each thread
             (no .thread_id? crq dismiss it once judged — at this head nothing else can)
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
  crq next [<repo> <pr>] [--wait]  emit the single next action as JSON (--wait blocks)
                                   omit the target inside a checkout: crq reads the
                                   remote and branch to find the pull request
  crq wait <repo> <pr>             block until actionable, then emit that action as JSON
  crq loop [<repo> <pr>]           coordinated trigger -> wait -> JSON feedback/convergence
  crq feedback [<repo> <pr>]       emit normalized actionable review findings as JSON
  crq resolve <thread-id> [<thread-id>...]
                                   resolve addressed GitHub review threads
  crq decline <thread-id> [...] --reason "<why>" [--keep-open]
                                   reply on a thread to record why a finding is declined
                                   (resolves it; --keep-open leaves it open)
  crq autofix install [--agent <path>] [--dry-run] [<repo>...]
                                   install and start unattended autofix
  crq autofix [on|off|default <repo>]
                                   which repositories crq may fix (on by default)
  crq watch [--no-dispatch] [--once] [<repo>...] [-- <fix command>]
                                   drive open PRs through crq next; --dispatch starts a
                                   session to fix the ones that need it
  crq hold <repo> <pr> --reason "<why>"
                                   stop crq reviewing a PR (crq hold lists them)
  crq unhold <repo> <pr>           put it back in the queue
  crq tidy <repo> <pr> [--dry-run] remove crq's own spent review-trigger comments
  crq reviewers <repo>             which bots review this project (and what each costs)
  crq reviewers set <repo> [--bots <a,b>] [--required <a,b>]
                                   choose this project's reviewers (either flag alone)
  crq reviewers clear <repo>       go back to the fleet default
  crq threads <repo> <pr>          list every unresolved review thread, outdated ones included
  crq dismiss <repo> <pr> <finding-id> [...] --reason "<why>"
                                   account for a finding GitHub gives you no thread to close
  crq autoreview [--once] [--no-incremental]
                                   keep open PRs reviewed, rate-coordinated
  crq preflight [--type all|committed|uncommitted] [--base <branch>]
                                   local CodeRabbit CLI pre-push review as JSON
  crq doctor                       emit JSON readiness report for agents and humans
  crq status [--line]              print the dashboard, or one line for a status bar
  crq cancel [<repo> <pr>]         remove queued/in-flight state for a PR
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
It returns unresolved findings before queueing a new round, so agents must clear
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
	case "autofix":
		fmt.Print(`crq autofix install [--agent <path>] [--dry-run] [<repo>...]
crq autofix                            (which repositories crq may fix)
crq autofix off <repo> [--reason "<why>"]
crq autofix on <repo>
crq autofix default <repo>             (back to the fleet default, which is on)

Install and start unattended autofix: crq watches every open PR and
starts a fix session for the ones that need one.

Autofix is ON for every repository in scope. That is the point of watching: a
pull request nobody fixes is a queue that reports work and does none. Turn it off
where you do not want crq writing code — a release branch, a repository you are
hand-tuning — and watching continues there regardless, so reviews still arrive
for a person to act on. The setting lives in the state ref, next to the reviewer
overrides, because the daemon has no checkout of what it watches.

It writes the fix prompt, a wrapper, and a service definition for this platform
(a systemd user unit, or a launchd agent on macOS), turns on whatever that
platform needs to survive a logout, and starts it. --dry-run prints the paths and
commands without touching anything.

The agent defaults to "claude" on PATH; --agent takes another path to it, or a
wrapper around it. The installed service runs the agent with Claude Code's own
flags (-p, --permission-mode, --output-format), so an agent with a different CLI
needs its own wrapper script and "crq watch -- <cmd>...", which runs
whatever argv you give it. --agent must name something runnable; a name that
cannot be resolved is refused here rather than at the first dispatch.

It is run with permissions bypassed because it is unattended — an interactive
prompt for "go test" or "git push" would hang forever with nobody to answer it.
What keeps that bounded is the isolated worktree, one claimed session per PR,
CRQ_DISPATCH_MAX_ATTEMPTS per head, and the prompt's stated scope.

Repositories default to CRQ_REPOS.
`)
	case "watch":
		fmt.Print(`crq watch [--no-dispatch] [--once] [--interval <d>] [--max-attempts <n>] [<repo>...] [-- <cmd>...]

Drive every open PR through the same crq next oracle an agent uses, emit one JSON
line per PR per pass, and start a fix session for each PR whose action is "fix".

Fixing is the default — watching a pull request nobody fixes is a queue that
reports work and does none. Three things turn it off: --no-dispatch for one run,
"crq autofix off <repo>" for one repository, and having no fix command configured
at all, which observes and says so rather than refusing to start.

A dispatched session runs in a worktree crq checked out at that head, with:

  CRQ_DISPATCH_REPO      owner/name
  CRQ_DISPATCH_PR        the number
  CRQ_DISPATCH_HEAD      the commit the findings are about
  CRQ_DISPATCH_FINDINGS  path to the findings as JSON

The command comes from CRQ_DISPATCH_CMD or from everything after --. It is run
directly, not through a shell, so nothing expands that you did not write.

Sessions run concurrently and OFF the decision loop, with no cap by default. The
queue exists for the account-metered review and nothing else; fixing findings
spends none of that allowance, so a PR whose findings are ready gets a session
now rather than a place in a line. CRQ_DISPATCH_CONCURRENCY (or --concurrency)
sets a cap if a machine cannot take the load — a resource valve, not a queue.
--concurrency 0 turns a configured cap off for one run.

The decisions themselves stay serial, because that is what keeps the metered
review in one queue.

crq still does not decide which findings are real — it starts the session and
says which PR to look at; the session judges. Every dispatch is claimed under
compare-and-swap, so two watchers cannot both work one PR, and bounded per head
(CRQ_DISPATCH_MAX_ATTEMPTS, default 3) so a fix that keeps not working stops
instead of spending a review round each time.

Repositories default to CRQ_REPOS.
`)
	case "hold", "unhold":
		fmt.Print(`crq hold <repo> <pr> --reason "<why>"
crq unhold <repo> <pr>
crq hold                             (lists what is held)

Stop crq requesting reviews for a PR, in one write.

Holding used to take two commands that could not be one: the skip marker stops
fleet auto-review from enqueueing, crq cancel stops the pump, and between the two
a daemon fired anyway. A hold is a single fact recorded where every firing path
already looks, so there is no window between the halves.

A hold does not cancel a review already in flight — that one is bought, and its
findings are still worth having. It stops the next one. The reason is required:
it is the note to whoever finds the PR stopped. A live autoreview daemon that
advertises hold support is required, so its lease keeps an older standby out.
`)
	case "tidy":
		fmt.Print(`crq tidy <repo> <pr> [--dry-run]

Delete the review-trigger comments crq posted that nothing needs any more. A PR
driven through a dozen rounds collects a dozen "@coderabbitai review" comments,
which buries the conversation a human came to read.

A comment is removed only when all three hold:

  * crq WROTE it, and it still READS as that one-line command. The candidates
    are the comments each round recorded posting, not anything matching the text
    and not one the round adopted: a person's "@coderabbitai review" is their
    decision to ask and not crq's to erase. crq posts under your own account, so
    a recorded comment someone has since edited is their words, and it stays.
  * the round that asked has PROGRESSED. A live round keeps its command, because
    that is the comment crq adopts instead of posting another one.
  * the bot answered it, and it predates the current head. Adoption only ever
    considers commands newer than the head commit, so deleting one of those
    would make the next pump post a duplicate and buy a second review; a head
    crq cannot read leaves the check unevaluable, and everything stays. Only a
    request crq's own retry replaced is spent whatever its timestamp says.

It never deletes the bots' own comments. An auto-generated reply can be a
rate-limit or skipped-review notice, which crq reads as evidence and surfaces as
a finding — deleting those would destroy feedback nobody had read yet.

Set CRQ_TIDY=1 to run it automatically as rounds progress under crq autoreview.
--dry-run reports what it would remove.
`)
	case "reviewers":
		fmt.Print(`crq reviewers <repo>
crq reviewers set <repo> [--bots <login,...>] [--required <login,...>]
crq reviewers clear <repo>

Which bots review one project, and what each of them costs.

Without a subcommand it reports the reviewers that will actually run there — the
fleet default, or this repository's own choice if it has one. Each entry carries
its budget: "account" is serialized against the shared CodeRabbit allowance,
"none" runs immediately, outside that queue. Budget is not requiredness: whether
a round WAITS for a reviewer is --required, whatever the reviewer costs.

  set     --bots chooses the co-reviewers; --required chooses which reviewers
          gate convergence. Either flag alone updates only its own half. An
          empty --bots means none here, which is a different answer from not
          setting it at all; an empty --required is refused, because a round
          that gates on nobody converges before any reviewer runs.
  clear   drops the override so the repository follows the fleet again.

The configuration lives in the shared state ref, not in a file the repository
carries: the daemon has no checkout of the repos it reviews, and a daemon and an
agent reading different configurations while writing one state ref is a class of
bug worth not having.

The primary reviewer is fleet-wide. Its markers and command are compiled into the
classifiers when crq starts, so a per-repo primary would mean per-repo
classifiers — a much larger change than choosing who else runs.
`)
	case "threads":
		fmt.Print(`crq threads <repo> <pr>

List the PR's unresolved review threads as JSON, including the ones GitHub has
marked OUTDATED, with .thread_id ready for crq resolve.

Findings leave outdated threads out on purpose: the code they point at is gone,
and anything carrying a thread ID blocks the round until it is resolved. But an
outdated thread is still open on the PR — and after a push that is every thread
from the previous head, so fixing and pushing used to leave no way to close them
through crq at all.

Read this, decide, then crq resolve (or crq decline) the ones you have answered.
`)
	case "dismiss":
		fmt.Print(`crq dismiss <repo> <pr> <finding-id> [<finding-id>...] --reason "<why>"

Record that you have accounted for a finding that has no review thread, so it
stops blocking the round.

crq resolve and crq decline both act on a thread. A review-body finding, a
review-skipped notice or an outside-diff remark has none, so neither command can
touch it — and a finding that can never be cleared blocks every future round, leaving
a PR whose current head no review was ever requested for.

Finding IDs come from .findings[].id. They are content-derived, not GitHub node
IDs, so the repo and PR are required. A dismissal covers the current head only:
push, and the next reviewer has to report it again.

Use it for a finding you have judged and set aside. Fix what is real instead — and
for a review crq was told was SKIPPED, narrowing the PR fixes the cause, while
dismissing only records that you decided to live with it at this head.
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
evidence about that shared quota. crq records it for the whole fleet — no probe
comment on the calibration PR, though the state write itself is a GitHub call —
and .quota says whether it did. It only ever EXTENDS a standing block, and only
when the CLI attributes the limit to the organisation crq queues for; otherwise
.quota.reason says why not. Read .retry_after rather than re-running.

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
		fmt.Print(`crq status [--line]

Print the dashboard rendered from the CAS state ref.

--line prints ONE line instead, for a harness status bar:

    crq 🔬 #41 reviewing · 2 queued

That answers "is it still going?" continuously, which is otherwise the question a
session spends the most tool calls on. It names the next PR only when the queue
actually knows which one that is.
`)
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
	// Before printing: a local block is evidence about the SHARED account quota,
	// so hand it to the queue rather than letting it die in this process. Local
	// preflight must keep working with no crq config and no GitHub token, so this
	// is best-effort and its outcome is reported rather than enforced.
	report.Quota = shareCLIQuota(ctx, report)
	printJSON(report)
	if err != nil {
		fatal(err)
	}
	return code
}

// shareCLIQuota records a CLI-reported account block in crq's shared state.
// Every failure path is a nil result with a reason, never an error: preflight is
// a local review command and must not start depending on GitHub.
func shareCLIQuota(ctx context.Context, report crq.PreflightReport) *crq.CLIQuotaResult {
	if !crq.IsCLIAccountBlock(report) {
		return nil
	}
	cfg, err := crq.LoadConfig()
	if err != nil {
		return &crq.CLIQuotaResult{Reason: "could not read crq config: " + err.Error()}
	}
	if err := cfg.RequireState(); err != nil {
		return &crq.CLIQuotaResult{Reason: "no crq state configured: " + err.Error()}
	}
	// Bound BEFORE resolving credentials. With no GITHUB_TOKEN/GH_TOKEN,
	// NewGitHub shells out to `gh auth token`, and a wedged credential store
	// would hang a preflight that has already produced its findings. The
	// transport then rides out a network outage indefinitely by default, which is
	// right for a review loop and wrong for a courtesy write.
	shareCtx, cancel := context.WithTimeout(ctx, cliQuotaShareTimeout)
	defer cancel()
	gh, err := ghapi.NewGitHub(shareCtx)
	if err != nil {
		return &crq.CLIQuotaResult{Reason: "could not reach github to record the block: " + err.Error()}
	}
	store := crq.NewGitStateStore(cfg, gh, stderrLogger{})
	service := crq.NewService(cfg, gh, store, stderrLogger{})
	result, err := service.RecordCLIQuota(shareCtx, report, codeRabbitOrg(shareCtx, report.Tool))
	if err != nil {
		return &crq.CLIQuotaResult{Reason: "could not record the block: " + err.Error()}
	}
	return &result
}

// cliQuotaShareTimeout bounds the best-effort state write. Local preflight is not
// allowed to become slower or less reliable because crq also wants to share what
// it learned.
const cliQuotaShareTimeout = 20 * time.Second

// codeRabbitOrg reads which CodeRabbit organisation produced this block, so it is
// only ever applied to the account it belongs to.
//
// It asks the SAME executable preflight ran. Probing cr/coderabbit from PATH
// instead discarded a valid block as "unknown organisation" whenever --bin or
// CRQ_CODERABBIT_BIN pointed elsewhere — and, worse, could read a different
// install's login and attribute the block to the wrong account.
func codeRabbitOrg(ctx context.Context, binary string) string {
	tools := map[string]toolInfo{}
	if strings.TrimSpace(binary) != "" {
		tools["cr"] = toolInfo{Found: true, Path: binary}
	} else {
		tools["cr"] = checkTool(ctx, "cr", "--version")
		tools["coderabbit"] = checkTool(ctx, "coderabbit", "--version")
	}
	return checkCodeRabbitAuth(ctx, tools).CurrentOrg
}

// parseReasonArgs splits positional arguments from a --reason value. An
// unrecognized flag is an error rather than a positional: a typo like --resaon
// must fail loudly, not silently become part of the target.
func parseReasonArgs(args []string) (rest []string, reason string, ok bool) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--reason":
			if i+1 >= len(args) {
				return nil, "", false
			}
			reason = args[i+1]
			i++
		case strings.HasPrefix(arg, "--reason="):
			reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			return nil, "", false
		default:
			rest = append(rest, arg)
		}
	}
	return rest, reason, true
}

func hasReasonArg(args []string) bool {
	for _, arg := range args {
		if arg == "--reason" || strings.HasPrefix(arg, "--reason=") {
			return true
		}
	}
	return false
}

// runReviewers handles `crq reviewers [set|clear] <repo> [flags]`.
func runReviewers(ctx context.Context, service *crq.Service, args []string) int {
	action, rest := "show", args
	if len(args) > 0 && (args[0] == "set" || args[0] == "clear") {
		action, rest = args[0], args[1:]
	}
	var repo string
	var bots, required *string
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--bots", arg == "--required":
			if i+1 >= len(rest) {
				fatal(fmt.Errorf("%s needs a value", arg))
				return 1
			}
			value := rest[i+1]
			i++
			if arg == "--bots" {
				bots = &value
			} else {
				required = &value
			}
		case strings.HasPrefix(arg, "--bots="):
			value := strings.TrimPrefix(arg, "--bots=")
			bots = &value
		case strings.HasPrefix(arg, "--required="):
			value := strings.TrimPrefix(arg, "--required=")
			required = &value
		case strings.HasPrefix(arg, "-"):
			fatal(fmt.Errorf("unknown flag %s (usage: crq reviewers set <repo> --bots <a,b>)", arg))
			return 1
		case repo == "":
			repo = arg
		default:
			fatal(fmt.Errorf("unexpected argument %q", arg))
			return 1
		}
	}
	if repo == "" {
		fatal(errors.New("usage: crq reviewers [set|clear] <repo> [--bots <a,b>] [--required <a,b>]"))
		return 1
	}

	var view crq.ReviewerView
	var err error
	switch action {
	case "clear":
		// Ignoring a mutation flag here would turn a malformed automation call
		// like `reviewers clear repo --bots codex` into a silent wipe.
		if bots != nil || required != nil {
			fatal(errors.New("clear takes no --bots/--required (it drops the whole override)"))
			return 1
		}
		view, err = service.ClearReviewers(ctx, repo)
	case "set":
		if bots == nil && required == nil {
			fatal(errors.New("set needs --bots or --required (crq reviewers clear <repo> drops the override)"))
			return 1
		}
		view, err = service.SetReviewers(ctx, repo, splitList(bots), splitList(required))
	default:
		// `crq reviewers owner/repo --bots codex` is a set command missing its
		// verb. Showing the configuration and exiting 0 tells automation the
		// mutation worked when nothing changed.
		if bots != nil || required != nil {
			fatal(errors.New("did you mean `crq reviewers set`? --bots/--required only apply to set"))
			return 1
		}
		view, err = service.Reviewers(ctx, repo)
	}
	if err != nil {
		fatal(err)
		return 1
	}
	printJSON(view)
	return 0
}

// splitList turns an unset flag into nil and an empty one into an empty
// non-nil slice: "not chosen" and "chosen to be none" are different answers.
func splitList(value *string) []string {
	if value == nil {
		return nil
	}
	out := []string{}
	for _, part := range strings.Split(*value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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

// target resolves which PR a command is about: the two arguments if given, and
// otherwise the checkout the caller is standing in. Agents drive the loop from
// inside the repository, so making them carry a repo and number they are already
// standing in is two chances to name the wrong PR silently.
func target(ctx context.Context, service *crq.Service, args []string, usage string) (string, int, error) {
	switch repo, pr, ok := repoPR(args); {
	case ok:
		return repo, pr, nil
	case len(args) == 0:
		return service.InferTarget(ctx)
	default:
		return "", 0, fmt.Errorf("usage: %s", usage)
	}
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

// parseDismissArgs splits `crq dismiss <repo> <pr> <id>...` from its --reason.
// Unlike a thread ID, a finding ID is not globally unique — it is a hash of the
// finding's own text — so the repo and PR genuinely identify something here and
// are required.
func parseDismissArgs(args []string) (rest []string, reason string, ok bool) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--reason":
			if i+1 >= len(args) {
				return nil, "", false
			}
			reason = args[i+1]
			i++
		case strings.HasPrefix(arg, "--reason="):
			reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			return nil, "", false // a typo must fail, not become a finding ID
		default:
			rest = append(rest, arg)
		}
	}
	return rest, reason, true
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
	OtherInstalls   []otherInstall      `json:"other_installs"`
	AgentCommands   []string            `json:"agent_commands"`
	Recommendations []string            `json:"recommendations"`
}

// otherInstall is another crq executable this host can run. Every one of them
// writes the same state ref, and an older one erases what a newer one recorded.
type otherInstall struct {
	Path      string `json:"path"`
	SameBuild bool   `json:"same_build"`
	ModTime   string `json:"mod_time,omitempty"`
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
	report.OtherInstalls = otherInstalls()
	for _, other := range report.OtherInstalls {
		if !other.SameBuild {
			report.Recommendations = append(report.Recommendations,
				"replace or remove "+other.Path+": a different crq build writes the same state ref, and an older one erases fields this one records")
		}
	}
	return report
}

// otherInstalls finds every other crq this host can run.
//
// They all write one state ref, and a build old enough to predate a field
// simply drops it: one stale binary quietly erased every dispatch claim in the
// fleet, so three fix sessions ran on one pull request at once. Version
// tolerance carries unknown members, but only for a binary that HAS it — the one
// that does the damage is by definition older than the mechanism meant to stop
// it. Nothing in this build can prevent that, which is why it is worth naming.
//
// Compared by content, not by version: the version string only changes at a
// release, so two builds from either side of one report the same thing and it
// cannot tell them apart. GOPATH/bin is included whether or not it is on PATH —
// the binary that caused this was not on the daemon's PATH and ran anyway.
func otherInstalls() []otherInstall {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	mine, err := fileDigest(self)
	if err != nil {
		return nil
	}
	dirs := filepath.SplitList(os.Getenv("PATH"))
	if gopath := strings.TrimSpace(os.Getenv("GOPATH")); gopath != "" {
		dirs = append(dirs, filepath.Join(gopath, "bin"))
	} else if home, herr := os.UserHomeDir(); herr == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	var found []otherInstall
	seen := map[string]bool{self: true}
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		path := filepath.Join(dir, "crq")
		if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
			path = resolved
		}
		if seen[path] {
			continue
		}
		info, serr := os.Stat(path)
		if serr != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		seen[path] = true
		entry := otherInstall{Path: path, ModTime: info.ModTime().UTC().Format(time.RFC3339)}
		if digest, derr := fileDigest(path); derr == nil {
			entry.SameBuild = digest == mine
		}
		found = append(found, entry)
	}
	return found
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
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

// autofixArgs is `crq autofix install`'s parsed command line.
type autofixArgs struct {
	agent     string
	agentArgs string
	dryRun    bool
	repos     []string
}

// parseAutofixArgs is shared by the pre-authentication dry-run path and the
// install itself, so the two cannot disagree about what was asked for.
func parseAutofixArgs(args []string) (autofixArgs, error) {
	fs := flag.NewFlagSet("autofix", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agent := fs.String("agent", "", "fix agent to run: claude or codex (default: claude on PATH)")
	agentArgs := fs.String("agent-args", "", "extra flags for the agent, e.g. model and reasoning effort")
	dryRun := fs.Bool("dry-run", false, "print what would be written and run")
	sub, rest := "", args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return autofixArgs{}, err
	}
	if sub != "install" {
		return autofixArgs{}, errors.New("usage: crq autofix install [--agent <path>] [--dry-run] [<repo>...]")
	}
	return autofixArgs{agent: *agent, agentArgs: *agentArgs, dryRun: *dryRun, repos: fs.Args()}, nil
}

// autofixSubcommand names what `crq autofix ...` was asked to do. An empty string
// means the bare command, which lists; "?" means something this command does not
// have.
//
// A typo must not list. `crq autofix of owner/name` reading as the bare listing
// would report the repository as fixable and leave it fixable — an answer to a
// question nobody asked, in place of the instruction that was meant.
func autofixSubcommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "on", "off", "default", "list", "install":
		return args[0]
	}
	return "?"
}

// parseAutofixReason splits `<repo>` from an optional --reason value. An
// unrecognized flag is an error rather than a positional: a typo like --resaon
// must fail loudly, not silently become the repository name.
func parseAutofixReason(args []string) (rest []string, reason string, ok bool) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--reason":
			if i+1 >= len(args) {
				return nil, "", false
			}
			reason = args[i+1]
			i++
		case strings.HasPrefix(arg, "--reason="):
			reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			return nil, "", false
		default:
			rest = append(rest, arg)
		}
	}
	return rest, reason, true
}
