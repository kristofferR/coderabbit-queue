package crq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Concurrency caps how many fix sessions run at once. 0 means no cap, which
	// is the default: fixing findings spends no CodeRabbit quota, so it has no
	// reason to queue. It exists only as a resource valve for a machine that
	// cannot take the load.
	Concurrency int
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
	pool := newDispatchPool(opts.Concurrency)
	defer pool.wait()
	if opts.Interval <= 0 {
		opts.Interval = s.cfg.WatchInterval
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = s.cfg.DispatchMaxAttempts
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = s.cfg.DispatchConcurrency
	}
	if opts.Dispatch && len(opts.Command) == 0 {
		opts.Command = s.cfg.DispatchCommand
	}
	if opts.Dispatch && len(opts.Command) == 0 {
		return errors.New("dispatch needs a command: set CRQ_DISPATCH_CMD, or pass one after --")
	}
	for {
		wait := opts.Interval
		if err := s.watchPass(ctx, opts, pool, emit); err != nil {
			reset, throttled := ghapi.ThrottleWait(err)
			if !throttled {
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
	for _, repo := range repos {
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
				// Started, not finished: the pass moves on to the next PR while
				// this session runs.
				event.Dispatched, event.Skipped = pool.start(func() {
					s.runDispatch(ctx, opts, report)
				})
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
	var unhealthy bool
	if _, err := s.store.Update(ctx, func(st *State) error {
		was := st.Drain.Unhealthy()
		st.NoteDispatch(s.cfg.Host, started, reason, s.clock())
		unhealthy = st.Drain.Unhealthy() && !was
		return nil
	}); err != nil {
		return
	}
	if unhealthy && s.log != nil {
		s.log.Printf("ALERT: no fix session has started in %d passes — %s", DrainUnhealthyAfter, reason)
	}
}

// runDispatch performs one dispatch and records whether a session started. It
// runs in the pool, off the pass, so a long session delays nothing else.
func (s *Service) runDispatch(ctx context.Context, opts WatchOptions, report NextReport) {
	ok, why := s.dispatch(ctx, opts, report)
	// A round another watcher already holds is not this dispatcher failing.
	if !ok && strings.Contains(why, "already fixing") {
		return
	}
	s.noteDispatchHealth(ctx, ok, why)
	if !ok && s.log != nil {
		s.log.Printf("watch: %s#%d not fixed: %s", report.Repo, report.PR, why)
	}
}

// dispatch claims the round, checks the head out, and runs the fix session.
func (s *Service) dispatch(ctx context.Context, opts WatchOptions, report NextReport) (bool, string) {
	// DryRun means crq writes nothing and posts nothing. Claiming shared state
	// and running a code-writing command is the largest possible violation of
	// that, so it is checked before anything else.
	if s.cfg.DryRun {
		return false, "dry run: would dispatch a fix session"
	}
	token := randomToken()
	claimed, why := s.claimDispatch(ctx, report, token, opts.MaxAttempts)
	if !claimed {
		return false, why
	}

	co, err := s.workspace(ctx).Checkout(ctx, report.Repo, report.PR, report.Head)
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

	// Losing the claim means another watcher has taken this round and may be
	// running its own session. Two sessions writing one worktree is worse than
	// no session, so the heartbeat cancels this one.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := s.beatDispatch(runCtx, report, token, cancel)

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
	s.releaseDispatch(context.WithoutCancel(ctx), report, token, true)
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
	if dirty, err := co.Git(context.WithoutCancel(ctx), "status", "--porcelain"); err == nil && strings.TrimSpace(dirty) != "" {
		if s.log != nil {
			s.log.Printf("watch: keeping %s — the session left uncommitted work", co.Dir)
		}
		return true, ""
	}
	_ = co.Remove(context.WithoutCancel(ctx))
	return true, ""
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

func (s *Service) claimDispatch(ctx context.Context, report NextReport, token string, maxAttempts int) (bool, string) {
	reason := ""
	_, err := s.store.Update(ctx, func(st *State) error {
		round := st.Round(report.Repo, report.PR)
		if round == nil || round.Head != report.Head {
			reason = "no round for this head"
			return ErrNoChange
		}
		ok, why := round.ClaimDispatch(s.cfg.Host, token, s.clock(), maxAttempts)
		if !ok {
			reason = why
			return ErrNoChange
		}
		st.PutRound(*round)
		return nil
	})
	if err != nil {
		return false, err.Error()
	}
	return reason == "", reason
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
				taken := false
				if _, err := s.store.Update(ctx, func(st *State) error {
					round := st.Round(report.Repo, report.PR)
					if round == nil {
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
				// The claim is simply gone: the round was superseded, which is
				// what this session's own push does. Stop heartbeating and let
				// it finish — resolving its threads comes AFTER the push.
				if !taken && ctx.Err() == nil {
					continue
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

// start runs fn in the pool. It reports whether a session was started, and why
// not when it was not.
func (p *dispatchPool) start(fn func()) (bool, string) {
	if p.slots != nil {
		select {
		case p.slots <- struct{}{}:
		default:
			// Only reachable when an operator has set a cap. Unfixed findings
			// waiting on a slot is the shape of problem this whole command
			// exists to remove, so say so rather than logging it as routine.
			return false, "at the configured dispatch cap (CRQ_DISPATCH_CONCURRENCY); this PR waits"
		}
	}
	p.wg.Add(1)
	go func() {
		defer func() {
			if p.slots != nil {
				<-p.slots
			}
			p.wg.Done()
		}()
		fn()
	}()
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
func pruneSessionLogs(dir string, pr int) {
	const keep = 5
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
	if len(mine) <= keep {
		return
	}
	sort.Strings(mine) // the timestamp is in the name, so this is chronological
	for _, name := range mine[:len(mine)-keep] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}
