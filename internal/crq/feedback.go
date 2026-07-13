package crq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Finding struct {
	ID        string    `json:"id"`
	Bot       string    `json:"bot"`
	Severity  string    `json:"severity"`
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	ThreadID  string    `json:"thread_id,omitempty"`
	CommentID int64     `json:"comment_id,omitempty"`
	ReviewID  int64     `json:"review_id,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	URL       string    `json:"url,omitempty"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type FeedbackReport struct {
	Status     string          `json:"status"`
	Repo       string          `json:"repo"`
	PR         int             `json:"pr"`
	Head       string          `json:"head"`
	Reason     string          `json:"reason,omitempty"`
	Converged  bool            `json:"converged"`
	ReviewedBy map[string]bool `json:"reviewed_by"`
	Findings   []Finding       `json:"findings"`
	CheckedAt  time.Time       `json:"checked_at"`
}

func (s *Service) Feedback(ctx context.Context, repo string, pr int) (FeedbackReport, error) {
	repo = NormalizeRepo(repo)
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return FeedbackReport{}, err
	}
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
		Findings:   []Finding{},
		CheckedAt:  time.Now().UTC(),
	}
	// Two bot sets with different jobs. requiredBots gates convergence: crq isn't
	// "done" until every one has reviewed the head, so only these seed ReviewedBy.
	// extractBots is the broader set whose findings we surface — a superset that
	// includes Codex — so a bot that reviews without being required (and would hang
	// convergence if it were) still has its findings reported instead of dropped.
	requiredBots := botSet(s.cfg.RequiredBots)
	// Always extract from the required bots too: a bot crq waits for whose findings
	// it didn't surface would hang the loop forever. FeedbackBots only widens this.
	extractBots := botSet(unionBots(s.cfg.FeedbackBots, s.cfg.RequiredBots))
	for bot := range requiredBots {
		report.ReviewedBy[bot] = false
	}
	completion := s.feedbackCompletionContext(ctx, repo, pr, head)

	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return report, err
	}
	// Review-body findings — CodeRabbit's detailed and "Prompt for AI agents"
	// blocks — carry no per-finding resolution state, only the review's commit.
	// When inline comments fail to post (GitHub 5xx / code-review limits) that
	// body block is the ONLY record of the findings, so gating extraction to the
	// current head silently drops an entire review the moment the head moves on
	// (a rebase, a squash-merge). Extract instead from each bot's LATEST review
	// regardless of its commit: a newer review from the same bot supersedes it,
	// and resolved/outdated inline threads still suppress individual prompt
	// duplicates below. Convergence (markReviewed) stays gated to a review whose
	// commit matches the head, so the loop still waits for a real head review.
	latestReview := map[string]Review{}
	for _, review := range reviews {
		login := review.User.Login
		if !inBots(extractBots, login) {
			continue
		}
		// Once a fresh review round has started for this head, a body submitted
		// before that round belongs to the previous head. Unresolved threads are
		// still surfaced below across commits, while thread-less body findings
		// must be re-reported by the current round instead of trapping the loop.
		if completion.OK &&
			(head == "" || !strings.HasPrefix(review.CommitID, head)) &&
			!notBefore(review.SubmittedAt, completion.Cutoff) {
			continue
		}
		if head != "" && strings.HasPrefix(review.CommitID, head) {
			// markReviewed only flips existing ReviewedBy keys (required bots), so a
			// non-required extract bot reviewing here is a harmless no-op.
			markReviewed(report.ReviewedBy, login)
		}
		if cur, ok := latestReview[login]; !ok || reviewNewer(review, cur) {
			latestReview[login] = review
		}
	}
	for _, review := range reviews {
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
			}
		}
	} else if IsRateLimited(err) {
		// A transient GraphQL rate limit must not silently degrade to the REST
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
			if !inBots(extractBots, comment.User.Login) {
				continue
			}
			commit := shortOID(firstNonEmpty(comment.CommitID, comment.OriginalCommitID))
			if head != "" && commit != "" && commit != head {
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Bot:       comment.User.Login,
				Severity:  severityOf(comment.Body),
				Path:      comment.Path,
				Line:      firstPositive(comment.Line, comment.OriginalLine),
				Title:     titleOf(comment.Body),
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

	issueComments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		// Don't silently drop the issue-comment source: a rate limit or API error
		// here would otherwise let crq report clean/converged while Codex issue
		// comments (or completion signals) were simply never fetched.
		return report, err
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
	for _, comment := range issueComments {
		if !inBots(extractBots, comment.User.Login) {
			continue
		}
		if s.isRateLimited(comment.Body) {
			continue
		}
		// Codex reports a clean review as a top-level issue comment rather than a
		// submitted GitHub review. Treat that message as a completion signal when
		// Codex gates this round, and never surface it as an actionable finding when
		// Codex is extraction-only. The persisted wait is the only safe way to bind
		// an issue comment (which has no commit SHA) to the current head.
		if isCodexBot(comment.User.Login) && isCodexNoActionReviewCompletion(comment.Body) {
			if completion.OK && notBefore(issueCommentTime(comment), completion.Cutoff) {
				markReviewed(report.ReviewedBy, comment.User.Login)
			}
			continue
		}
		// Issue comments carry no commit SHA, so a stale completion summary from an
		// earlier commit must not be treated as a review of the current head — rely on
		// the persisted current-round feedback wait before treating a configured-bot
		// no-action summary as the review completion signal.
		if s.isConfiguredBot(comment.User.Login) {
			if completion.OK && isNoActionReviewCompletion(comment.Body) && notBefore(issueCommentTime(comment), completion.Cutoff) {
				codexOK, err := s.codexInactiveOrThumbed(ctx, repo, pr, head, completion, issueComments, reviews, report.ReviewedBy)
				if err != nil {
					return report, err
				}
				if !codexOK {
					continue
				}
				markReviewed(report.ReviewedBy, comment.User.Login)
			}
			continue
		}
		if isNonActionableText(comment.Body) {
			continue // notices/acks (e.g. usage-limit messages) aren't findings
		}
		if cutoff := headCutoffOf(); !cutoff.IsZero() && comment.CreatedAt.Before(cutoff) {
			continue // posted before the current head was committed — a stale round
		}
		report.Findings = append(report.Findings, Finding{
			Bot:       comment.User.Login,
			Severity:  severityOf(comment.Body),
			Title:     titleOf(comment.Body),
			Body:      strings.TrimSpace(comment.Body),
			CommentID: comment.ID,
			URL:       comment.URL,
			Source:    "issue_comment",
			CreatedAt: comment.CreatedAt,
		})
	}

	s.applyCompletionReplyFallback(ctx, repo, pr, head, &report, issueComments, reviews)

	report.Findings = dedupeFindings(report.Findings, suppressPromptAt)
	sort.Slice(report.Findings, func(i, j int) bool {
		if rankSeverity(report.Findings[i].Severity) != rankSeverity(report.Findings[j].Severity) {
			return rankSeverity(report.Findings[i].Severity) > rankSeverity(report.Findings[j].Severity)
		}
		if report.Findings[i].Path != report.Findings[j].Path {
			return report.Findings[i].Path < report.Findings[j].Path
		}
		return report.Findings[i].Line < report.Findings[j].Line
	})
	report.Converged = len(report.Findings) == 0
	for _, reviewed := range report.ReviewedBy {
		report.Converged = report.Converged && reviewed
	}
	if report.Converged {
		report.Status = "converged"
	} else if len(report.Findings) == 0 {
		report.Status = "waiting"
	}
	return report, nil
}

type feedbackCompletionContext struct {
	Cutoff         time.Time
	FiredCommentID int64
	OK             bool
}

func (s *Service) feedbackCompletionContext(ctx context.Context, repo string, pr int, head string) feedbackCompletionContext {
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return feedbackCompletionContext{}
	}
	key := QueueKey(repo, pr)
	if wait := state.AwaitingFeedback[key]; wait.Head == head && !wait.StartedAt.IsZero() {
		firedCommentID := wait.FiredCommentID
		if firedCommentID == 0 {
			firedCommentID = feedbackWaitFiredCommentID(state, repo, pr, head)
		}
		return feedbackCompletionContext{Cutoff: wait.StartedAt.UTC(), FiredCommentID: firedCommentID, OK: true}
	}
	if state.InFlight != nil && state.InFlight.Repo == repo && state.InFlight.PR == pr && state.InFlight.Head == head && state.InFlight.FiredAt != nil {
		return feedbackCompletionContext{Cutoff: state.InFlight.FiredAt.UTC(), FiredCommentID: state.InFlight.FiredCommentID, OK: true}
	}
	for _, item := range state.History {
		if NormalizeRepo(item.Repo) == repo && item.PR == pr && item.Commit == head && !item.At.IsZero() {
			return feedbackCompletionContext{Cutoff: item.At.UTC(), OK: true}
		}
	}
	return feedbackCompletionContext{}
}

