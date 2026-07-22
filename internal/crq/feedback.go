package crq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type FeedbackReport struct {
	Status     string            `json:"status"`
	Repo       string            `json:"repo"`
	PR         int               `json:"pr"`
	Head       string            `json:"head"`
	Reason     string            `json:"reason,omitempty"`
	Converged  bool              `json:"converged"`
	ReviewedBy map[string]bool   `json:"reviewed_by"`
	Findings   []dialect.Finding `json:"findings"`
	CheckedAt  time.Time         `json:"checked_at"`
	// CodeRabbitDeferred marks a round degraded to Codex-only while the
	// CodeRabbit account is rate-limited: Codex feedback is authoritative for
	// this round, the CodeRabbit review stays queued and fires after
	// DeferredUntil. Converged stays false until it does.
	CodeRabbitDeferred bool       `json:"coderabbit_deferred,omitempty"`
	DeferredUntil      *time.Time `json:"coderabbit_deferred_until,omitempty"`
}

func (s *Service) Feedback(ctx context.Context, repo string, pr int) (FeedbackReport, error) {
	repo = NormalizeRepo(repo)
	now := s.clock()
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FeedbackReport{}, err
	}
	round := st.Round(repo, pr)

	// One fetch drives both halves: observe() reads the pull, reviews and issue
	// comments (plus reactions when the round has fired). Feedback parses its
	// findings from the raw reviews/comments and derives convergence from
	// engine.Completion over the same snapshot — no second fetch path, and the
	// "is head reviewed?" rules live only in the engine.
	obs, err := s.observe(ctx, repo, pr, round, now)
	if err != nil {
		return FeedbackReport{}, err
	}
	pull := obs.pull
	head := ""
	if len(pull.Head.SHA) >= 9 {
		head = pull.Head.SHA[:9]
	}
	report := FeedbackReport{
		Status:     "feedback",
		Repo:       repo,
		PR:         pr,
		Head:       head,
		ReviewedBy: map[string]bool{},
		Findings:   []dialect.Finding{},
		CheckedAt:  now,
	}

	// The completion anchor is the current round only when it still tracks this
	// head. A stale or missing round yields a headless anchor (FiredAt nil), so
	// only head-matching reviews and SHA-bound Codex summaries can count — the
	// engine's rule set reproduces v2's "no wait context" behavior.
	completionRound := Round{Repo: repo, PR: pr, Head: head}
	if round != nil && round.Head == head {
		completionRound = *round
	}
	anchorOK := completionRound.FiredAt != nil
	anchorCutoff := time.Time{}
	if anchorOK {
		anchorCutoff = completionRound.FiredAt.UTC()
	}
	completion := engine.Completion(completionRound, obs.eng, s.policy())
	report.ReviewedBy = completion.ReviewedBy

	// extractBots is the broader set whose findings we surface — a superset that
	// includes Codex — so a bot that reviews without being required (and would
	// hang convergence if it were) still has its findings reported instead of
	// dropped. It always includes the required bots: a bot crq waits for whose
	// findings it didn't surface would hang the loop forever.
	extractBots := dialect.BotSet(unionBots(s.cfg.FeedbackBots, s.cfg.RequiredBots))

	// Review-body findings — CodeRabbit's detailed and "Prompt for AI agents"
	// blocks — carry no per-finding resolution state, only the review's commit.
	// When inline comments fail to post (GitHub 5xx / code-review limits) that
	// body block is the ONLY record of the findings, so gating extraction to the
	// current head silently drops an entire review the moment the head moves on
	// (a rebase, a squash-merge). Extract instead from each bot's LATEST review
	// regardless of its commit: a newer review from the same bot supersedes it,
	// and resolved/outdated inline threads still suppress individual prompt
	// duplicates below. Convergence (engine.Completion) stays gated to a review
	// whose commit matches the head, so the loop still waits for a real review.
	latestReview := map[string]ghapi.Review{}
	for _, review := range obs.reviews {
		login := review.User.Login
		if !dialect.InBots(extractBots, login) {
			continue
		}
		// Once a fresh review round has started for this head, a body submitted
		// before that round belongs to the previous head. Unresolved threads are
		// still surfaced below across commits, while thread-less body findings
		// must be re-reported by the current round instead of trapping the loop.
		if anchorOK && s.isConfiguredBot(login) &&
			(head == "" || !strings.HasPrefix(review.CommitID, head)) &&
			!notBefore(review.SubmittedAt, anchorCutoff) {
			continue
		}
		if cur, ok := latestReview[login]; !ok || reviewNewer(review, cur) {
			latestReview[login] = review
		}
	}
	for _, review := range obs.reviews {
		if lr, ok := latestReview[review.User.Login]; !ok || lr.ID != review.ID {
			continue
		}
		report.Findings = append(report.Findings, parseReviewBodyFindings(review, review.User.Login)...)
	}

	suppressPromptAt := map[string]bool{}
	if threads, err := s.reviewThreads(ctx, repo, pr); err == nil {
		for _, thread := range threads {
			report.Findings = append(report.Findings, threadFindings(thread, extractBots)...)
			// A resolved/outdated inline thread emits no finding, but CodeRabbit's
			// "Prompt for AI agents" block still lists the same location. Record it so
			// the prompt duplicate is suppressed too — otherwise an addressed finding
			// reappears as a thread-less prompt finding and the loop never converges.
			if thread.IsResolved || thread.IsOutdated {
				for _, key := range promptSuppressKeys(thread, extractBots) {
					suppressPromptAt[key] = true
				}
				// A resolved thread where the bot got the last word contesting the
				// agent's decline is NOT actually settled — surface the rebuttal so
				// the loop re-addresses it instead of silently dropping it.
				if rebuttal := threadRebuttal(thread, extractBots); rebuttal != nil {
					report.Findings = append(report.Findings, *rebuttal)
				}
			}
		}
	} else if ghapi.IsThrottled(err) {
		// A transient GraphQL throttle must not silently degrade to the REST
		// fallback, which loses thread resolution/outdated state and the cross-commit
		// unresolved findings this command promises. Surface it so Loop rides it out
		// instead of reporting converged from incomplete data.
		return report, err
	} else {
		comments, cerr := s.gh.ListReviewComments(ctx, repo, pr)
		if cerr != nil {
			return report, cerr
		}
		for _, comment := range comments {
			if !dialect.InBots(extractBots, comment.User.Login) {
				continue
			}
			commit := dialect.ShortOID(firstNonEmpty(comment.CommitID, comment.OriginalCommitID))
			if head != "" && commit != "" && commit != head {
				continue
			}
			report.Findings = append(report.Findings, dialect.Finding{
				Bot:       comment.User.Login,
				Severity:  dialect.SeverityOf(comment.Body),
				Path:      comment.Path,
				Line:      firstPositive(comment.Line, comment.OriginalLine),
				Title:     dialect.TitleOf(comment.Body),
				Body:      strings.TrimSpace(comment.Body),
				CommentID: comment.ID,
				ReviewID:  comment.PullRequestReviewID,
				Commit:    commit,
				URL:       comment.URL,
				Source:    "review_comment",
				CreatedAt: comment.CreatedAt,
			})
		}
	}

	// Top-level issue comments carry no commit SHA, so bound them to the current
	// head: a bot finding posted before this head was committed belongs to an earlier
	// round and must not trap crq loop on stale, already-addressed feedback. The
	// head commit time is resolved lazily — only when there's an actionable candidate.
	headCutoff := time.Time{}
	headCutoffLoaded := false
	headCutoffOf := func() time.Time {
		if !headCutoffLoaded {
			headCutoffLoaded = true
			if pull.Head.SHA != "" {
				if c, cerr := s.gh.GetCommit(ctx, repo, pull.Head.SHA); cerr == nil {
					headCutoff = c.Committer.Date
				}
			}
		}
		return headCutoff
	}
	for _, comment := range obs.comments {
		if !dialect.InBots(extractBots, comment.User.Login) {
			continue
		}
		if s.cr.IsRateLimited(comment.Body) {
			continue // an account-quota notice is never a finding
		}
		// Codex clean-review summaries and every configured-bot issue comment are
		// completion signals, not actionable findings: engine.Completion already
		// folded them into ReviewedBy, so they are never surfaced here.
		if dialect.IsCodexBot(comment.User.Login) && dialect.IsCodexNoActionReviewCompletion(comment.Body) {
			continue
		}
		if s.isConfiguredBot(comment.User.Login) {
			continue
		}
		if dialect.IsNonActionableText(comment.Body) {
			continue // notices/acks (e.g. usage-limit messages) aren't findings
		}
		if cutoff := headCutoffOf(); !cutoff.IsZero() && comment.CreatedAt.Before(cutoff) {
			continue // posted before the current head was committed — a stale round
		}
		report.Findings = append(report.Findings, dialect.Finding{
			Bot:       comment.User.Login,
			Severity:  dialect.SeverityOf(comment.Body),
			Title:     dialect.TitleOf(comment.Body),
			Body:      strings.TrimSpace(comment.Body),
			CommentID: comment.ID,
			URL:       comment.URL,
			Source:    "issue_comment",
			CreatedAt: comment.CreatedAt,
		})
	}

	report.Findings = dedupeFindings(report.Findings, suppressPromptAt)
	sort.Slice(report.Findings, func(i, j int) bool {
		if dialect.RankSeverity(report.Findings[i].Severity) != dialect.RankSeverity(report.Findings[j].Severity) {
			return dialect.RankSeverity(report.Findings[i].Severity) > dialect.RankSeverity(report.Findings[j].Severity)
		}
		if report.Findings[i].Path != report.Findings[j].Path {
			return report.Findings[i].Path < report.Findings[j].Path
		}
		return report.Findings[i].Line < report.Findings[j].Line
	})
	report.Converged = engine.Converged(report.Findings, completion)
	// Degrade detection: a live rate-limit window plus observed Codex
	// responsiveness means this round runs Codex-only for now. Converged is
	// structurally false here (CodeRabbit has no review evidence), so a
	// deferred round can never masquerade as converged.
	if s.cfg.RateLimitCodexDegrade && !report.Converged {
		if until, ok := st.AccountBlockedUntil(repo, pr, head, now); ok &&
			engine.CodexOnlyEligible(completionRound, obs.eng, &until, now) {
			report.CodeRabbitDeferred = true
			u := until
			report.DeferredUntil = &u
		}
	}
	switch {
	case report.Converged:
		report.Status = "converged"
	case report.CodeRabbitDeferred && len(report.Findings) == 0 &&
		engine.DoneExcept(report.ReviewedBy, s.cfg.Bot):
		report.Status = "deferred"
		report.Reason = "codex reviewed clean; coderabbit review deferred until " +
			report.DeferredUntil.UTC().Format(time.RFC3339) + " (account rate-limited)"
	case len(report.Findings) == 0:
		report.Status = "waiting"
	}
	return report, nil
}

