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
	// Dismissed counts the findings this head's round has accounted for through
	// `crq dismiss`. They are withheld from Findings, so the count is how a
	// caller sees that something was set aside rather than never reported.
	Dismissed int `json:"dismissed,omitempty"`

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

	// Observe BEFORE publishing anything. Enqueue makes this head fire-eligible
	// in shared state, and an autoreview daemon pumping concurrently can claim
	// and fire it in the gap — CAS protects each individual write, not this
	// sequence. Deciding first means the drain-first rule is enforced before the
	// round is exposed at all, rather than only in this process's own pump.
	report, action, feedback, err := s.nextFromState(ctx, repo, pr)
	if err != nil {
		return report, err
	}
	// Convergence can release the head before the optional primary acknowledges
	// the metered command. Preserve that slot before telling the caller to push:
	// the push supersedes this round, so leaving the hold attached only to the
	// live round would let Normalize release it while the review is still in
	// flight.
	if action.Kind == engine.ActionPush && feedback.PrimaryAckPending {
		if err := s.completeWaitRound(ctx, repo, pr, report.Head, true, &feedback.config); err != nil {
			return report, err
		}
	}

	// Undrained feedback for THIS head: publish nothing. Another review of the
	// same head would spend account quota to be told what the caller is already
	// holding.
	//
	// The gate is deliberately narrow. Findings CARRIED from an older commit —
	// an unresolved thread the caller may well have already fixed — must not
	// stop a new head from being reviewed, or an unresolvable one deadlocks the
	// PR forever and no review is ever requested for the code that replaced it.
	if len(engine.FindingsOnHead(action.Findings, report.Head)) > 0 {
		return report, nil
	}

	// The caller is mid-flight: it is about to move the head, or it is holding
	// fixes for a finding it was just handed. Queueing a review now spends a
	// window on code that is being replaced, and the round is superseded moments
	// later anyway. A carried thread reaches here with FindingsOnHead empty, so
	// this guard — not that one — is what stops a review firing ahead of the
	// fixes for it.
	if action.Kind == engine.ActionPush || (action.Kind == engine.ActionFix && report.LocalWork) {
		return report, nil
	}

	// A dry run reports decisions and writes nothing. Enqueue is a CAS write
	// plus a dashboard sync, so it has to be skipped here rather than left to
	// the dry-run-aware apply path further down.
	if s.cfg.DryRun {
		return report, nil
	}

	// Nothing left to drain, so record the round for this head (idempotent;
	// supersedes on a new head) and advance the queue one step.
	enqueued, err := s.Enqueue(ctx, repo, pr)
	if err != nil {
		return report, err
	}
	// Enqueue re-reads the head. If it moved in between, every conclusion above
	// describes a head that is no longer current — and returning `done` for it
	// would stop a caller just as an unreviewed head was queued. Say so and let
	// the next call decide on one snapshot.
	if enqueued.Head != "" && report.Head != "" && enqueued.Head != report.Head {
		report.Head = enqueued.Head
		report.Action = string(engine.ActionWait)
		report.Reason = "the head moved while deciding; re-reading it"
		report.Findings = []dialect.Finding{}
		// Dismissals are head-scoped, so the count read for the old head says
		// nothing about this one — reporting it beside the new head would credit
		// the new head with decisions never made about it.
		report.Dismissed = 0
		at := s.clock().Add(s.waitTick()).UTC()
		report.RecheckAfter = &at
		return report, nil
	}
	startedCoReview, err := s.advance(ctx, repo, pr, feedback)
	if err != nil {
		// Throttling is expected and self-clearing: the instruction above still
		// stands on the observation already taken, and the next call advances.
		// Anything else is a real failure — a token that cannot post the review
		// command, say — and returning a cheerful `wait` would have callers
		// retrying forever against something no amount of waiting fixes.
		if _, throttled := ghapi.ThrottleWait(err); !throttled {
			return report, err
		}
		if s.log != nil {
			s.log.Printf("throttled while advancing %s: %v", QueueKey(repo, pr), err)
		}
	}
	// A co-review just started answers in minutes, but the schedule above was
	// computed when nothing was in flight — for an account-blocked round that
	// means the quota window, hours away. Bring the caller back on the short
	// cadence instead of sleeping through the findings this call set in motion.
	if startedCoReview {
		at := s.clock().Add(s.waitTick()).UTC()
		if report.RecheckAfter == nil || report.RecheckAfter.After(at) {
			report.RecheckAfter = &at
		}
	}
	return report, nil
}

