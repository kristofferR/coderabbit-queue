package crq

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// NextReport is what `crq next` prints: one instruction, and the data needed to
// carry it out. The `action` field is the entire contract — a caller reads it,
// does exactly that, and calls again. Nothing else needs interpreting, which is
// why this command exits 0 for every action and reserves non-zero for hard
// failures alone.
type NextReport struct {
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Repo   string `json:"repo"`
	PR     int    `json:"pr"`
	Head   string `json:"head,omitempty"`

	// RecheckAfter is when to call `crq next` again — the ONE time field, set
	// for both hold and wait so there is never a question of which to read. crq
	// computes it; a caller must never invent a delay of its own.
	RecheckAfter *time.Time `json:"recheck_after,omitempty"`

	// Pending lists the required reviewers with no evidence for this head.
	Pending  []string          `json:"pending,omitempty"`
	Findings []dialect.Finding `json:"findings"`

	ReviewedBy map[string]bool `json:"reviewed_by,omitempty"`
	// LocalWork records whether crq saw changes the PR head does not have. It
	// is what separates "push" from "done"; when crq is not run inside the
	// repository it is false and LocalWorkReason says so.
	LocalWork       bool      `json:"local_work"`
	LocalWorkReason string    `json:"local_work_reason,omitempty"`
	CheckedAt       time.Time `json:"checked_at"`
}

// Next answers "what should I do about this PR right now?" in one
// non-blocking call.
//
// It replaces the agent-side protocol that `crq loop` documented but could not
// enforce: drain findings before starting a round, hold the head while a
// required reviewer is pending, pick a sensible delay. Each of those is now a
// value in the returned report rather than a judgement call at the call site.
//
// The call also advances the queue by one pump step, so a PR in a repository
// outside the autoreview fleet still progresses without a daemon — and because
// every write is CAS'd, running it alongside the daemon is safe. Being
// non-blocking is the point: there is no long-lived process for a harness to
// kill, and a caller that dies mid-loop simply calls again.
func (s *Service) Next(ctx context.Context, repo string, pr int) (NextReport, error) {
	repo = NormalizeRepo(repo)
	report := NextReport{Repo: repo, PR: pr, Findings: []dialect.Finding{}, CheckedAt: s.clock()}

	// Ensure a round exists for the current head (idempotent; supersedes on a
	// new head), so the pump below has something to advance.
	if _, err := s.Enqueue(ctx, repo, pr); err != nil {
		return report, err
	}

	feedback, err := s.Feedback(ctx, repo, pr)
	if err != nil {
		return report, err
	}
	report.Head = feedback.Head
	report.ReviewedBy = feedback.ReviewedBy

	// One queue step. Not fatal if it fails: the instruction below is derived
	// from the observation we already have, and the next call pumps again.
	if _, perr := s.Pump(ctx); perr != nil && s.log != nil {
		s.log.Printf("warning: pump during next for %s: %v", QueueKey(repo, pr), perr)
	}

	st, _, err := s.store.Load(ctx)
	if err != nil {
		return report, err
	}
	now := s.clock()
	round := st.Round(repo, pr)

	// NextAction needs only "is it open" and "what is the head" from the
	// observation — the findings and per-bot evidence come from Feedback above.
	// pullHead is the ETag-cached read for exactly that, so this call costs a
	// 304 instead of a second full observation on the shared REST quota.
	head, open, err := s.pullHead(ctx, repo, pr)
	if err != nil {
		return report, err
	}
	if head != "" {
		report.Head = head
	}

	report.LocalWork, report.LocalWorkReason = s.checkLocalWork(ctx, repo, report.Head)

	in := engine.NextInput{
		Obs:           engine.Observation{Head: head, Open: open},
		Completion:    engine.CompletionStatus{ReviewedBy: feedback.ReviewedBy, Done: allReviewed(feedback.ReviewedBy)},
		Findings:      feedback.Findings,
		Global:        s.global(st, now),
		LocalWork:     report.LocalWork,
		Deferred:      feedback.CodeRabbitDeferred,
		DeferredUntil: feedback.DeferredUntil,
		MinDelay:      s.cfg.PollInterval,
	}
	if round != nil {
		in.Round = *round
	}

	action := engine.NextAction(in, now)
	report.Action = string(action.Kind)
	report.Reason = action.Reason
	report.Pending = action.Pending
	if len(action.Findings) > 0 {
		report.Findings = action.Findings
	}
	if !action.At.IsZero() {
		at := action.At.UTC()
		report.RecheckAfter = &at
	}
	return report, nil
}