func (s *Service) Loop(ctx context.Context, repo string, pr int) (FeedbackReport, int, error) {
	repo = NormalizeRepo(repo)
	// Do not spend a new review slot while actionable feedback from an earlier
	// round is still open. Feedback intentionally carries unresolved threads
	// across commits; surfacing them here makes "fix, resolve, then re-review" a
	// hard loop invariant instead of something every agent has to remember.
	//
	// An active wait for this head is different: extraction-only bots can answer
	// before the required reviewer, and those findings must remain buffered until
	// the configured reviewer gate completes.
	head, open, err := s.pullHead(ctx, repo, pr)
	if err != nil {
		return FeedbackReport{}, 1, err
	}
	if open {
		state, _, loadErr := s.store.Load(ctx)
		if loadErr != nil {
			return FeedbackReport{}, 1, loadErr
		}
		if state.WaitingHead(repo, pr) != head {
			for {
				report, feedbackErr := s.Feedback(ctx, repo, pr)
				if feedbackErr != nil {
					if wait, ok := ghapi.ThrottleWait(feedbackErr); ok {
						if wait <= 0 {
							wait = s.cfg.PollInterval
						}
						if serr := ghapi.SleepCtx(ctx, wait); serr != nil {
							return report, 1, serr
						}
						continue
					}
					return report, 1, feedbackErr
				}
				blocking := engine.BlockingFindings(report.Findings, head)
				if len(blocking) > 0 {
					report.Findings = blocking
					report.Status = "feedback"
					report.Reason = "unresolved findings must be addressed before a new review round"
					return report, 10, nil
				}
				break
			}
		}
	}
	waitResult, waitCode, err := s.waitToFire(ctx, repo, pr)
	if err != nil {
		return FeedbackReport{}, 1, err
	}
	if waitCode == 2 {
		status := "timeout"
		code := 2
		if waitResult.Action == "skipped" {
			status = "skipped"
			code = 0
		}
		// The slot wait timed out (CRQ_WAIT_TIMEOUT) without firing a review. Don't
		// enter the feedback poll — that would burn another feedback timeout and could
		// return stale pre-existing findings despite no new review round. Report the
		// timeout so the caller retries later instead. A skipped wait result is
		// terminal, not retryable, so preserve it as a skipped report.
		if waitResult.Action == "skipped" {
			s.completeWaitRound(ctx, repo, pr, "")
		}
		return FeedbackReport{Status: status, Repo: NormalizeRepo(repo), PR: pr, Head: waitResult.Head, Reason: waitResult.Reason, ReviewedBy: map[string]bool{}, Findings: []dialect.Finding{}}, code, nil
	}
	head = waitResult.Head
	if head == "" {
		var herr error
		head, _, herr = s.pullHead(ctx, repo, pr)
		if herr != nil {
			return FeedbackReport{}, 1, herr
		}
	}
	deadline, err := s.ensureWaitDeadline(ctx, repo, pr, head)
	if err != nil {
		return FeedbackReport{}, 1, err
	}
	var lastLog time.Time
	var settledAt time.Time
	// Pump keeps the queue moving while we wait, but once a minute is plenty (the
	// autoreview daemon pumps too); pumping on every tick just burns REST quota.
	var lastPump time.Time
	pumpEvery := pumpEveryFor(s.cfg.PollInterval)
	for {
		report, err := s.Feedback(ctx, repo, pr)
		if err != nil {
			// A GitHub REST throttle (the shared 5000/hr quota) is transient — ride
			// it out like a network outage rather than failing the agent. Wait for the
			// reset and push the review deadline past it: GitHub throttling isn't the
			// bot taking long to review.
			if wait, ok := ghapi.ThrottleWait(err); ok {
				if wait <= 0 {
					wait = s.cfg.PollInterval
				}
				deadline = deadline.Add(wait)
				if s.log != nil {
					s.log.Printf("%s#%d GitHub API throttled; waiting %s for the reset, then resuming", repo, pr, wait.Round(time.Second))
				}
				s.pushWaitDeadline(ctx, repo, pr, head, deadline)
				if serr := ghapi.SleepCtx(ctx, wait); serr != nil {
					return report, 1, serr
				}
				continue
			}
			return report, 1, err
		}
		// Findings are work immediately, even when another required reviewer is
		// still pending. Return control so the caller can fix locally, but keep the
		// reviewed head unchanged until every required bot finishes; pushing early
		// restarts the remaining checks and wastes the review slot.
		if len(report.Findings) > 0 {
			report.Status = "feedback"
			if allReviewed(report.ReviewedBy) {
				report.Reason = "all required reviewers finished; address findings, push once, and resolve threads"
				s.completeWaitRound(ctx, repo, pr, head)
			} else if report.CodeRabbitDeferred {
				// Degraded round: Codex answered while CodeRabbit is rate-limited.
				// These findings are this round's work — fixing and pushing is
				// exactly right; the CodeRabbit review stays queued and fires
				// against the newest head once the window opens.
				report.Reason = "codex findings during a coderabbit rate-limit window; fix, push, and loop again — the coderabbit review stays queued and fires when the window opens"
			} else {
				// A required reviewer is still pending (e.g. Codex posted a finding
				// before CodeRabbit reviewed). Return the findings to work on, but leave
				// the round active — completing it would release the slot and stop
				// observing the pending bot's review/rate-limit/timeout, losing evidence
				// the loop is still obligated to wait for.
				report.Reason = "hold current head: fix locally, but do not commit or push until every required reviewer finishes"
			}
			return report, 10, nil
		}
		if report.Converged || report.Status == "deferred" {
			// Don't trust the first converged observation: bots deliver in waves
			// (Codex auto-reviews a pushed head minutes later; CodeRabbit's real
			// review can trail its comment shells). Hold the verdict for the settle
			// window and only exit 0 if nothing new lands; any finding or pending
			// reviewer resets the normal flow above. A deferred (Codex-only) clean
			// verdict settles the same way but must NOT complete the round — the
			// queued CodeRabbit review is still owed for this head.
			if settledAt.IsZero() {
				settledAt = s.clock()
			}
			if s.cfg.SettleWindow <= 0 || s.clock().Sub(settledAt) >= s.cfg.SettleWindow {
				if report.Converged {
					s.completeWaitRound(ctx, repo, pr, head)
				}
				return report, 0, nil
			}
		} else {
			settledAt = time.Time{}
		}
		// Keep the queue moving (re-fire once an account-block window clears) and pick up
		// the Blocked state it leaves behind. Pumping every poll tick is redundant —
		// with several loops waiting concurrently it multiplies into real REST-quota
		// cost — so each waiter pumps at most once per pumpEvery.
		if lastPump.IsZero() || time.Since(lastPump) >= pumpEvery {
			if _, err := s.Pump(ctx); err != nil && s.log != nil {
				s.log.Printf("warning: pump while waiting for feedback failed: %v", err)
			}
			lastPump = time.Now()
		}
		// While the account is blocked the PR can't be reviewed yet — it just
		// stays queued — so that wait must not count against the feedback timeout, and
		// there's nothing to fetch until the window clears. Push the deadline past the
		// block and poll slowly, so a long queue wait doesn't drain the shared GitHub
		// REST quota (and re-hit its throttle) every PollInterval.
		poll := s.cfg.PollInterval
		var blockedUntil *time.Time
		now := s.clock()
		if st, _, lerr := s.store.Load(ctx); lerr == nil {
			if until, ok := st.AccountBlockedUntil(repo, pr, head, now); ok {
				blockedUntil = &until
			}
		}
		// A degraded round waits for Codex, not for the window: keep the normal
		// poll cadence against the un-extended deadline. (The rate-limit reply
		// usually lands before Codex answers, so an iteration or two may extend
		// the deadline first — harmless: once Codex evidence arrives the loop
		// exits promptly, and if Codex never answers the round degrades
		// gracefully back to riding out the window.)
		if blockedUntil != nil && !report.CodeRabbitDeferred {
			extended := extendDeadlineForBlock(deadline, blockedUntil, now, s.cfg.FeedbackWaitTimeout)
			if extended.After(deadline) {
				deadline = extended
				s.pushWaitDeadline(ctx, repo, pr, head, deadline)
			}
			poll = blockedPollInterval(*blockedUntil, now, s.cfg.PollInterval)
		}
		if now.After(deadline) && settledAt.IsZero() {
			// A degraded round must not be completed on timeout: marking the head
			// reviewed would silently cancel the still-owed CodeRabbit review.
			if !report.CodeRabbitDeferred {
				s.completeWaitRound(ctx, repo, pr, head)
			}
			if len(report.Findings) > 0 {
				report.Status = "feedback"
				report.Reason = "review wait timed out; actionable findings must be addressed before retrying"
				return report, 10, nil
			}
			report.Status = "timeout"
			return report, 2, nil
		}
		if s.log != nil && time.Since(lastLog) >= 30*time.Second {
			activeElapsed := feedbackWaitElapsed(deadline, s.cfg.FeedbackWaitTimeout, now)
			if blockedUntil != nil && report.CodeRabbitDeferred {
				s.log.Printf("%s#%d degraded to codex-only — coderabbit rate-limited until %s; waiting for codex on %s (%s / %s)", repo, pr, blockedUntil.UTC().Format(time.RFC3339), report.Head, activeElapsed.Round(time.Second), s.cfg.FeedbackWaitTimeout)
			} else if blockedUntil != nil {
				s.log.Printf("%s#%d queued — account blocked until %s; waiting, not counting it against the %s review wait (%s active)", repo, pr, blockedUntil.UTC().Format(time.RFC3339), s.cfg.FeedbackWaitTimeout, activeElapsed.Round(time.Second))
			} else {
				s.log.Printf("%s#%d waiting for review feedback on %s — reviewed %s (%s / %s)", repo, pr, report.Head, reviewedSummary(report.ReviewedBy), activeElapsed.Round(time.Second), s.cfg.FeedbackWaitTimeout)
			}
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return report, 1, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ensureWaitDeadline returns the wall-clock deadline the loop bounds its poll
// by. The wait IS the round: when the fired/reviewing round has no WaitDeadline
// yet (Pump normally sets it at fire time), this sets one budget past the fire.
// If the round is no longer waiting (completed/none), it returns a transient
// deadline so the loop still terminates.
func (s *Service) ensureWaitDeadline(ctx context.Context, repo string, pr int, head string) (time.Time, error) {
	repo = NormalizeRepo(repo)
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if dl, ok := st.RoundWaitDeadline(repo, pr, head); ok {
		return dl, nil
	}
	changed := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		r := st.Round(repo, pr)
		if r == nil || r.Head != head || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) || r.WaitDeadline != nil {
			return ErrNoChange
		}
		start := s.clock()
		if r.FiredAt != nil {
			start = r.FiredAt.UTC()
		}
		dl := start.Add(s.cfg.FeedbackWaitTimeout)
		r.WaitDeadline = &dl
		st.PutRound(*r)
		changed = true
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	if changed {
		s.sync(ctx, updated)
	}
	if dl, ok := updated.RoundWaitDeadline(repo, pr, head); ok {
		return dl, nil
	}
	// The round is no longer a wait (completed/none): synthesize a transient
	// deadline so the loop still bounds its poll.
	return s.clock().Add(s.cfg.FeedbackWaitTimeout), nil
}

// pushWaitDeadline moves the fired/reviewing round's wait deadline later (never
// earlier), persisting the extension an account block or GitHub throttle bought.
func (s *Service) pushWaitDeadline(ctx context.Context, repo string, pr int, head string, deadline time.Time) {
	repo = NormalizeRepo(repo)
	changed := false
	state, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		r := st.Round(repo, pr)
		if r == nil || r.Head != head || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
			return ErrNoChange
		}
		if r.WaitDeadline != nil && !deadline.After(*r.WaitDeadline) {
			return ErrNoChange
		}
		dl := deadline.UTC()
		r.WaitDeadline = &dl
		st.PutRound(*r)
		changed = true
		return nil
	})
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: failed to persist feedback wait deadline for %s#%d: %v", repo, pr, err)
		}
		return
	}
	if changed {
		s.sync(ctx, state)
	}
}

