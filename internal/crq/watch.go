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
func (s *Service) Watch(ctx context.Context, opts WatchOptions, emit func(WatchEvent)) error {
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
	for {
		if err := s.watchPass(ctx, opts, emit); err != nil {
			if _, ok := ghapi.ThrottleWait(err); !ok {
				return err
			}
			if s.log != nil {
				s.log.Printf("watch: %v; waiting it out", err)
			}
		}
		if opts.Once {
			return nil
		}
		if err := ghapi.SleepCtx(ctx, opts.Interval); err != nil {
			return err
		}
	}
}

func (s *Service) watchPass(ctx context.Context, opts WatchOptions, emit func(WatchEvent)) error {
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
	for _, repo := range repos {
		pulls, err := s.gh.ListPulls(ctx, repo, openPullQuery())
		if err != nil {
			return err
		}
		for _, pull := range pulls {
			if err := ctx.Err(); err != nil {
				return err
			}
			report, err := s.Next(ctx, repo, pull.Number)
			if err != nil {
				if _, ok := ghapi.ThrottleWait(err); ok {
					return err
				}
				if s.log != nil {
					s.log.Printf("watch: %s#%d: %v", repo, pull.Number, err)
				}
				continue
			}
			event := WatchEvent{
				Repo: repo, PR: pull.Number,
				Action: report.Action, Reason: report.Reason,
				Findings: len(report.Findings), At: s.clock().UTC(),
			}
			if opts.Dispatch && report.Action == string(engine.ActionFix) {
				event.Dispatched, event.Skipped = s.dispatch(ctx, opts, report)
			}
			if emit != nil {
				emit(event)
			}
		}
	}
	return nil
}

// dispatch claims the round, checks the head out, and runs the fix session.
func (s *Service) dispatch(ctx context.Context, opts WatchOptions, report NextReport) (bool, string) {
	token := randomToken()
	claimed, why := s.claimDispatch(ctx, report, token, opts.MaxAttempts)
	if !claimed {
		return false, why
	}
	defer s.releaseDispatch(context.WithoutCancel(ctx), report, token)

	co, err := s.workspace().Checkout(ctx, report.Repo, report.PR, report.Head)
	if err != nil {
		return false, "checkout failed: " + err.Error()
	}
	defer func() { _ = co.Remove(context.WithoutCancel(ctx)) }()

	// The session gets the findings as a file rather than an argument: they are
	// long, and a shell would be the wrong place to carry them.
	findingsPath := filepath.Join(co.Dir, ".crq-findings.json")
	body, err := json.MarshalIndent(report.Findings, "", "  ")
	if err != nil {
		return false, err.Error()
	}
	if err := os.WriteFile(findingsPath, body, 0o600); err != nil {
		return false, err.Error()
	}

	stop := s.beatDispatch(ctx, report, token)
	defer stop()

	cmd := exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	cmd.Dir = co.Dir
	cmd.Env = append(os.Environ(),
		"CRQ_DISPATCH_REPO="+report.Repo,
		fmt.Sprintf("CRQ_DISPATCH_PR=%d", report.PR),
		"CRQ_DISPATCH_HEAD="+report.Head,
		"CRQ_DISPATCH_FINDINGS="+findingsPath,
	)
	if s.log != nil {
		s.log.Printf("watch: dispatching %s for %s#%d@%s (%d findings)",
			opts.Command[0], report.Repo, report.PR, report.Head, len(report.Findings))
	}
	if err := cmd.Run(); err != nil {
		return false, "fix session failed: " + err.Error()
	}
	return true, ""
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

func (s *Service) releaseDispatch(ctx context.Context, report NextReport, token string) {
	_, _ = s.store.Update(ctx, func(st *State) error {
		round := st.Round(report.Repo, report.PR)
		if round == nil || !round.ReleaseDispatch(token) {
			return ErrNoChange
		}
		st.PutRound(*round)
		return nil
	})
}

// beatDispatch refreshes the claim while the session runs, so a session that
// outlives the TTL keeps its round and a crashed watcher's does not.
func (s *Service) beatDispatch(ctx context.Context, report NextReport, token string) func() {
	beatCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(DispatchTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case <-ticker.C:
				_, _ = s.store.Update(beatCtx, func(st *State) error {
					round := st.Round(report.Repo, report.PR)
					if round == nil || !round.HeartbeatDispatch(token, s.clock()) {
						return ErrNoChange
					}
					st.PutRound(*round)
					return nil
				})
			}
		}
	}()
	return cancel
}

func (s *Service) workspace() Workspace { return Workspace{Root: s.cfg.WorkspaceRoot} }

func openPullQuery() url.Values {
	q := url.Values{}
	q.Set("state", "open")
	return q
}