func (s *Service) codexInactiveOrThumbed(ctx context.Context, repo string, pr int, head string, completion feedbackCompletionContext, issueComments []IssueComment, reviews []Review, reviewedBy map[string]bool) (bool, error) {
	codexActive := hasCodexBot(s.cfg.RequiredBots)
	codexReviewed := reviewedByBot(reviewedBy, "chatgpt-codex-connector[bot]")
	for _, review := range reviews {
		if !isCodexBot(review.User.Login) {
			continue
		}
		if head != "" && review.CommitID != "" && strings.HasPrefix(review.CommitID, head) {
			codexActive = true
			codexReviewed = true
			break
		}
		if review.CommitID == "" && !review.SubmittedAt.IsZero() && notBefore(review.SubmittedAt, completion.Cutoff) {
			codexActive = true
			codexReviewed = true
			break
		}
	}
	if codexReviewed {
		return true, nil
	}
	if !codexActive {
		for _, comment := range issueComments {
			if isCodexBot(comment.User.Login) && !isNonActionableText(comment.Body) && notBefore(issueCommentTime(comment), completion.Cutoff) {
				codexActive = true
				break
			}
		}
	}
	if !codexActive {
		return true, nil
	}
	if ok, err := s.codexThumbedUp(ctx, repo, pr, completion); err != nil {
		return false, err
	} else if ok {
		markReviewed(reviewedBy, "chatgpt-codex-connector[bot]")
		return true, nil
	}
	return false, nil
}