// completeWaitRound ends the wait by completing the fired/reviewing round. The
// completed round remains as the "this head was reviewed" dedup marker, so a
// subsequent enqueue/needsReview at the same head is deduped rather than re-fired.
func (s *Service) completeWaitRound(ctx context.Context, repo string, pr int, head string) {
	repo = NormalizeRepo(repo)
	changed := false
	state, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		r := st.Round(repo, pr)
		if r == nil || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
			return ErrNoChange
		}
		if head != "" && r.Head != head {
			return ErrNoChange
		}
		if err := r.Complete(); err != nil {
			return err
		}
		releaseSlot(st, QueueKey(repo, pr))
		st.PutRound(*r)
		changed = true
		return nil
	})
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: failed to clear feedback wait for %s#%d: %v", repo, pr, err)
		}
		return
	}
	if changed {
		s.sync(ctx, state)
	}
}

// waitToFire runs Wait (enqueue + coordinated fire), riding out GitHub REST rate
// limits the same way the feedback loop does instead of failing the agent on a
// transient throttle. Returns Wait's result and exit code (3 = already reviewed
// for head).
func (s *Service) waitToFire(ctx context.Context, repo string, pr int) (PumpResult, int, error) {
	for {
		result, code, err := s.Wait(ctx, repo, pr)
		if err == nil {
			return result, code, nil
		}
		wait, ok := ghapi.ThrottleWait(err)
		if !ok {
			return result, code, err
		}
		if wait <= 0 {
			wait = s.cfg.PollInterval
		}
		if s.log != nil {
			s.log.Printf("%s#%d GitHub API throttled before firing; waiting %s for the reset, then retrying", repo, pr, wait.Round(time.Second))
		}
		if serr := ghapi.SleepCtx(ctx, wait); serr != nil {
			return result, code, serr
		}
	}
}

