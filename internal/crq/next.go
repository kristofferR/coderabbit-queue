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

	report, action, feedback, err := s.nextFromState(ctx, repo, pr)
	if err != nil {
		return report, err
	}

	// One queue step, AFTER the decision. Pump owns DecideFire, whose gate list
	// has no "unresolved findings exist" step, so pumping first let this command
	// spend account quota on a round the caller had not drained yet — the very
	// rule `next` exists to enforce.
	//
	// The gate is deliberately narrow: only feedback for THIS head means a fire
	// would buy nothing. Findings CARRIED from an older commit — an unresolved
	// thread the caller may well have already fixed — must not stop a new head
	// from being reviewed, or an unresolvable one deadlocks the PR forever and
	// no review is ever requested for the code that replaced it. DecideFire
	// separately refuses a head it has already reviewed, so this only has to
	// answer "is the feedback I am holding about the head I would fire on".
	//
	// Not fatal if it fails: the instruction above stands on the observation
	// already taken, and the next call pumps again.
	if len(engine.FindingsOnHead(action.Findings, report.Head)) == 0 {
		if err := s.advance(ctx, repo, pr, feedback); err != nil && s.log != nil {
			s.log.Printf("warning: advancing %s: %v", QueueKey(repo, pr), err)
		}
	}
	return report, nil
}

// advance moves the queue one step on this caller's behalf.
//
// The account-wide FIFO and the fire slot exist to serialize exactly one thing:
// the primary reviewer's metered review. A round that will not spend that quota
// is not a queue citizen, and letting one wait its turn is how PRs ended up
// parked for hours behind blocked rounds whose quota they were never going to
// touch — a free-plan repo whose primary only ever posts a walkthrough, or a
// round degraded to its co-reviewers while the account is rate-limited.
//
// So this PR's own round gets resolved directly whenever its work is quota-free,
// and only otherwise does the global pump run. The two conditions come from the
// observation already taken, which is what keeps the bypass free: attempting it
// unconditionally would re-observe this PR on every call for a case that applies
// to a minority of rounds.
func (s *Service) advance(ctx context.Context, repo string, pr int, feedback FeedbackReport) error {
	if feedback.PrimaryUnavailable || feedback.CodeRabbitDeferred {
		if _, handled, err := s.advanceQuotaFree(ctx, repo, pr); err != nil {
			return err
		} else if handled {
			return nil
		}
	}
	_, err := s.Pump(ctx)
	return err
}

// nextFromState derives the instruction from what is already recorded and
// observable. It writes NOTHING — no enqueue, no pump, no dashboard sync — which
// is what lets `crq wait` re-evaluate as often as it likes without spending the
// account's write budget or firing reviews behind the caller's back.
//
// ONE observation drives the whole decision. Feedback already reads the pull, so
// head, open, per-bot evidence and findings all describe the same instant.
// Reading the head separately used to let a push land between the two and answer
// "done" for a head nobody had reviewed.
//
// Both `crq next` and `crq wait` decide here, through the same pure
// engine.NextAction, so the blocking and non-blocking forms cannot disagree.
func (s *Service) nextFromState(ctx context.Context, repo string, pr int) (NextReport, engine.Action, FeedbackReport, error) {
	report := NextReport{Repo: repo, PR: pr, Findings: []dialect.Finding{}, CheckedAt: s.clock()}

	feedback, err := s.Feedback(ctx, repo, pr)
	if err != nil {
		return report, engine.Action{}, feedback, err
	}
	report.Head = feedback.Head
	report.ReviewedBy = feedback.ReviewedBy

	st, _, err := s.store.Load(ctx)
	if err != nil {
		return report, engine.Action{}, feedback, err
	}
	now := s.clock()
	round := st.Round(repo, pr)

	report.LocalWork, report.LocalWorkReason = s.checkLocalWork(ctx, repo, report.Head)

	in := engine.NextInput{
		Obs:           engine.Observation{Head: feedback.Head, Open: feedback.Open},
		Completion:    engine.CompletionStatus{ReviewedBy: feedback.ReviewedBy, Done: allReviewed(feedback.ReviewedBy)},
		Findings:      feedback.Findings,
		Global:        s.global(st, now),
		Primary:       s.cfg.Bot,
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
	return report, action, feedback, nil
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
	if !ok || !remoteMatchesRepo(remotes, repo) {
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
	if head == "" || strings.HasPrefix(local, head) {
		return false, ""
	}
	// A local HEAD that differs from the PR head means one of two opposite
	// things: unpushed commits (real work), or a checkout that is simply behind
	// (nothing to push). Reporting work for the second produces a `push` the
	// caller cannot land — a rejected non-fast-forward, every round — so only an
	// ancestry check settles it, and anything unprovable answers false.
	if _, known := git("rev-parse", "--verify", "--quiet", head+"^{commit}"); !known {
		return false, "the pr head " + head + " is not in this checkout, so ahead and behind are indistinguishable"
	}
	if _, ahead := git("merge-base", "--is-ancestor", head, "HEAD"); !ahead {
		return false, "local HEAD " + shortSHA(local) + " is behind the pr head " + head
	}
	return true, "local HEAD " + shortSHA(local) + " is ahead of the pr head " + head
}

// remoteMatchesRepo reports whether any configured remote points at repo.
//
// It compares the owner/name slug exactly. Substring-matching the raw
// `git remote -v` output made "owner/app" match a checkout of
// "owner/application", which then had its unrelated HEAD read as unlanded work.
func remoteMatchesRepo(remotes, repo string) bool {
	want := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(repo), ".git"))
	if want == "" {
		return false
	}
	for _, line := range strings.Split(remotes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if repoSlugFromRemote(fields[1]) == want {
			return true
		}
	}
	return false
}

// repoSlugFromRemote reduces a git remote URL to its lowercase "owner/name",
// covering https, ssh:// and scp-style forms — including the host aliases
// (git@github.com-work:owner/name.git) a multi-account setup produces.
func repoSlugFromRemote(remote string) string {
	url := strings.ToLower(strings.TrimSpace(remote))
	url = strings.TrimSuffix(url, ".git")
	// scp-style separates the path with ":" rather than "/"; flattening both
	// lets one segment walk handle every form.
	url = strings.ReplaceAll(url, ":", "/")
	var segments []string
	for _, segment := range strings.Split(url, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) < 2 {
		return ""
	}
	return segments[len(segments)-2] + "/" + segments[len(segments)-1]
}

func shortSHA(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}