func (s *Service) codexThumbedUp(ctx context.Context, repo string, pr int, completion feedbackCompletionContext) (bool, error) {
	reactions, err := s.gh.ListIssueReactions(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	for _, reaction := range reactions {
		if isCurrentCodexThumbsUp(reaction, completion.Cutoff) {
			return true, nil
		}
	}
	if completion.FiredCommentID == 0 {
		return false, nil
	}
	reactions, err = s.gh.ListCommentReactions(ctx, repo, completion.FiredCommentID)
	if err != nil {
		return false, err
	}
	for _, reaction := range reactions {
		if isCurrentCodexThumbsUp(reaction, completion.Cutoff) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) Loop(ctx context.Context, repo string, pr int) (FeedbackReport, int, error) {
	repo = NormalizeRepo(repo)
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
			s.clearFeedbackWait(ctx, repo, pr, "")
		}
		return FeedbackReport{Status: status, Repo: NormalizeRepo(repo), PR: pr, Head: waitResult.Head, Reason: waitResult.Reason, ReviewedBy: map[string]bool{}, Findings: []Finding{}}, code, nil
	}
	head := waitResult.Head
	if head == "" {
		var herr error
		head, _, herr = s.pullHead(ctx, repo, pr)
		if herr != nil {
			return FeedbackReport{}, 1, herr
		}
	}
	wait, err := s.ensureFeedbackWait(ctx, repo, pr, head)
	if err != nil {
		return FeedbackReport{}, 1, err
	}
	deadline := wait.Deadline
	start := wait.StartedAt
	if start.IsZero() {
		start = time.Now().UTC()
	}
	var lastLog time.Time
	// Pump keeps the queue moving while we wait, but once a minute is plenty (the
	// autoreview daemon pumps too); pumping on every tick just burns REST quota.
	var lastPump time.Time
	pumpEvery := pumpEveryFor(s.cfg.PollInterval)
	for {
		report, err := s.Feedback(ctx, repo, pr)
		if err != nil {
			// A GitHub REST rate limit (the shared 5000/hr quota) is transient — ride
			// it out like a network outage rather than failing the agent. Wait for the
			// reset and push the review deadline past it: GitHub throttling isn't the
			// bot taking long to review.
			if wait, ok := rateLimitWait(err); ok {
				if wait <= 0 {
					wait = s.cfg.PollInterval
				}
				deadline = deadline.Add(wait)
				if s.log != nil {
					s.log.Printf("%s#%d GitHub API rate-limited; waiting %s for the reset, then resuming", repo, pr, wait.Round(time.Second))
				}
				s.extendFeedbackWaitDeadline(ctx, repo, pr, head, deadline)
				if serr := sleepCtx(ctx, wait); serr != nil {
					return report, 1, serr
				}
				continue
			}
			return report, 1, err
		}
		// Extraction-only bots such as Codex often respond before the required
		// CodeRabbit review. Keep their findings buffered until every required bot
		// has reviewed this head, then return the complete round in one report.
		if len(report.Findings) > 0 && allReviewed(report.ReviewedBy) {
			report.Status = "feedback"
			s.clearFeedbackWait(ctx, repo, pr, head)
			return report, 10, nil
		}
		if report.Converged {
			s.clearFeedbackWait(ctx, repo, pr, head)
			return report, 0, nil
		}
		// Keep the queue moving (re-fire once a rate-limit window clears) and pick up
		// the Blocked state it leaves behind. Pumping every poll tick is redundant —
		// with several loops waiting concurrently it multiplies into real REST-quota
		// cost — so each waiter pumps at most once per pumpEvery.
		if lastPump.IsZero() || time.Since(lastPump) >= pumpEvery {
			if _, err := s.Pump(ctx); err != nil && s.log != nil {
				s.log.Printf("warning: pump while waiting for feedback failed: %v", err)
			}
			lastPump = time.Now()
		}
		// While the account is rate-limited the PR can't be reviewed yet — it just
		// stays queued — so that wait must not count against the feedback timeout, and
		// there's nothing to fetch until the window clears. Push the deadline past the
		// block and poll slowly, so a long queue wait doesn't drain the shared GitHub
		// REST quota (and re-hit its rate limit) every PollInterval.
		poll := s.cfg.PollInterval
		var blockedUntil *time.Time
		if st, _, lerr := s.store.Load(ctx); lerr == nil && st.Blocked.BlockedUntil != nil && st.Blocked.BlockedUntil.After(time.Now()) {
			blockedUntil = st.Blocked.BlockedUntil
			extended := extendDeadlineForBlock(deadline, blockedUntil, time.Now(), s.cfg.FeedbackWaitTimeout)
			if extended.After(deadline) {
				deadline = extended
				s.extendFeedbackWaitDeadline(ctx, repo, pr, head, deadline)
			}
			poll = blockedPollInterval(*blockedUntil, time.Now(), s.cfg.PollInterval)
		}
		if time.Now().After(deadline) {
			report.Status = "timeout"
			s.clearFeedbackWait(ctx, repo, pr, head)
			return report, 2, nil
		}
		if s.log != nil && time.Since(lastLog) >= 30*time.Second {
			if blockedUntil != nil {
				s.log.Printf("%s#%d queued — account rate-limited until %s; waiting, not counting it against the %s review wait (%s elapsed)", repo, pr, blockedUntil.UTC().Format(time.RFC3339), s.cfg.FeedbackWaitTimeout, time.Since(start).Round(time.Second))
			} else {
				s.log.Printf("%s#%d waiting for review feedback on %s — reviewed %s (%s / %s)", repo, pr, report.Head, reviewedSummary(report.ReviewedBy), time.Since(start).Round(time.Second), s.cfg.FeedbackWaitTimeout)
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

func (s *Service) newFeedbackWait(repo string, pr int, head string, started time.Time, firedCommentID int64) FeedbackWait {
	started = started.UTC()
	if started.IsZero() {
		started = time.Now().UTC()
	}
	return FeedbackWait{
		Repo:           NormalizeRepo(repo),
		PR:             pr,
		Head:           head,
		StartedAt:      started,
		Deadline:       started.Add(s.cfg.FeedbackWaitTimeout),
		FiredCommentID: firedCommentID,
		ByHost:         s.cfg.Host,
	}
}

func (s *Service) ensureFeedbackWait(ctx context.Context, repo string, pr int, head string) (FeedbackWait, error) {
	repo = NormalizeRepo(repo)
	key := QueueKey(repo, pr)
	var wait FeedbackWait
	changed := false
	state, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		if st.AwaitingFeedback == nil {
			st.AwaitingFeedback = map[string]FeedbackWait{}
		}
		firedCommentID := feedbackWaitFiredCommentID(*st, repo, pr, head)
		if existing := st.AwaitingFeedback[key]; existing.Head == head {
			wait = existing
			if wait.StartedAt.IsZero() {
				wait.StartedAt = feedbackWaitStart(*st, repo, pr, head, time.Now().UTC())
				changed = true
			}
			if wait.Deadline.IsZero() {
				wait.Deadline = wait.StartedAt.Add(s.cfg.FeedbackWaitTimeout)
				changed = true
			}
			wait.Repo = repo
			wait.PR = pr
			if wait.ByHost == "" {
				wait.ByHost = s.cfg.Host
				changed = true
			}
			if wait.FiredCommentID == 0 && firedCommentID != 0 {
				wait.FiredCommentID = firedCommentID
				changed = true
			}
			if changed {
				st.AwaitingFeedback[key] = wait
				st.Fired[key] = head
				return nil
			}
			return ErrNoChange
		}
		started := feedbackWaitStart(*st, repo, pr, head, time.Now().UTC())
		wait = s.newFeedbackWait(repo, pr, head, started, firedCommentID)
		st.AwaitingFeedback[key] = wait
		st.Fired[key] = head
		changed = true
		return nil
	})
	if err != nil {
		return FeedbackWait{}, err
	}
	if changed {
		s.sync(ctx, state)
	}
	return wait, nil
}

func feedbackWaitFiredCommentID(st State, repo string, pr int, head string) int64 {
	if st.InFlight != nil && st.InFlight.Repo == repo && st.InFlight.PR == pr && st.InFlight.Head == head {
		return st.InFlight.FiredCommentID
	}
	return 0
}

func feedbackWaitStart(st State, repo string, pr int, head string, fallback time.Time) time.Time {
	if st.InFlight != nil && st.InFlight.Repo == repo && st.InFlight.PR == pr && st.InFlight.Head == head && st.InFlight.FiredAt != nil {
		return st.InFlight.FiredAt.UTC()
	}
	for _, item := range st.History {
		if NormalizeRepo(item.Repo) == repo && item.PR == pr && item.Commit == head {
			return item.At.UTC()
		}
	}
	return fallback.UTC()
}

func (s *Service) extendFeedbackWaitDeadline(ctx context.Context, repo string, pr int, head string, deadline time.Time) {
	repo = NormalizeRepo(repo)
	key := QueueKey(repo, pr)
	changed := false
	state, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		wait := st.AwaitingFeedback[key]
		if wait.Head != head {
			return ErrNoChange
		}
		if !deadline.After(wait.Deadline) {
			return ErrNoChange
		}
		wait.Deadline = deadline.UTC()
		st.AwaitingFeedback[key] = wait
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

func (s *Service) clearFeedbackWait(ctx context.Context, repo string, pr int, head string) {
	repo = NormalizeRepo(repo)
	key := QueueKey(repo, pr)
	changed := false
	state, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		wait := st.AwaitingFeedback[key]
		if wait.Head == "" {
			return ErrNoChange
		}
		if head != "" && wait.Head != head {
			return ErrNoChange
		}
		delete(st.AwaitingFeedback, key)
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
		wait, ok := rateLimitWait(err)
		if !ok {
			return result, code, err
		}
		if wait <= 0 {
			wait = s.cfg.PollInterval
		}
		if s.log != nil {
			s.log.Printf("%s#%d GitHub API rate-limited before firing; waiting %s for the reset, then retrying", repo, pr, wait.Round(time.Second))
		}
		if serr := sleepCtx(ctx, wait); serr != nil {
			return result, code, serr
		}
	}
}

// blockedPollInterval slows the feedback poll while the account is rate-limited:
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
// CodeRabbit account is rate-limited. A blocked PR can't be reviewed — it just
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
		Nodes []struct {
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

var (
	detailSummaryRE = regexp.MustCompile(`(?i)<summary>\s*([^<]+?)\s+\([0-9]+\)\s*</summary>`)
	// Line headers come backticked in "Outside diff range comments" (`12-15`:) and
	// un-backticked in "Comments failed to post" (12-15:) — accept both.
	detailHeaderRE = regexp.MustCompile("^`?([0-9]+)(?:\\s*-\\s*([0-9]+))?`?: *(.*)$")
	promptBlockRE  = regexp.MustCompile("(?is)<summary>[^<]*Prompt for all review comments with AI agents[^<]*</summary>.*?```\\s*(.*?)\\s*```")
	promptFileRE   = regexp.MustCompile("^In (?:`@([^`]+)`|@([^:]+)):$")
	promptBulletRE = regexp.MustCompile("^- (?:Around line|Line)\\s+([0-9]+)(?:\\s*-\\s*([0-9]+))?:\\s*(.*)$")
	boldTitleRE    = regexp.MustCompile(`(?m)^\*\*([^*\n]+)\*\*`)
	crCommentRE    = regexp.MustCompile(`<!--\s*cr-comment:v1:([a-f0-9]+)\s*-->`)
	htmlCommentRE  = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// reviewNewer reports whether review a supersedes b: later submission wins, and
// a higher ID breaks ties (equal/zero timestamps) so selection is deterministic.
func reviewNewer(a, b Review) bool {
	if !a.SubmittedAt.Equal(b.SubmittedAt) {
		return a.SubmittedAt.After(b.SubmittedAt)
	}
	return a.ID > b.ID
}

func parseReviewBodyFindings(review Review, bot string) []Finding {
	body := strings.TrimSpace(review.Body)
	if body == "" {
		return nil
	}
	clean := stripMarkdownQuote(body)
	out := parseDetailedReviewFindings(clean, review, bot)
	out = append(out, parsePromptReviewFindings(clean, review, bot)...)
	return out
}

func parseDetailedReviewFindings(body string, review Review, bot string) []Finding {
	lines := strings.Split(body, "\n")
	var out []Finding
	currentPath := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if match := detailSummaryRE.FindStringSubmatch(line); match != nil {
			summary := strings.TrimSpace(match[1])
			if looksLikePath(summary) {
				currentPath = summary
			}
			continue
		}
		match := detailHeaderRE.FindStringSubmatch(line)
		if match == nil || currentPath == "" {
			continue
		}
		startLine, _ := strconv.Atoi(match[1])
		meta := strings.TrimSpace(match[3])
		if isNonActionableText(meta) {
			continue
		}
		start := i + 1
		end := len(lines)
		for j := start; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if detailHeaderRE.MatchString(next) || detailSummaryRE.MatchString(next) {
				end = j
				break
			}
		}
		block := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		title := titleFromDetailedBlock(block)
		if title == "" {
			title = titleOf(block)
		}
		bodyText := compactReviewBody(block)
		finding := Finding{
			Bot:       bot,
			Severity:  severityOf(meta + "\n" + block),
			Path:      strings.TrimPrefix(currentPath, "@"),
			Line:      startLine,
			Title:     title,
			Body:      bodyText,
			ReviewID:  review.ID,
			Commit:    shortOID(review.CommitID),
			URL:       review.HTMLURL,
			Source:    "review_body",
			CreatedAt: review.SubmittedAt,
		}
		if isActionableFinding(finding) {
			out = append(out, finding)
		}
	}
	return out
}

func parsePromptReviewFindings(body string, review Review, bot string) []Finding {
	var out []Finding
	for _, blockMatch := range promptBlockRE.FindAllStringSubmatch(body, -1) {
		block := blockMatch[1]
		lines := strings.Split(block, "\n")
		currentPath := ""
		for i := 0; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if match := promptFileRE.FindStringSubmatch(line); match != nil {
				currentPath = firstNonEmpty(match[1], match[2])
				currentPath = strings.TrimPrefix(currentPath, "@")
				continue
			}
			match := promptBulletRE.FindStringSubmatch(line)
			if match == nil || currentPath == "" {
				continue
			}
			startLine, _ := strconv.Atoi(match[1])
			parts := []string{strings.TrimSpace(match[3])}
			for j := i + 1; j < len(lines); j++ {
				next := strings.TrimSpace(lines[j])
				if next == "" {
					continue
				}
				if strings.HasPrefix(next, "---") || promptFileRE.MatchString(next) || promptBulletRE.MatchString(next) {
					break
				}
				parts = append(parts, next)
				i = j
			}
			bodyText := strings.TrimSpace(strings.Join(parts, " "))
			finding := Finding{
				Bot:       bot,
				Severity:  severityOf(bodyText),
				Path:      currentPath,
				Line:      startLine,
				Title:     titleOf(bodyText),
				Body:      bodyText,
				ReviewID:  review.ID,
				Commit:    shortOID(review.CommitID),
				URL:       review.HTMLURL,
				Source:    "review_prompt",
				CreatedAt: review.SubmittedAt,
			}
			if isActionableFinding(finding) {
				out = append(out, finding)
			}
		}
	}
	return out
}

// threadFindings turns one GitHub review thread into findings. An unresolved,
// non-outdated thread is still actionable no matter which commit its comments
// were filed on: GitHub's own resolution/outdated state is the source of truth,
// so a real finding from an earlier commit is surfaced instead of silently
// dropped when HEAD moves. (This is why callers do not need a manual
// cross-review audit.) Resolved or outdated threads are skipped.
func threadFindings(thread reviewThread, bots map[string]struct{}) []Finding {
	if thread.IsResolved || thread.IsOutdated {
		return nil
	}
	var out []Finding
	for _, comment := range thread.Comments.Nodes {
		if !inBots(bots, comment.Author.Login) {
			continue
		}
		commit := shortOID(comment.Commit.OID)
		if commit == "" {
			commit = shortOID(comment.OriginalCommit.OID)
		}
		out = append(out, Finding{
			Bot:       comment.Author.Login,
			Severity:  severityOf(comment.Body),
			Path:      firstNonEmpty(thread.Path, comment.Path),
			Line:      firstPositive(thread.Line, comment.Line, comment.OriginalLine),
			Title:     titleOf(comment.Body),
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
		if !inBots(bots, comment.Author.Login) {
			continue
		}
		path := firstNonEmpty(thread.Path, comment.Path)
		line := firstPositive(thread.Line, comment.Line, comment.OriginalLine)
		if path == "" || line <= 0 {
			continue
		}
		keys = append(keys, normalizeBotName(comment.Author.Login)+"|"+path+"|"+strconv.Itoa(line))
	}
	return keys
}

func dedupeFindings(in []Finding, suppressPromptAt map[string]bool) []Finding {
	seen := map[string]bool{}
	structuredAtLocation := map[string]bool{}
	for _, finding := range in {
		if finding.Source != "review_prompt" && finding.Path != "" && finding.Line > 0 {
			structuredAtLocation[normalizeBotName(finding.Bot)+"|"+finding.Path+"|"+strconv.Itoa(finding.Line)] = true
		}
	}
	out := []Finding{}
	for _, finding := range in {
		finding.Body = strings.TrimSpace(finding.Body)
		finding.Title = strings.TrimSpace(finding.Title)
		if !isActionableFinding(finding) {
			continue
		}
		if finding.Source == "review_prompt" {
			key := normalizeBotName(finding.Bot) + "|" + finding.Path + "|" + strconv.Itoa(finding.Line)
			if structuredAtLocation[key] || suppressPromptAt[key] {
				continue
			}
		}
		key := normalizeBotName(finding.Bot) + "|" + finding.Path + "|" + strconv.Itoa(finding.Line) + "|" + finding.Title + "|" + finding.Body + "|" + finding.ThreadID
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

func botSet(bots []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, bot := range bots {
		bot = strings.TrimSpace(bot)
		if bot != "" {
			out[bot] = struct{}{}
		}
	}
	return out
}

// inBots matches a comment author against the configured bots, tolerating the
// "[bot]" suffix: GitHub's REST API reports "coderabbitai[bot]" but GraphQL
// (review threads) reports "coderabbitai", and the config may use either form.
// Without this, crq missed every review-thread finding and so never surfaced a
// thread_id to resolve.
func inBots(bots map[string]struct{}, login string) bool {
	if _, ok := bots[login]; ok {
		return true
	}
	stripped := strings.TrimSuffix(login, "[bot]")
	if _, ok := bots[stripped]; ok {
		return true
	}
	_, ok := bots[stripped+"[bot]"]
	return ok
}

func normalizeBotName(login string) string {
	return strings.TrimSuffix(login, "[bot]")
}

// isCompletionReply reports whether body is the bot's reply to a processed
// review command (CodeRabbit: "Review finished."). An empty marker disables
// the completion-reply convergence fallback entirely.
func (s *Service) isCompletionReply(body string) bool {
	marker := strings.TrimSpace(s.cfg.CompletionMarker)
	if marker == "" {
		return false
	}
	return strings.Contains(strings.ToLower(body), strings.ToLower(marker))
}

// isAutoReply reports whether body is one of the bot's auto-generated replies
// to a command — completion, rate-limit, skip, or progress. The bot posts
// exactly one per command, which is what lets completions be paired to the
// command they answer.
func (s *Service) isAutoReply(body string) bool {
	marker := strings.TrimSpace(s.cfg.CalibrationMarker)
	if marker == "" {
		return false
	}
	return strings.Contains(strings.ToLower(body), strings.ToLower(marker))
}

// applyCompletionReplyFallback marks the configured bot reviewed when a
// no-findings re-review completed by issue-comment reply rather than a review
// object. The state anchor is mandatory because issue comments carry no commit,
// and a prior submitted review by the bot is mandatory because only a
// re-review can complete without posting a review object (see
// completionReplyForFiredCommand).
func (s *Service) applyCompletionReplyFallback(ctx context.Context, repo string, pr int, head string, report *FeedbackReport, issueComments []IssueComment, reviews []Review) {
	if !needsConfiguredBotReview(report.ReviewedBy, s.cfg.Bot) {
		return
	}
	firedAt, ok := s.completionFallbackFiredAt(ctx, repo, pr, head)
	if !ok {
		return
	}
	if s.completionReplyForFiredCommand(issueComments, reviews, firedAt) {
		markReviewed(report.ReviewedBy, s.cfg.Bot)
	}
}

func (s *Service) completionFallbackFiredAt(ctx context.Context, repo string, pr int, head string) (time.Time, bool) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return time.Time{}, false
	}
	firedAt := feedbackWaitStart(st, repo, pr, head, time.Time{})
	if wait := st.AwaitingFeedback[QueueKey(repo, pr)]; firedAt.IsZero() && wait.Head == head && !wait.StartedAt.IsZero() {
		// History is bounded and shared across the fleet, so the entry can be
		// evicted during a long wait; the live wait carries the same fire anchor.
		firedAt = wait.StartedAt
	}
	if firedAt.IsZero() {
		return time.Time{}, false
	}
	return firedAt, true
}

type reviewCommandReply struct {
	commandID  int64
	commandAt  time.Time
	completion bool
}

// completionReplyForFiredCommand reports whether the command fired at/after
// firedAt received a completion reply. A raw timestamp check is not enough: an
// earlier round still finishing can post its "Review finished" after the new
// head's command was fired, which would converge the new round before its
// review ran. Replies are paired chronologically with the earliest unanswered
// command, while submitted reviews consume the command they answered, so older
// completed commands cannot steal a later completion reply.
//
// The completion reply only stands in for a review when the bot has already
// submitted at least one review on the PR: it models a no-findings re-review
// ("nothing new since my last review"), which presupposes a prior review.
// CodeRabbit can answer the first-ever command on a PR with an instant
// "Review finished" while the real review is still queued on its side
// (observed: ack 5s after the trigger, review with 11 findings landing
// minutes later) — counting that ack converged the round with zero findings
// and cleared the feedback wait, which also let autoreview fire a duplicate
// command for the same head.
func (s *Service) completionReplyForFiredCommand(comments []IssueComment, reviews []Review, firedAt time.Time) bool {
	if !botHasAnyReview(reviews, s.cfg.Bot) {
		return false
	}
	for _, reply := range s.reviewCommandReplies(comments, reviews) {
		if reply.completion && notBefore(reply.commandAt, firedAt) {
			return true
		}
	}
	return false
}

// botHasAnyReview reports whether login has a submitted review on the PR, on
// any commit. A CodeRabbit review that actually ran always submits a review
// object ("Actionable comments posted: N"), so its absence means the PR was
// never reviewed and a completion reply cannot mean "nothing new to re-review".
func botHasAnyReview(reviews []Review, login string) bool {
	bots := botSet([]string{login})
	for _, review := range reviews {
		if inBots(bots, review.User.Login) {
			return true
		}
	}
	return false
}

func (s *Service) reviewCommandHasCompletionReply(comments []IssueComment, reviews []Review, commandID int64) bool {
	if commandID == 0 {
		return false
	}
	for _, reply := range s.reviewCommandReplies(comments, reviews) {
		if reply.commandID == commandID && reply.completion {
			return true
		}
	}
	return false
}

func (s *Service) reviewCommandReplies(comments []IssueComment, reviews []Review) []reviewCommandReply {
	command := strings.TrimSpace(s.cfg.ReviewCommand)
	if command == "" {
		return nil
	}

	type eventKind int
	const (
		eventCommand eventKind = iota
		eventAutoReply
		eventReview
	)
	type event struct {
		kind    eventKind
		at      time.Time
		id      int64
		comment IssueComment
	}

	var events []event
	for _, c := range comments {
		body := strings.TrimSpace(c.Body)
		switch {
		case body == command && !s.isConfiguredBot(c.User.Login):
			events = append(events, event{kind: eventCommand, at: commentTime(c), id: c.ID, comment: c})
		case s.isConfiguredBot(c.User.Login) && s.isAutoReply(c.Body):
			events = append(events, event{kind: eventAutoReply, at: commentTime(c), id: c.ID, comment: c})
		}
	}
	for _, review := range reviews {
		if !s.isConfiguredBot(review.User.Login) || review.SubmittedAt.IsZero() {
			continue
		}
		events = append(events, event{kind: eventReview, at: review.SubmittedAt, id: review.ID})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].at.Equal(events[j].at) {
			return events[i].at.Before(events[j].at)
		}
		if events[i].kind != events[j].kind {
			return events[i].kind < events[j].kind
		}
		return events[i].id < events[j].id
	})

	var out []reviewCommandReply
	var pending []IssueComment
	for _, ev := range events {
		switch ev.kind {
		case eventCommand:
			pending = append(pending, ev.comment)
		case eventReview:
			if len(pending) > 0 {
				pending = pending[1:]
			}
		case eventAutoReply:
			if len(pending) == 0 {
				continue
			}
			cmd := pending[0]
			pending = pending[1:]
			out = append(out, reviewCommandReply{
				commandID:  cmd.ID,
				commandAt:  commentTime(cmd),
				completion: s.isCompletionReply(ev.comment.Body) && !s.isRateLimited(ev.comment.Body),
			})
		}
	}
	return out
}

func commentTime(c IssueComment) time.Time {
	if !c.CreatedAt.IsZero() {
		return c.CreatedAt
	}
	return c.UpdatedAt
}

// needsConfiguredBotReview reports whether login gates convergence (has a
// ReviewedBy key) and its review for the head hasn't been seen yet — the only
// case where the completion-reply fallback needs to run.
func needsConfiguredBotReview(reviewedBy map[string]bool, login string) bool {
	norm := normalizeBotName(login)
	for bot, reviewed := range reviewedBy {
		if bot == login || normalizeBotName(bot) == norm {
			return !reviewed
		}
	}
	return false
}

func isCodexBot(login string) bool {
	return normalizeBotName(login) == "chatgpt-codex-connector"
}

func hasCodexBot(bots []string) bool {
	for _, bot := range bots {
		if isCodexBot(bot) {
			return true
		}
	}
	return false
}

func isCurrentCodexThumbsUp(reaction Reaction, since time.Time) bool {
	if !isCodexBot(reaction.User.Login) || reaction.Content != "+1" {
		return false
	}
	return reaction.CreatedAt.IsZero() || notBefore(reaction.CreatedAt, since)
}

// markReviewed flips the configured required-bot key that login matches to true,
// tolerating the "[bot]" suffix difference between REST ("coderabbitai[bot]") and
// GraphQL ("coderabbitai") logins. It updates the existing key rather than
// inserting the raw login, so convergence (which ANDs every key) can't be broken
// by a duplicate key that never flips true.
func markReviewed(reviewedBy map[string]bool, login string) {
	norm := normalizeBotName(login)
	for bot := range reviewedBy {
		if bot == login || normalizeBotName(bot) == norm {
			reviewedBy[bot] = true
			return
		}
	}
}

func reviewedByBot(reviewedBy map[string]bool, login string) bool {
	norm := normalizeBotName(login)
	for bot, reviewed := range reviewedBy {
		if reviewed && (bot == login || normalizeBotName(bot) == norm) {
			return true
		}
	}
	return false
}

func severityOf(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "critical"), strings.Contains(lower, "🔴"):
		return "critical"
	case strings.Contains(lower, "major"), strings.Contains(lower, "high"), strings.Contains(lower, "🟠"):
		return "major"
	case strings.Contains(lower, "potential issue"), strings.Contains(lower, "medium"), strings.Contains(lower, "🟡"):
		return "potential"
	case strings.Contains(lower, "nitpick"), strings.Contains(lower, "minor"), strings.Contains(lower, "low"), strings.Contains(lower, "🔵"):
		return "minor"
	default:
		return "unknown"
	}
}