// blockedPollInterval slows the feedback poll while the account is blocked:
// nothing can be fetched until the window clears, so wait until just past the
// reset instead of every PollInterval — capped so the loop still re-checks
// periodically. Keeps a long queue wait from draining the shared GitHub REST quota.
// pumpEveryFor bounds how often a waiting feedback loop pumps the queue: never
// more than once a minute, and never more often than it polls. Several loops
// waiting concurrently each used to pump every tick, which multiplied into real
// REST-quota drain for zero extra queue throughput.
func pumpEveryFor(poll time.Duration) time.Duration {
	if poll < time.Minute {
		return time.Minute
	}
	return poll
}

func blockedPollInterval(blockedUntil, now time.Time, base time.Duration) time.Duration {
	const maxWait = 5 * time.Minute
	wait := blockedUntil.Sub(now) + time.Second
	if wait < base {
		return base
	}
	if wait > maxWait {
		return maxWait
	}
	return wait
}

// extendDeadlineForBlock keeps the feedback-wait deadline from elapsing while the
// CodeRabbit account is blocked. A blocked PR can't be reviewed — it just
// stays queued until the window clears and crq re-fires — so that time shouldn't
// burn the review-wait budget. When blocked, the deadline is pushed to a full
// budget past the block; it is never moved earlier.
func extendDeadlineForBlock(deadline time.Time, blockedUntil *time.Time, now time.Time, budget time.Duration) time.Time {
	if blockedUntil == nil || !blockedUntil.After(now) {
		return deadline
	}
	if extended := blockedUntil.Add(budget); extended.After(deadline) {
		return extended
	}
	return deadline
}