// settleUntil is when a convergence verdict may be trusted: the newest review
// plus the configured quiet period. Nil when settling is disabled or nothing has
// been observed to settle from.
func (s *Service) settleUntil(feedback FeedbackReport) *time.Time {
	if s.cfg.SettleWindow <= 0 || feedback.LastEvidenceAt.IsZero() {
		return nil
	}
	at := feedback.LastEvidenceAt.Add(s.cfg.SettleWindow).UTC()
	return &at
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
// It reports whether it resolved this round through the quota-free path, which
// is how the caller knows a co-review was just set in motion and the schedule
// computed before it is now too long.
func (s *Service) advance(ctx context.Context, repo string, pr int, feedback FeedbackReport) (bool, error) {
	if feedback.PrimaryUnavailable || feedback.CodeRabbitDeferred {
		if _, handled, err := s.advanceQuotaFree(ctx, repo, pr); err != nil {
			return false, err
		} else if handled {
			return true, nil
		}
	}
	_, err := s.Pump(ctx)
	return false, err
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

	report.LocalWork, report.LocalWorkReason = s.checkLocalWork(ctx,
		[]string{repo, feedback.HeadRepo}, report.Head, feedback.HeadRef)

	// Feedback has already withheld what this round dismissed — including from
	// its own convergence verdict — so there is one filter, not one per caller.
	report.Dismissed = feedback.Dismissed

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
		SettleUntil:   s.settleUntil(feedback),
	}
	// Only a round that still tracks THIS head may shape the verdict. A stale
	// fired/reviewing round carries its own phase and deadline, and an elapsed
	// one would report a terminal `blocked` for a head it never covered.
	if round != nil && round.Head == feedback.Head {
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

func (s *Service) checkLocalWork(ctx context.Context, repos []string, head, headRef string) (bool, string) {
	if s.localWorkFn != nil {
		return s.localWorkFn(ctx, head)
	}
	return localWork(ctx, repos, head, headRef)
}

// localWork reports whether the working copy holds changes the PR head does not
// have. It is the difference between "push your fixes" and "nothing left to do",
// so both mistakes are expensive: a false negative strands finished work behind
// a terminal `done`, and a false positive tells the caller to push something
// that is not this PR at all.
//
// The order matters. It first checks the checkout belongs to one of the PR's
// repositories — the base, or on a fork PR the head repository, which is the
// only remote a contributor's clone has. Then it establishes the checkout is on
// the PR's line of history BEFORE reading a dirty tree as this PR's work: an
// unrelated branch of the right repository is dirty for its own reasons, and
// pushing it would land somebody else's changes.
//
// Anything it cannot establish answers false with a reason, which errs toward
// `done` rather than `push`. That is the safe direction: `push` is only ever
// emitted once the head is already released, so a missed one costs one extra
// call, while a spurious `hold` would stall the loop.
func localWork(ctx context.Context, repos []string, head, headRef string) (bool, string) {
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
	if !ok {
		return false, "could not read this checkout's remotes"
	}
	matched := ""
	for _, candidate := range repos {
		if candidate != "" && remoteMatchesRepo(remotes, candidate) {
			matched = candidate
			break
		}
	}
	if matched == "" {
		return false, "this checkout has no remote for " + strings.Join(nonEmpty(repos), " or ")
	}
	local, ok := git("rev-parse", "HEAD")
	if !ok {
		return false, "could not read local HEAD"
	}

	// Sitting exactly on the PR head: only an uncommitted change is new work —
	// but the caller still has to be able to push it. A detached checkout at the
	// PR SHA has no branch, so the prescribed push fails and the next call, now
	// ahead of the head, reports no local work and can converge over the fix.
	if head == "" || strings.HasPrefix(local, head) {
		if why := branchMismatch(git, headRef); why != "" {
			return false, why
		}
		if status, ok := git("status", "--porcelain"); ok && status != "" {
			return true, "uncommitted changes in the working tree"
		}
		return false, ""
	}

	// Otherwise the checkout is somewhere else, and only ancestry can say
	// whether that somewhere is this PR's branch carrying unpushed commits, a
	// checkout merely behind it, or an unrelated branch entirely.
	if _, known := git("rev-parse", "--verify", "--quiet", head+"^{commit}"); !known {
		return false, "the pr head " + head + " is not in this checkout, so its relation to local HEAD is unknown"
	}
	if _, ahead := git("merge-base", "--is-ancestor", head, "HEAD"); !ahead {
		return false, "local HEAD " + shortSHA(local) + " is not on the pr's branch (behind it, or a different branch)"
	}
	// Descending from the PR head is not the same as being the PR branch: a
	// feature branch forked off it also descends, and reporting its commits as
	// this PR's work invites a push that updates something else entirely.
	if why := branchMismatch(git, headRef); why != "" {
		return false, why
	}
	return true, "local HEAD " + shortSHA(local) + " is ahead of the pr head " + head
}

// branchMismatch explains why this checkout is not the PR's branch, or "" when
// it is. A detached checkout counts as a mismatch: there is nothing to push to.
func branchMismatch(git func(...string) (string, bool), headRef string) string {
	if headRef == "" {
		return ""
	}
	branch, ok := git("rev-parse", "--abbrev-ref", "HEAD")
	if !ok || branch == "HEAD" {
		return "this checkout is detached, so there is no branch to push to " + headRef
	}
	if branch != headRef {
		return "checked out " + branch + ", not the pr branch " + headRef
	}
	return ""
}

// nonEmpty drops blanks so a reason never reads "owner/repo or ".
func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
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