// NextWaiting is Next for an interactive caller: it sleeps through the states a
// caller cannot act on (wait, hold) and returns the first actionable
// instruction. It shares Next's code path exactly, so the blocking and
// non-blocking forms can never disagree about what should happen.
func (s *Service) NextWaiting(ctx context.Context, repo string, pr int) (NextReport, error) {
	for {
		report, err := s.Next(ctx, repo, pr)
		if err != nil {
			if wait, ok := ghapi.ThrottleWait(err); ok {
				if wait <= 0 {
					wait = s.cfg.PollInterval
				}
				if serr := ghapi.SleepCtx(ctx, wait); serr != nil {
					return report, serr
				}
				continue
			}
			return report, err
		}
		switch engine.ActionKind(report.Action) {
		case engine.ActionWait, engine.ActionHold:
			if report.RecheckAfter == nil {
				return report, nil
			}
			delay := report.RecheckAfter.Sub(s.clock())
			if delay <= 0 {
				delay = s.cfg.PollInterval
			}
			if s.log != nil {
				s.log.Printf("%s#%d %s — %s; rechecking at %s",
					repo, pr, report.Action, report.Reason, report.RecheckAfter.Format(time.RFC3339))
			}
			if serr := ghapi.SleepCtx(ctx, delay); serr != nil {
				return report, serr
			}
		default:
			return report, nil
		}
	}
}

func (s *Service) checkLocalWork(ctx context.Context, repo, head string) (bool, string) {
	if s.localWorkFn != nil {
		return s.localWorkFn(ctx, head)
	}
	return localWork(ctx, repo, head)
}

// localWork reports whether the working copy holds changes the PR head does not
// have — a dirty tree, or a local HEAD that is not the PR head. It is the
// difference between "push your fixes" and "nothing left to do".
//
// It first checks that this checkout actually belongs to the PR's repository:
// asking about owner/a from a checkout of owner/b would otherwise read that
// unrelated HEAD as unlanded work.
//
// Anything it cannot establish answers false with a reason, which errs toward
// `done` rather than `push`. That is the safe direction: `push` is only ever
// emitted once the head is already released, so a missed one costs one extra
// call, while a spurious `hold` would stall the loop.
func localWork(ctx context.Context, repo, head string) (bool, string) {
	git := func(args ...string) (string, bool) {
		out, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	if _, ok := git("rev-parse", "--is-inside-work-tree"); !ok {
		return false, "not run inside a git checkout"
	}
	remotes, ok := git("remote", "-v")
	if !ok || !strings.Contains(strings.ToLower(remotes), strings.ToLower(repo)) {
		// Match on any remote, not just origin, so a fork checkout whose upstream
		// is the PR's repository still counts.
		return false, "this checkout has no remote for " + repo
	}
	if status, ok := git("status", "--porcelain"); ok && status != "" {
		return true, "uncommitted changes in the working tree"
	}
	local, ok := git("rev-parse", "HEAD")
	if !ok {
		return false, "could not read local HEAD"
	}
	if head != "" && !strings.HasPrefix(local, head) {
		return true, "local HEAD " + shortSHA(local) + " is not the pr head " + head
	}
	return false, ""
}

func shortSHA(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}