// feedbackWaitElapsed reports only reviewable time. An account block extends
// deadline, so deriving progress from the remaining budget excludes the blocked
// interval and can never produce impossible output such as "35m / 20m".
func feedbackWaitElapsed(deadline time.Time, budget time.Duration, now time.Time) time.Duration {
	if budget <= 0 {
		return 0
	}
	elapsed := budget - deadline.Sub(now)
	if elapsed < 0 {
		return 0
	}
	if elapsed > budget {
		return budget
	}
	return elapsed
}

func reviewedSummary(m map[string]bool) string {
	if len(m) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		state := "waiting"
		if m[k] {
			state = "done"
		}
		parts = append(parts, k+"="+state)
	}
	return strings.Join(parts, " ")
}

type ResolvedThread struct {
	ThreadID string `json:"thread_id"`
	Resolved bool   `json:"resolved"`
}

func (s *Service) ResolveThreads(ctx context.Context, threadIDs []string) ([]ResolvedThread, error) {
	var out []ResolvedThread
	for _, id := range threadIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var result struct {
			ResolveReviewThread struct {
				Thread struct {
					ID         string `json:"id"`
					IsResolved bool   `json:"isResolved"`
				} `json:"thread"`
			} `json:"resolveReviewThread"`
		}
		err := s.gh.GraphQL(ctx, `mutation($id:ID!){
  resolveReviewThread(input:{threadId:$id}) {
    thread { id isResolved }
  }
}`, map[string]any{"id": id}, &result)
		if err != nil {
			return out, err
		}
		out = append(out, ResolvedThread{ThreadID: result.ResolveReviewThread.Thread.ID, Resolved: result.ResolveReviewThread.Thread.IsResolved})
	}
	return out, nil
}

