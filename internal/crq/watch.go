package crq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// WatchOptions configures `crq watch`.
type WatchOptions struct {
	// Repos to watch. Empty means every repository in CRQ_REPOS.
	Repos []string
	// Interval between passes.
	Interval time.Duration
	// Once runs a single pass and returns.
	Once bool
	// Dispatch turns "a PR needs fixing" into a session that fixes it. Off by
	// default: watching is an observation, dispatching writes code.
	Dispatch bool
	// Command is the fix session to run, argv-style. Empty means CRQ_DISPATCH_CMD.
	Command []string
	// MaxAttempts bounds dispatches per head. 0 means the configured default.
	MaxAttempts int
	// Concurrency caps how many fix sessions run at once. nil takes the
	// configured cap; 0 means no cap, which is the default: fixing findings
	// spends no CodeRabbit quota, so it has no reason to queue. It exists only as
	// a resource valve for a machine that cannot take the load.
	//
	// A pointer because 0 is a real instruction here, not an unset int: passing
	// `--concurrency 0` is how one run overrides a cap set in
	// CRQ_DISPATCH_CONCURRENCY, and an int cannot tell that from no flag at all.
	Concurrency *int
}

// WatchEvent is one PR's state at a pass, and what the watcher did about it.
type WatchEvent struct {
	Repo     string `json:"repo"`
	PR       int    `json:"pr"`
	Action   string `json:"action"`
	Reason   string `json:"reason,omitempty"`
	Findings int    `json:"findings"`
	// Dispatched says a fix session was started for this event; Skipped says why
	// one was not, when dispatch was enabled and the action asked for it.
	Dispatched bool      `json:"dispatched,omitempty"`
	Skipped    string    `json:"skipped,omitempty"`
	At         time.Time `json:"at"`
}