func rankSeverity(sev string) int {
	switch sev {
	case "critical":
		return 5
	case "major":
		return 4
	case "potential":
		return 3
	case "minor":
		return 2
	default:
		return 1
	}
}

func titleOf(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "#*_` ")
		if line != "" && !strings.HasPrefix(line, "<details") && !strings.HasPrefix(line, "</") {
			if len(line) > 180 {
				line = line[:180]
			}
			return line
		}
	}
	return "Review finding"
}

func titleFromDetailedBlock(body string) string {
	if match := boldTitleRE.FindStringSubmatch(body); match != nil {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func stripMarkdownQuote(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, " \t")
		// CodeRabbit nests review-body sections (outside-diff-range,
		// duplicates, nitpicks) several blockquote levels deep — strip
		// every leading quote marker, not just the first.
		for strings.HasPrefix(line, ">") {
			line = strings.TrimPrefix(line, ">")
			line = strings.TrimPrefix(line, " ")
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func compactReviewBody(body string) string {
	body = crCommentRE.ReplaceAllString(body, "")
	body = htmlCommentRE.ReplaceAllString(body, "")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	var out []string
	skipFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			skipFence = !skipFence
			continue
		}
		if skipFence || strings.HasPrefix(trimmed, "<details") || strings.HasPrefix(trimmed, "</details") ||
			strings.HasPrefix(trimmed, "<summary") || strings.HasPrefix(trimmed, "</summary") ||
			strings.HasPrefix(trimmed, "<blockquote") || strings.HasPrefix(trimmed, "</blockquote") {
			continue
		}
		isRule := len(trimmed) >= 3 && strings.Trim(trimmed, "-_* ") == ""
		if trimmed != "" && !isRule {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n")
}

var rootFileRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

func looksLikePath(summary string) bool {
	summary = strings.TrimSpace(summary)
	if summary == "" || strings.Contains(summary, " ") {
		return false
	}
	if strings.Contains(summary, "/") || strings.Contains(summary, ".") {
		return true
	}
	// Root-level files often have neither a slash nor a dot (Dockerfile, Makefile,
	// LICENSE). In a "<file> (N)" detail summary a single filename-safe token is a
	// file, so accept it rather than dropping its findings.
	return rootFileRE.MatchString(summary)
}

func isActionableFinding(finding Finding) bool {
	title := compactReviewBody(finding.Title)
	body := compactReviewBody(finding.Body)
	if title == "" && body == "" {
		return false
	}
	text := strings.ToLower(title + "\n" + body)
	return !isNonActionableText(text)
}

func isNonActionableText(text string) bool {
	text = normalizeReviewText(text)
	if isCodexNoActionReviewCompletion(text) {
		return true
	}
	nonActionable := []string{
		"lgtm",
		"also applies to:",
		"no issue here",
		"incorrect or invalid review comment",
		"likely an incorrect or invalid review comment",
		"version claim",
		"both referenced files exist",
		"good regression test",
		"already fixed",
		"now fixed",
		"no further action is needed",
		"confirm intended ux",
		"worth confirming",
		"skipped: comment is from another github bot",
		"you have reached your codex usage limits for code reviews",
	}
	for _, phrase := range nonActionable {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func isNoActionReviewCompletion(text string) bool {
	text = normalizeReviewText(text)
	return strings.Contains(text, "no actionable comments were generated in the recent review") ||
		isCodexNoActionReviewCompletion(text)
}

func isCodexNoActionReviewCompletion(text string) bool {
	text = normalizeReviewText(text)
	return strings.Contains(text, "didn't find any major issues") &&
		strings.Contains(text, "keep them coming")
}

func normalizeReviewText(text string) string {
	return strings.NewReplacer("’", "'", "‘", "'").Replace(strings.ToLower(text))
}

func issueCommentTime(comment IssueComment) time.Time {
	if comment.UpdatedAt.After(comment.CreatedAt) {
		return comment.UpdatedAt.UTC()
	}
	return comment.CreatedAt.UTC()
}

func shortOID(oid string) string {
	if len(oid) >= 9 {
		return oid[:9]
	}
	return oid
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