type DeclinedThread struct {
	ThreadID string `json:"thread_id"`
	URL      string `json:"url,omitempty"`
	Resolved bool   `json:"resolved"`
}

// DeclineThreads posts a reason as a reply on each review thread, documenting why
// a finding is not being addressed. By default the thread is left unresolved (an
// on-the-record disagreement); pass resolve=true to also close it ("won't fix").
func (s *Service) DeclineThreads(ctx context.Context, threadIDs []string, reason string, resolve bool) ([]DeclinedThread, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("a decline reason is required (--reason)")
	}
	var out []DeclinedThread
	for _, id := range threadIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var reply struct {
			AddPullRequestReviewThreadReply struct {
				Comment struct {
					URL string `json:"url"`
				} `json:"comment"`
			} `json:"addPullRequestReviewThreadReply"`
		}
		err := s.gh.GraphQL(ctx, `mutation($threadId:ID!,$body:String!){
  addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadId, body:$body}) {
    comment { url }
  }
}`, map[string]any{"threadId": id, "body": reason}, &reply)
		if err != nil {
			return out, err
		}
		dt := DeclinedThread{ThreadID: id, URL: reply.AddPullRequestReviewThreadReply.Comment.URL}
		if resolve {
			resolved, rerr := s.ResolveThreads(ctx, []string{id})
			if rerr != nil {
				return out, rerr
			}
			if len(resolved) > 0 {
				dt.Resolved = resolved[0].Resolved
			}
		}
		out = append(out, dt)
	}
	return out, nil
}

type reviewThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Comments   struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			DatabaseID   int64     `json:"databaseId"`
			Body         string    `json:"body"`
			URL          string    `json:"url"`
			Path         string    `json:"path"`
			Line         int       `json:"line"`
			OriginalLine int       `json:"originalLine"`
			CreatedAt    time.Time `json:"createdAt"`
			Author       struct {
				Login string `json:"login"`
			} `json:"author"`
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
			OriginalCommit struct {
				OID string `json:"oid"`
			} `json:"originalCommit"`
		} `json:"nodes"`
	} `json:"comments"`
}

func (s *Service) reviewThreads(ctx context.Context, repo string, pr int) ([]reviewThread, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var all []reviewThread
	cursor := ""
	for {
		var result struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []reviewThread `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		variables := map[string]any{"owner": owner, "name": name, "number": pr, "cursor": nil}
		if cursor != "" {
			variables["cursor"] = cursor
		}
		query := `query($owner:String!, $name:String!, $number:Int!, $cursor:String) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      reviewThreads(first:100, after:$cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated path line
          comments(first:50) {
            totalCount
            nodes {
              databaseId body url path line originalLine createdAt
              author { login }
              commit { oid }
              originalCommit { oid }
            }
          }
        }
      }
    }
  }
}`
		if err := s.gh.GraphQL(ctx, query, variables, &result); err != nil {
			return all, err
		}
		page := result.Repository.PullRequest.ReviewThreads
		all = append(all, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			break
		}
		cursor = page.PageInfo.EndCursor
	}
	return all, nil
}

// parseReviewBodyFindings adapts a GitHub review to the dialect parsers, which
// take only the metadata they attach to findings.
func parseReviewBodyFindings(review ghapi.Review, bot string) []dialect.Finding {
	return dialect.ParseReviewBodyFindings(review.Body, dialect.ReviewMeta{
		ID:          review.ID,
		CommitID:    review.CommitID,
		HTMLURL:     review.HTMLURL,
		SubmittedAt: review.SubmittedAt,
	}, bot)
}

// reviewNewer reports whether review a supersedes b: later submission wins, and
// a higher ID breaks ties (equal/zero timestamps) so selection is deterministic.
func reviewNewer(a, b ghapi.Review) bool {
	if !a.SubmittedAt.Equal(b.SubmittedAt) {
		return a.SubmittedAt.After(b.SubmittedAt)
	}
	return a.ID > b.ID
}

// threadRebuttal surfaces a bot's contested reply on a RESOLVED thread as a
// finding. When the agent declines a finding with `crq decline --resolve`, the
// bot often replies conceding ("I'm withdrawing this finding") or contesting
// ("I'm retaining the finding: ..."). threadFindings drops resolved threads, so
// a contest would vanish and the loop would converge over an unaddressed
// rebuttal. This re-surfaces it: the thread's latest comment is a bot reply that
// follows the agent's own comment and does not clearly withdraw the finding.
// Ambiguous replies surface too — never bury a rebuttal on a false concession.
// Returns nil when the thread is unresolved (threadFindings already covers it),
// when the last word is not the bot's, when the agent never replied, or when the
// bot withdrew.
func threadRebuttal(thread reviewThread, bots map[string]struct{}) *dialect.Finding {
	if !thread.IsResolved && !thread.IsOutdated {
		return nil
	}
	nodes := thread.Comments.Nodes
	if len(nodes) < 2 {
		return nil
	}
	// Only judge complete threads: comments(first:50) truncates long
	// discussions, and the "last word" below would be a stale mid-thread reply.
	// Skipping is safe — a thread that long has had human attention.
	if thread.Comments.TotalCount > len(nodes) {
		return nil
	}
	last := nodes[len(nodes)-1]
	if !dialect.InBots(bots, last.Author.Login) {
		return nil // the agent, not the bot, had the last word
	}
	// The rebuttal shape is strictly bot finding → agent reply → bot last word.
	// A human-started thread that a bot merely answered is not a declined
	// finding, and surfacing it would fabricate a contest.
	if !dialect.InBots(bots, nodes[0].Author.Login) {
		return nil
	}
	agentReplied := false
	for _, c := range nodes[1 : len(nodes)-1] {
		if !dialect.InBots(bots, c.Author.Login) {
			agentReplied = true
			break
		}
	}
	if !agentReplied {
		return nil // the bot is talking to itself, not answering a decline
	}
	if dialect.IsReviewFindingWithdrawn(last.Body) {
		return nil // conceded — the decline stands
	}
	if dialect.IsNonActionableText(last.Body) {
		return nil // a platform notice or ack, not a rebuttal (e.g. Codex's
		// "create an environment" boilerplate posted as a thread reply)
	}
	// A contested decline deserves attention even when the finding's own severity
	// is a nitpick, so floor an unknown severity at major.
	severity := dialect.FloorSeverity(dialect.SeverityOf(last.Body), "major")
	return &dialect.Finding{
		Bot:       last.Author.Login,
		Severity:  severity,
		Path:      firstNonEmpty(thread.Path, last.Path),
		Line:      firstPositive(thread.Line, last.Line, last.OriginalLine),
		Title:     "Reviewer contests your reply — re-address or reply again: " + dialect.TitleOf(last.Body),
		Body:      strings.TrimSpace(last.Body),
		ThreadID:  thread.ID,
		CommentID: last.DatabaseID,
		URL:       last.URL,
		Source:    "review_reply",
		CreatedAt: last.CreatedAt,
	}
}

// threadFindings turns one GitHub review thread into findings. An unresolved,
// non-outdated thread is still actionable no matter which commit its comments
// were filed on: GitHub's own resolution/outdated state is the source of truth,
// so a real finding from an earlier commit is surfaced instead of silently
// dropped when HEAD moves. (This is why callers do not need a manual
// cross-review audit.) Resolved or outdated threads are skipped.
func threadFindings(thread reviewThread, bots map[string]struct{}) []dialect.Finding {
	if thread.IsResolved || thread.IsOutdated {
		return nil
	}
	var out []dialect.Finding
	for _, comment := range thread.Comments.Nodes {
		if !dialect.InBots(bots, comment.Author.Login) {
			continue
		}
		commit := dialect.ShortOID(comment.Commit.OID)
		if commit == "" {
			commit = dialect.ShortOID(comment.OriginalCommit.OID)
		}
		out = append(out, dialect.Finding{
			Bot:       comment.Author.Login,
			Severity:  dialect.SeverityOf(comment.Body),
			Path:      firstNonEmpty(thread.Path, comment.Path),
			Line:      firstPositive(thread.Line, comment.Line, comment.OriginalLine),
			Title:     dialect.TitleOf(comment.Body),
			Body:      strings.TrimSpace(comment.Body),
			ThreadID:  thread.ID,
			CommentID: comment.DatabaseID,
			Commit:    commit,
			URL:       comment.URL,
			Source:    "review_thread",
			CreatedAt: comment.CreatedAt,
		})
	}
	return out
}

// promptSuppressKeys returns the bot|path|line dedupe keys for a thread's bot
// comments, matching the keys dedupeFindings builds for prompt findings, so a
// resolved/outdated thread can suppress its "Prompt for AI agents" duplicate.
func promptSuppressKeys(thread reviewThread, bots map[string]struct{}) []string {
	var keys []string
	for _, comment := range thread.Comments.Nodes {
		if !dialect.InBots(bots, comment.Author.Login) {
			continue
		}
		path := firstNonEmpty(thread.Path, comment.Path)
		line := firstPositive(thread.Line, comment.Line, comment.OriginalLine)
		if path == "" || line <= 0 {
			continue
		}
		keys = append(keys, dialect.NormalizeBotName(comment.Author.Login)+"|"+path+"|"+strconv.Itoa(line))
	}
	return keys
}

func dedupeFindings(in []dialect.Finding, suppressPromptAt map[string]bool) []dialect.Finding {
	seen := map[string]bool{}
	structuredAtLocation := map[string]bool{}
	for _, finding := range in {
		if finding.Source != "review_prompt" && finding.Path != "" && finding.Line > 0 {
			structuredAtLocation[dialect.NormalizeBotName(finding.Bot)+"|"+finding.Path+"|"+strconv.Itoa(finding.Line)] = true
		}
	}
	out := []dialect.Finding{}
	for _, finding := range in {
		finding.Body = strings.TrimSpace(finding.Body)
		finding.Title = strings.TrimSpace(finding.Title)
		if !dialect.IsActionableFinding(finding) {
			continue
		}
		if finding.Source == "review_prompt" {
			key := dialect.NormalizeBotName(finding.Bot) + "|" + finding.Path + "|" + strconv.Itoa(finding.Line)
			if structuredAtLocation[key] || suppressPromptAt[key] {
				continue
			}
		}
		key := dialect.NormalizeBotName(finding.Bot) + "|" + finding.Path + "|" + strconv.Itoa(finding.Line) + "|" + finding.Title + "|" + finding.Body + "|" + finding.ThreadID
		sum := sha256.Sum256([]byte(key))
		finding.ID = hex.EncodeToString(sum[:])
		if seen[finding.ID] {
			continue
		}
		seen[finding.ID] = true
		out = append(out, finding)
	}
	return out
}

func isCurrentCodexThumbsUp(reaction ghapi.Reaction, since time.Time) bool {
	if !dialect.IsCodexBot(reaction.User.Login) || reaction.Content != "+1" {
		return false
	}
	return reaction.CreatedAt.IsZero() || notBefore(reaction.CreatedAt, since)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func (r FeedbackReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