// Watch drives every open PR in scope through the same `next` oracle an agent
// uses, and — with Dispatch — starts a session to fix the ones that need it.
//
// The queue's stated non-goal is that crq does not write code or decide which
// findings are real. Dispatch does not change that: crq starts a session and
// tells it which PR to look at; the session does the judging. That is why this
// is a separate command over the same oracle rather than something the pump
// does, and why it is off unless asked for.
//
// Every dispatch is claimed under CAS, so two watchers cannot both spawn a
// session for one PR, and bounded per head, so a fix that keeps not working
// stops instead of spending a review round each time.
func (s *Service) Watch(ctx context.Context, opts WatchOptions, emit func(WatchEvent) error) error {
	if opts.Interval <= 0 {
		opts.Interval = s.cfg.WatchInterval
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = s.cfg.DispatchMaxAttempts
	}
	if opts.Dispatch && len(opts.Command) == 0 {
		opts.Command = s.cfg.DispatchCommand
	}
	if opts.Dispatch && len(opts.Command) == 0 {
		return errors.New("dispatch needs a command: set CRQ_DISPATCH_CMD, or pass one after --")
	}
	// Fix sessions run OUTSIDE the pass, and by default without a cap.
	//
	// The queue exists for exactly one thing: the account-metered review. Fixing
	// findings spends none of that allowance, so making a PR wait for a dispatch
	// slot queues work that has no reason to wait — the same mistake crq already
	// corrected for co-only rounds, which bypass the slot and the quota gate
	// entirely. A PR whose findings are ready gets a session now.
	//
	// The decisions stay serial on purpose: `Next` is what enqueues and fires, so
	// deciding one PR at a time is what keeps the metered review in one queue.
	// Only the sessions overlap.
	//
	// The configured cap applies unless this run states one: sizing the pool from
	// an unset option ignored the cap an operator set in CRQ_DISPATCH_CONCURRENCY,
	// which is precisely the machine that cannot take the load.
	concurrency := s.cfg.DispatchConcurrency
	if opts.Concurrency != nil {
		concurrency = *opts.Concurrency
	}
	pool := newDispatchPool(concurrency)
	defer pool.wait()
	for {
		wait := opts.Interval
		if err := s.watchPass(ctx, opts, pool, emit); err != nil {
			reset, throttled := ghapi.ThrottleWait(err)
			if !throttled {
				return err
			}
			// A one-shot run is somebody's cron or CI job, and it does not get to
			// sleep out a reset that may be an hour away. Report the throttle:
			// exiting 0 after checking part of the fleet — or none of it — makes an
			// incomplete scan look like a clean one, which is the same lie the
			// per-PR failure path above already refuses to tell.
			if opts.Once {
				return err
			}
			// Retrying on the ordinary interval hammers an exhausted quota and
			// pushes the reset further out, which is the opposite of waiting it
			// out. Sleep for what the API said.
			if reset > wait {
				wait = reset
			}
			if s.log != nil {
				s.log.Printf("watch: %v; waiting %s for the reset", err, wait.Round(time.Second))
			}
		}
		if opts.Once {
			return nil
		}
		if err := ghapi.SleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

func (s *Service) watchPass(ctx context.Context, opts WatchOptions, pool *dispatchPool, emit func(WatchEvent) error) error {
	var failures []string
	repos := opts.Repos
	if len(repos) == 0 {
		for repo := range s.cfg.AllowRepos {
			repos = append(repos, repo)
		}
		sort.Strings(repos) // stable order: a pass must not depend on map iteration
	}
	if len(repos) == 0 {
		return errors.New("nothing to watch: pass a repository, or set CRQ_REPOS")
	}
	// Gather every candidate first, then start from a different one each pass.
	//
	// A fixed order starves the tail: with three dispatch slots and four PRs
	// needing fixes, the same three take the slots every pass and the fourth is
	// told "at dispatch capacity" forever. One PR sat five hours that way while
	// its findings grew from 15 to 25. This is the same fix the quota-free
	// rescue scan already needed, for the same reason.
	type candidate struct {
		repo string
		pull ghapi.Pull
	}
	var candidates []candidate
	gate := NormalizeRepo(s.cfg.GateRepo)
	for _, repo := range repos {
		// The calibration PR is deliberately kept open to probe account quota; it
		// is not work, and `Next` would enqueue a real review for it — or dispatch
		// a session against CALIBRATION.md. autoReviewPass excludes the gate
		// repository for the same reason.
		if gate != "" && NormalizeRepo(repo) == gate {
			continue
		}
		pulls, err := s.gh.ListPulls(ctx, repo, openPullQuery())
		if err != nil {
			return err
		}
		for _, pull := range pulls {
			candidates = append(candidates, candidate{repo, pull})
		}
	}
	if len(candidates) > 0 {
		s.watchOffset = (s.watchOffset + 1) % len(candidates)
	}
	for i := range candidates {
		c := candidates[(i+s.watchOffset)%len(candidates)]
		repo, pull := c.repo, c.pull
		{
			if err := ctx.Err(); err != nil {
				return err
			}
			// The fleet's skip marker suppresses review deliberately, to protect
			// the shared quota. Next is a MUTATING oracle — it enqueues and can
			// fire — so the marker has to be honoured before calling it, not
			// after.
			if marker := strings.TrimSpace(s.cfg.SkipMarker); marker != "" && strings.Contains(pull.Body, marker) {
				continue
			}
			report, err := s.Next(ctx, repo, pull.Number)
			if err != nil {
				if _, ok := ghapi.ThrottleWait(err); ok {
					return err
				}
				if s.log != nil {
					s.log.Printf("watch: %s#%d: %v", repo, pull.Number, err)
				}
				// A one-shot run is somebody's cron or CI job: reporting success
				// after skipping a PR it could not read makes a broken scan look
				// like a clean one.
				if opts.Once {
					failures = append(failures, fmt.Sprintf("%s#%d: %v", repo, pull.Number, err))
				}
				continue
			}
			event := WatchEvent{
				Repo: repo, PR: pull.Number,
				Action: report.Action, Reason: report.Reason,
				Findings: len(report.Findings), At: s.clock().UTC(),
			}
			if opts.Dispatch && report.Action == string(engine.ActionFix) {
				// Claimed here, run in the pool: the pass moves on to the next PR
				// while this session runs.
				event.Dispatched, event.Skipped = s.startDispatch(ctx, opts, pool, report)
			}
			if emit != nil {
				// A consumer that has gone away (a closed pipe, a full
				// destination) means nothing is observing a watcher that is still
				// firing reviews and starting sessions. Stop instead.
				if err := emit(event); err != nil {
					return fmt.Errorf("emitting %s#%d: %w", repo, pull.Number, err)
				}
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d pull request(s) could not be checked: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// noteDispatchHealth records whether fix sessions are starting, and says so
// loudly the first time it is clear they are not.
//
// The failure this exists for looked exactly like success from the outside: the
// watcher ran, the queue moved, PRs reported findings — and every dispatch died
// on a wedged git mirror, in a log line nobody was reading.
func (s *Service) noteDispatchHealth(ctx context.Context, started bool, reason string) {
	var flipped, unhealthy bool
	state, err := s.store.Update(ctx, func(st *State) error {
		was := st.Drain.Unhealthy()
		st.NoteDispatch(s.cfg.Host, started, reason, s.clock())
		unhealthy = st.Drain.Unhealthy()
		flipped = unhealthy != was
		return nil
	})
	if err != nil {
		return
	}
	// The dashboard is where this alert is meant to be read, and a drain that has
	// stopped working may produce no other write for hours. Only on the flip: the
	// rendered dashboard does not change for the attempts in between.
	if flipped {
		s.sync(ctx, state)
	}
	if flipped && unhealthy && s.log != nil {
		s.log.Printf("ALERT: no fix session has started in %d dispatch attempts — %s", DrainUnhealthyAfter, reason)
	}
}

// startDispatch claims the round and hands the session to the pool.
//
// The claim is taken HERE, in the pass, rather than inside the session: an event
// saying `dispatched` is read as "somebody is handling this PR", so a round
// another watcher holds, or one that has spent its attempts, has to come back as
// skipped rather than as work in progress.
func (s *Service) startDispatch(ctx context.Context, opts WatchOptions, pool *dispatchPool, report NextReport) (bool, string) {
	// DryRun means crq writes nothing and posts nothing. Claiming shared state,
	// running a code-writing command, and recording dispatch health are all
	// writes, so this is checked before any of them.
	if s.cfg.DryRun {
		return false, "dry run: would dispatch a fix session"
	}
	if ok, why := pool.acquire(); !ok {
		return false, why
	}
	token := randomToken()
	claimed, why, byDesign := s.claimDispatch(ctx, report, token, opts.MaxAttempts)
	if !claimed {
		pool.release()
		// A round another watcher already holds, or one that has spent its
		// per-head attempts, is the bound doing its job — not this dispatcher
		// failing. Counting it would raise "fix sessions are not starting" after
		// three passes over a watcher that is obeying its own configuration, and
		// an exhausted head refuses again on every pass forever.
		if !byDesign {
			s.noteDispatchHealth(ctx, false, why)
			if s.log != nil {
				s.log.Printf("watch: %s#%d not fixed: %s", report.Repo, report.PR, why)
			}
		}
		return false, why
	}
	pool.run(func() { s.runDispatch(ctx, opts, report, token) })
	return true, ""
}

// runDispatch runs one claimed dispatch and records whether a session started. It
// runs in the pool, off the pass, so a long session delays nothing else.
func (s *Service) runDispatch(ctx context.Context, opts WatchOptions, report NextReport, token string) {
	ok, why := s.dispatch(ctx, opts, report, token)
	s.noteDispatchHealth(ctx, ok, why)
	if !ok && s.log != nil {
		s.log.Printf("watch: %s#%d not fixed: %s", report.Repo, report.PR, why)
	}
}

// dispatch checks the claimed round's head out and runs the fix session.
func (s *Service) dispatch(ctx context.Context, opts WatchOptions, report NextReport, token string) (bool, string) {
	// Losing the claim means another watcher has taken this round and may be
	// running its own session. Two sessions writing one worktree is worse than
	// no session, so the heartbeat cancels this one.
	//
	// It starts BEFORE the checkout, not after: a first clone of a large
	// repository can outlast DispatchTTL, and a claim nobody is refreshing reads
	// as abandoned — so the takeover this exists to prevent would happen while
	// this dispatch was still fetching.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := s.beatDispatch(runCtx, report, token, cancel)

	co, err := s.workspace(runCtx).Checkout(runCtx, report.Repo, report.PR, report.Head)
	if err != nil {
		// Nothing ran, so the attempt did not happen: a transient clone failure
		// must not eat the per-head budget and permanently skip the PR.
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		return false, "checkout failed: " + err.Error()
	}

	// The findings go OUTSIDE the worktree. At the repository root they are an
	// untracked file, and a session following the documented `git add -A` push
	// would commit crq's review payload into the PR.
	findingsPath, err := s.writeFindings(report)
	if err != nil {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, err.Error()
	}
	defer os.Remove(findingsPath)

	// A session's output went nowhere, so when one failed there was nothing to
	// read but the fact that nothing happened. Every session gets a file, and
	// the path is logged before it starts so it is findable while it runs.
	logPath, logFile, err := s.sessionLog(ctx, report)
	if err != nil {
		s.releaseDispatch(context.WithoutCancel(ctx), report, token, false)
		_ = co.Remove(context.WithoutCancel(ctx))
		return false, "could not open a session log: " + err.Error()
	}
	defer logFile.Close()

	cmd := exec.CommandContext(runCtx, opts.Command[0], opts.Command[1:]...)
	cmd.Dir = co.Dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(),
		"CRQ_DISPATCH_REPO="+report.Repo,
		fmt.Sprintf("CRQ_DISPATCH_PR=%d", report.PR),
		"CRQ_DISPATCH_HEAD="+report.Head,
		"CRQ_DISPATCH_FINDINGS="+findingsPath,
	)
	if s.log != nil {
		s.log.Printf("watch: dispatching %s for %s#%d@%s (%d findings) — log: %s",
			opts.Command[0], report.Repo, report.PR, report.Head, len(report.Findings), logPath)
	}
	runErr := cmd.Run()
	// A command that never reached a process did not use up the per-head budget.
	// A missing or non-executable fix agent would otherwise spend every attempt
	// on a session that never ran, and correcting the configuration would come
	// too late for that head — the case releaseDispatch's contract is about.
	s.releaseDispatch(context.WithoutCancel(ctx), report, token, commandStarted(runErr))
	if lost() {
		return false, "another watcher took this round; the session was stopped"
	}
	if runErr != nil {
		// Keep the worktree AND name the log: a failed session is the one whose
		// state somebody needs to look at.
		return false, fmt.Sprintf("fix session failed: %v (log: %s)", runErr, logPath)
	}

	// Keep a worktree the session left work in. Removing it discards fixes that
	// were made but not pushed, which is the one outcome a fix session must
	// never suffer.
	if kept, why := sessionWork(context.WithoutCancel(ctx), co, report.Head); kept {
		if s.log != nil {
			s.log.Printf("watch: keeping %s — %s", co.Dir, why)
		}
		return true, ""
	}
	_ = co.Remove(context.WithoutCancel(ctx))
	return true, ""
}

// commandStarted reports whether a failed run got as far as a process. A command
// that could not be found or could not be executed fails before that, as an
// *exec.Error or a fork/exec *fs.PathError; a process that ran and exited
// unsuccessfully gives an *exec.ExitError, and that attempt did happen.
func commandStarted(err error) bool {
	if err == nil {
		return true
	}
	var noCommand *exec.Error
	var noFile *fs.PathError
	return !errors.As(err, &noCommand) && !errors.As(err, &noFile)
}

// sessionWork reports whether this checkout holds work that exists nowhere else,
// and says what it is.
//
// A clean working tree is not proof that the session's fixes landed: it also
// commits, and a commit it did not push lives only here. So anything that cannot
// be established — an unreadable tree, an unconfirmable push — counts as work
// worth keeping. The worktree is pruned by age either way; a lost fix is not
// recoverable at all.
func sessionWork(ctx context.Context, co Checkout, head string) (bool, string) {
	dirty, err := co.Git(ctx, "status", "--porcelain")
	if err != nil {
		return true, "its working tree could not be read"
	}
	if strings.TrimSpace(dirty) != "" {
		return true, "the session left uncommitted work"
	}
	local, err := co.Git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return true, "its HEAD could not be read"
	}
	if head == "" || strings.HasPrefix(local, head) {
		return false, "" // still on the reviewed head: nothing was committed here
	}
	// The session committed. Ask the remote whether that commit arrived, rather
	// than assuming a session that exited 0 pushed: fetch, then look for a remote
	// branch containing it.
	if _, err := co.Git(ctx, "fetch", "origin"); err != nil {
		return true, "the session committed, and the push could not be confirmed"
	}
	if on, err := co.Git(ctx, "branch", "--remotes", "--contains", local); err != nil || strings.TrimSpace(on) == "" {
		return true, "the session committed work that is on no remote branch"
	}
	return false, ""
}

// writeFindings puts the findings somewhere the fix session can read and the
// repository cannot accidentally commit.
func (s *Service) writeFindings(report NextReport) (string, error) {
	body, err := json.MarshalIndent(report.Findings, "", "  ")
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", fmt.Sprintf("crq-findings-%d-*.json", report.PR))
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		return "", err
	}
	return file.Name(), nil
}

// claimDispatch claims this PR's round for the session about to run.
//
// The third return says a refusal is the queue working as designed — a session
// already running for this PR, or a head that has spent its attempt budget —
// rather than evidence about whether fix sessions can start at all.
func (s *Service) claimDispatch(ctx context.Context, report NextReport, token string, maxAttempts int) (bool, string, bool) {
	reason, byDesign := "", false
	_, err := s.store.Update(ctx, func(st *State) error {
		round := st.Round(report.Repo, report.PR)
		if round != nil && round.Head != report.Head && round.DispatchHeld(s.clock()) {
			// Somebody is fixing an earlier head of this PR right now, and is
			// entitled to finish: a session's own push moves the head, so this is
			// what a successful session looks like from the outside. Superseding
			// its round here would start a second session against the work it is
			// still landing.
			reason, byDesign = "another watcher is already fixing this pull request", true
			return ErrNoChange
		}
		if round == nil || round.Head != report.Head {
			// Findings on a head the queue is not tracking: a review somebody
			// triggered by hand, feedback that predates the drain, or a head that
			// moved on while the previous round still stood. `Next` returns `fix`
			// before Enqueue in that case — deliberately, so no second review is
			// bought for a head whose findings are already in hand — so nothing
			// else supersedes the stale round either, and every pass took the same
			// path and refused these dispatches forever.
			var err error
			if round == nil {
				round, err = st.NewRound(report.Repo, report.PR, report.Head, s.clock())
			} else {
				round, err = st.Supersede(report.Repo, report.PR, report.Head, s.clock())
			}
			if err != nil {
				return err
			}
			// Record the head as reviewed only when these findings are about it —
			// then it demonstrably was. That is the "this head was reviewed"
			// marker, so the round is NOT fire-eligible and no review is bought
			// here either. Feedback CARRIED from an older commit proves nothing
			// about this head: marking it reviewed would leave a completed round
			// no reviewer ever looked at, which dedups the review away while the
			// caller waits for one that can no longer be requested.
			if len(engine.FindingsOnHead(report.Findings, report.Head)) > 0 {
				if err := round.Dedupe(s.clock()); err != nil {
					return err
				}
				round.Note = "reviewed outside the queue; adopted to fix its findings"
			} else {
				round.Note = "adopted to fix feedback carried from an earlier head"
			}
		}
		ok, why := round.ClaimDispatch(s.cfg.Host, token, s.clock(), maxAttempts)
		if !ok {
			reason, byDesign = why, true
			return ErrNoChange
		}
		st.PutRound(*round)
		return nil
	})
	if err != nil {
		return false, err.Error(), false
	}
	return reason == "", reason, byDesign
}

// releaseDispatch frees the round. attempted=false also gives the attempt back,
// for a claim that never reached a running session — a clone that failed or a
// command that could not start did not use up the per-head budget.
func (s *Service) releaseDispatch(ctx context.Context, report NextReport, token string, attempted bool) {
	_, _ = s.store.Update(ctx, func(st *State) error {
		round := st.Round(report.Repo, report.PR)
		if round == nil || !round.ReleaseDispatch(token) {
			return ErrNoChange
		}
		if !attempted && round.Dispatch != nil && round.Dispatch.Attempts > 0 {
			round.Dispatch.Attempts--
		}
		st.PutRound(*round)
		return nil
	})
}

// beatDispatch refreshes the claim while the session runs, so a session that
// outlives the TTL keeps its round and a crashed watcher's does not.
// beatDispatch refreshes the claim while the session runs and reports whether it
// was lost. Losing it means another watcher took the round over, so stop() is
// called to end this session rather than let two write one worktree.
func (s *Service) beatDispatch(ctx context.Context, report NextReport, token string, stop func()) func() bool {
	var lost atomic.Bool
	go func() {
		ticker := time.NewTicker(DispatchTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				taken, superseded := false, false
				if _, err := s.store.Update(ctx, func(st *State) error {
					round := st.Round(report.Repo, report.PR)
					// A round for ANOTHER head is not this round: superseding is
					// what this session's own push does, and the fresh round's
					// claim belongs to whoever takes the new head — reading it as
					// a theft would kill this session between pushing and
					// resolving, every time it succeeded.
					if round == nil || round.Head != report.Head {
						superseded = true
						return ErrNoChange
					}
					ok, byOther := round.HeartbeatDispatch(token, s.clock())
					taken = byOther
					if !ok {
						return ErrNoChange
					}
					st.PutRound(*round)
					return nil
				}); err != nil {
					// A failed write is not proof of anything; the next tick
					// decides.
					continue
				}
				if taken {
					// Somebody else is running a session for this round. Two in
					// one worktree is worse than none.
					lost.Store(true)
					stop()
					return
				}
				// The claim is simply gone, or this head is no longer the one being
				// worked on. Stop heartbeating and let the session finish —
				// resolving its threads comes AFTER the push.
				if superseded {
					return
				}
			}
		}
	}()
	return lost.Load
}

func openPullQuery() url.Values {
	q := url.Values{}
	q.Set("state", "open")
	return q
}

// dispatchPool bounds how many fix sessions run at once.
//
// Non-blocking on purpose: when every slot is busy the PR is left for the next
// pass rather than stalling the decision loop behind a session. Queuing here
// would recreate the problem it exists to solve.
type dispatchPool struct {
	slots chan struct{}
	wg    sync.WaitGroup
}

// newDispatchPool bounds concurrent sessions. size <= 0 means no bound, which is
// the default: this is a resource valve, not a queue.
func newDispatchPool(size int) *dispatchPool {
	if size <= 0 {
		return &dispatchPool{}
	}
	return &dispatchPool{slots: make(chan struct{}, size)}
}

// acquire takes a slot, reporting why not when every one is busy. It is separate
// from run so the caller can claim the round — and give the slot back if the
// claim fails — before anything is said to have been dispatched.
func (p *dispatchPool) acquire() (bool, string) {
	if p.slots == nil {
		return true, ""
	}
	select {
	case p.slots <- struct{}{}:
		return true, ""
	default:
		// Only reachable when an operator has set a cap. Unfixed findings
		// waiting on a slot is the shape of problem this whole command
		// exists to remove, so say so rather than logging it as routine.
		return false, "at the configured dispatch cap (CRQ_DISPATCH_CONCURRENCY); this PR waits"
	}
}

// release gives an acquired slot back without running anything.
func (p *dispatchPool) release() {
	if p.slots != nil {
		<-p.slots
	}
}

// run runs fn in an already-acquired slot.
func (p *dispatchPool) run(fn func()) {
	p.wg.Add(1)
	go func() {
		defer func() {
			p.release()
			p.wg.Done()
		}()
		fn()
	}()
}

// start acquires a slot and runs fn in it, reporting why not when every slot is
// busy.
func (p *dispatchPool) start(fn func()) (bool, string) {
	if ok, why := p.acquire(); !ok {
		return false, why
	}
	p.run(fn)
	return true, ""
}

// wait blocks until every running session has finished, so a --once run does not
// return while its sessions are still writing.
func (p *dispatchPool) wait() { p.wg.Wait() }

// sessionLog opens the file a fix session's output goes to, and prunes the ones
// nobody is going to read.
func (s *Service) sessionLog(ctx context.Context, report NextReport) (string, *os.File, error) {
	root, err := s.workspace(ctx).root()
	if err != nil {
		return "", nil, err
	}
	owner, name, ok := splitRepo(report.Repo)
	if !ok {
		return "", nil, fmt.Errorf("repo must be owner/name, got %q", report.Repo)
	}
	dir := filepath.Join(root, "logs", owner, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	pruneSessionLogs(dir, report.PR)
	path := filepath.Join(dir, fmt.Sprintf("%d-%s-%s.log",
		report.PR, shortSHA(report.Head), s.clock().UTC().Format("20060102T150405")))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, err
	}
	return path, file, nil
}

// pruneSessionLogs keeps the most recent few logs per PR. Older ones describe a
// head nobody is working on any more.
//
// It runs BEFORE the new log is created, so it leaves room for it: keeping the
// full bound here would settle at one more file per PR than the bound says.
func pruneSessionLogs(dir string, pr int) {
	const keep = 5
	room := keep - 1
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := fmt.Sprintf("%d-", pr)
	var mine []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".log") {
			mine = append(mine, e.Name())
		}
	}
	if len(mine) <= room {
		return
	}
	sort.Strings(mine) // the timestamp is in the name, so this is chronological
	for _, name := range mine[:len(mine)-room] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}
