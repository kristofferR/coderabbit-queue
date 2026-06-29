package crq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Logger interface {
	Printf(string, ...any)
}

type GitHubAPI interface {
	GetPull(context.Context, string, int) (Pull, error)
	ListReviews(context.Context, string, int) ([]Review, error)
	ListIssueComments(context.Context, string, int) ([]IssueComment, error)
	ListIssueCommentsPage(context.Context, string, int, int, int) ([]IssueComment, error)
	ListReviewComments(context.Context, string, int) ([]ReviewComment, error)
	ListCommentReactions(context.Context, string, int64) ([]Reaction, error)
	PostIssueComment(context.Context, string, int, string) (IssueComment, error)
	DeleteIssueComment(context.Context, string, int64) error
	SearchOpenPRs(context.Context, string, bool, int) ([]SearchPR, error)
	EachOpenPR(context.Context, string, bool, func(SearchPR) (bool, error)) error
	GraphQL(context.Context, string, map[string]any, any) error
}

type Service struct {
	cfg   Config
	gh    GitHubAPI
	store StateStore
	log   Logger
}

func NewService(cfg Config, gh GitHubAPI, store StateStore, log Logger) *Service {
	return &Service{cfg: cfg, gh: gh, store: store, log: log}
}

type EnqueueResult struct {
	Repo          string `json:"repo"`
	PR            int    `json:"pr"`
	Queued        bool   `json:"queued"`
	AlreadyQueued bool   `json:"already_queued"`
	Deduped       bool   `json:"deduped"`
	Head          string `json:"head,omitempty"`
	Seq           int64  `json:"seq,omitempty"`
}

func (s *Service) Enqueue(ctx context.Context, repo string, pr int) (EnqueueResult, error) {
	repo = NormalizeRepo(repo)
	result := EnqueueResult{Repo: repo, PR: pr}
	state, err := s.store.Update(ctx, func(state *State) error {
		if state.Contains(repo, pr) {
			result.AlreadyQueued = true
			return ErrNoChange
		}
		key := QueueKey(repo, pr)
		if fired := state.Fired[key]; fired != "" {
			head, err := s.headShort(ctx, repo, pr)
			if err == nil && head == fired {
				result.Deduped = true
				result.Head = head
				return ErrNoChange
			}
		}
		state.NextSeq++
		item := QueueItem{
			Seq:        state.NextSeq,
			Owner:      ownerOf(repo),
			Repo:       repo,
			PR:         pr,
			Host:       s.cfg.Host,
			EnqueuedAt: time.Now().UTC(),
		}
		state.Queue = append(state.Queue, item)
		result.Queued = true
		result.Seq = item.Seq
		return nil
	})
	if err != nil {
		return result, err
	}
	s.sync(ctx, state)
	return result, nil
}

// enqueueBatch appends several PRs to the queue in a single compare-and-swap
// write plus one dashboard sync, so a large autoreview pass doesn't produce N
// separate state writes / issue edits (the write-storm in #2). PRs already
// queued or in flight are skipped; the fired-head dedup still happens at pump
// time, so a stale candidate can't cause a double review.
func (s *Service) enqueueBatch(ctx context.Context, items []SearchPR) error {
	if len(items) == 0 {
		return nil
	}
	state, err := s.store.Update(ctx, func(st *State) error {
		added := 0
		for _, it := range items {
			repo := NormalizeRepo(it.Repo)
			if st.Contains(repo, it.Number) {
				continue
			}
			st.NextSeq++
			st.Queue = append(st.Queue, QueueItem{
				Seq:        st.NextSeq,
				Owner:      ownerOf(repo),
				Repo:       repo,
				PR:         it.Number,
				Host:       s.cfg.Host,
				EnqueuedAt: time.Now().UTC(),
			})
			added++
		}
		if added == 0 {
			return ErrNoChange
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.sync(ctx, state)
	return nil
}

type PumpResult struct {
	Action string `json:"action"`
	Repo   string `json:"repo,omitempty"`
	PR     int    `json:"pr,omitempty"`
	Head   string `json:"head,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (s *Service) Pump(ctx context.Context) (PumpResult, error) {
	if state, _, err := s.store.Load(ctx); err == nil && state.InFlight != nil {
		status, err := s.inflightStatus(ctx, state)
		if err != nil {
			return PumpResult{}, err
		}
		if status.Done || status.Requeue {
			updated, err := s.store.Update(ctx, func(st *State) error {
				if st.InFlight == nil || st.InFlight.Token != state.InFlight.Token {
					return nil
				}
				if status.Requeue {
					s.requeueInflight(st, status)
				} else {
					st.InFlight = nil
					st.Warn = ""
				}
				return nil
			})
			if err != nil {
				return PumpResult{}, err
			}
			s.sync(ctx, updated)
			if status.Requeue {
				return PumpResult{Action: "requeued", Repo: state.InFlight.Repo, PR: state.InFlight.PR, Reason: status.Reason}, nil
			}
			return PumpResult{Action: "cleared", Repo: state.InFlight.Repo, PR: state.InFlight.PR, Reason: status.Reason}, nil
		}
		return PumpResult{Action: "waiting", Repo: state.InFlight.Repo, PR: state.InFlight.PR, Reason: "review in flight"}, nil
	}

	state, _, err := s.store.Load(ctx)
	if err != nil {
		return PumpResult{}, err
	}
	queue := state.SortedQueue()
	if len(queue) == 0 {
		return PumpResult{Action: "idle"}, nil
	}
	if refreshed, err := s.RefreshQuota(ctx); err == nil {
		state = refreshed
	} else {
		return PumpResult{}, err
	}
	now := time.Now().UTC()
	if state.Blocked.BlockedUntil != nil && state.Blocked.BlockedUntil.After(now) {
		return PumpResult{Action: "blocked", Reason: state.Blocked.BlockedUntil.Format(time.RFC3339)}, nil
	}
	if state.LastFired != nil && now.Sub(*state.LastFired) < s.cfg.MinInterval {
		return PumpResult{Action: "min_interval", Reason: s.cfg.MinInterval.String()}, nil
	}
	queue = state.SortedQueue()
	if len(queue) == 0 {
		return PumpResult{Action: "idle"}, nil
	}
	item := queue[0]
	head, open, err := s.pullHead(ctx, item.Repo, item.PR)
	if err != nil {
		return PumpResult{}, err
	}
	if !open {
		// PR was closed or merged after queueing — drop it rather than fire a review
		// at a dead PR that can never converge.
		updated, uerr := s.store.Update(ctx, func(st *State) error {
			removeQueued(st, item.Seq)
			return nil
		})
		if uerr != nil {
			return PumpResult{}, uerr
		}
		s.sync(ctx, updated)
		return PumpResult{Action: "skipped", Repo: item.Repo, PR: item.PR, Reason: "pr closed"}, nil
	}
	if !isShortSHA(head) {
		return PumpResult{Action: "skipped", Repo: item.Repo, PR: item.PR, Reason: "could not read head"}, nil
	}
	key := QueueKey(item.Repo, item.PR)
	if state.Fired[key] == head {
		updated, err := s.store.Update(ctx, func(st *State) error {
			removeQueued(st, item.Seq)
			return nil
		})
		if err != nil {
			return PumpResult{}, err
		}
		s.sync(ctx, updated)
		return PumpResult{Action: "deduped", Repo: item.Repo, PR: item.PR, Head: head}, nil
	}
	if reviewed, err := s.botReviewedHead(ctx, item.Repo, item.PR, head); err == nil && reviewed {
		updated, err := s.store.Update(ctx, func(st *State) error {
			removeQueued(st, item.Seq)
			st.Fired[key] = head
			return nil
		})
		if err != nil {
			return PumpResult{}, err
		}
		s.sync(ctx, updated)
		return PumpResult{Action: "deduped", Repo: item.Repo, PR: item.PR, Head: head, Reason: "bot already reviewed head"}, nil
	} else if err != nil {
		return PumpResult{}, err
	}

	if s.cfg.DryRun {
		return PumpResult{Action: "dry_run", Repo: item.Repo, PR: item.PR, Head: head}, nil
	}

	token := randomToken()
	reserved, err := s.store.Update(ctx, func(st *State) error {
		if st.InFlight != nil {
			return nil
		}
		q := st.SortedQueue()
		if len(q) == 0 || q[0].Seq != item.Seq {
			return ErrCASConflict
		}
		removeQueued(st, item.Seq)
		st.InFlight = &InFlight{
			Seq:        item.Seq,
			Repo:       item.Repo,
			PR:         item.PR,
			Head:       head,
			Token:      token,
			Phase:      "reserved",
			ReservedAt: now,
			ByHost:     s.cfg.Host,
		}
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	s.sync(ctx, reserved)
	if reserved.InFlight == nil || reserved.InFlight.Token != token {
		return PumpResult{Action: "lost_race"}, nil
	}
	comment, err := s.gh.PostIssueComment(ctx, item.Repo, item.PR, s.cfg.ReviewCommand)
	if err != nil {
		updated, uerr := s.store.Update(ctx, func(st *State) error {
			if st.InFlight == nil || st.InFlight.Token != token {
				return nil
			}
			st.Queue = append(st.Queue, item)
			st.InFlight = nil
			st.Warn = "failed to post review command: " + err.Error()
			return nil
		})
		if uerr == nil {
			s.sync(ctx, updated)
		}
		return PumpResult{Action: "post_failed", Repo: item.Repo, PR: item.PR, Head: head, Reason: err.Error()}, err
	}
	firedAt := time.Now().UTC()
	updated, err := s.store.Update(ctx, func(st *State) error {
		if st.InFlight == nil || st.InFlight.Token != token {
			return nil
		}
		st.InFlight.Phase = "posted"
		st.InFlight.FiredAt = &firedAt
		st.InFlight.FiredCommentID = comment.ID
		st.LastFired = &firedAt
		st.Warn = ""
		st.Fired[key] = head
		st.History = append([]HistoryItem{{
			Repo:   item.Repo,
			PR:     item.PR,
			Commit: head,
			At:     firedAt,
			Host:   s.cfg.Host,
		}}, st.History...)
		if len(st.History) > 20 {
			st.History = st.History[:20]
		}
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	s.sync(ctx, updated)
	return PumpResult{Action: "fired", Repo: item.Repo, PR: item.PR, Head: head}, nil
}

func (s *Service) Wait(ctx context.Context, repo string, pr int) (PumpResult, int, error) {
	repo = NormalizeRepo(repo)
	start := time.Now()
	enqueued := false
	var lastLog time.Time
	for {
		if s.cfg.WaitTimeout > 0 && time.Since(start) > s.cfg.WaitTimeout {
			return PumpResult{Action: "timeout", Repo: repo, PR: pr}, 2, nil
		}
		if !enqueued {
			result, err := s.Enqueue(ctx, repo, pr)
			if err != nil {
				return PumpResult{}, 1, err
			}
			enqueued = result.Queued || result.AlreadyQueued
			if result.Deduped {
				return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
			}
		}
		result, err := s.Pump(ctx)
		if err != nil {
			return PumpResult{}, 1, err
		}
		state, _, err := s.store.Load(ctx)
		if err != nil {
			return PumpResult{}, 1, err
		}
		if state.InFlight != nil && state.InFlight.Repo == repo && state.InFlight.PR == pr && state.InFlight.Phase == "posted" {
			return PumpResult{Action: "fired", Repo: repo, PR: pr, Head: state.InFlight.Head}, 0, nil
		}
		if !state.Contains(repo, pr) {
			head, herr := s.headShort(ctx, repo, pr)
			if herr == nil && state.Fired[QueueKey(repo, pr)] == head {
				return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: head}, 3, nil
			}
			if result.Action == "fired" && result.Repo == repo && result.PR == pr {
				return result, 0, nil
			}
		}
		if s.log != nil && time.Since(lastLog) >= 30*time.Second {
			reason := result.Reason
			if reason == "" {
				reason = result.Action
			}
			s.log.Printf("crq: %s#%d waiting for a review slot — %s (%s elapsed)", repo, pr, reason, time.Since(start).Round(time.Second))
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return PumpResult{}, 1, ctx.Err()
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

func (s *Service) Cancel(ctx context.Context, repo string, pr int) error {
	repo = NormalizeRepo(repo)
	state, err := s.store.Update(ctx, func(st *State) error {
		for i := 0; i < len(st.Queue); i++ {
			if st.Queue[i].Repo == repo && st.Queue[i].PR == pr {
				st.Queue = append(st.Queue[:i], st.Queue[i+1:]...)
				i--
			}
		}
		if st.InFlight != nil && st.InFlight.Repo == repo && st.InFlight.PR == pr {
			st.InFlight = nil
		}
		delete(st.Fired, QueueKey(repo, pr))
		return nil
	})
	if err != nil {
		return err
	}
	s.sync(ctx, state)
	return nil
}

func (s *Service) Status(ctx context.Context) (State, string, error) {
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return State{}, "", err
	}
	return state, renderDashboard(state, s.cfg), nil
}

// warnRateLimited is the inflight requeue reason for a rate-limited fire. It is
// surfaced via the Blocked state, not the sticky Warn field.
const warnRateLimited = "rate limited"

func (s *Service) RefreshQuota(ctx context.Context) (State, error) {
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return State{}, err
	}
	if s.cfg.CalibrationPR <= 0 {
		return state, nil
	}
	now := time.Now().UTC()
	if state.Blocked.CheckedAt != nil && now.Sub(*state.Blocked.CheckedAt) < s.cfg.CalibrationTTL {
		return state, nil
	}
	blocked, err := s.readQuota(ctx, now)
	if err != nil {
		return state, err
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		if st.Blocked.CheckedAt != nil && time.Since(*st.Blocked.CheckedAt) < s.cfg.CalibrationTTL {
			return ErrNoChange
		}
		st.Blocked = blocked
		// Clear a stale "rate limited" warning once the window has passed, so the
		// dashboard can't show both "not currently limited" and a rate-limit warn.
		if st.Warn == warnRateLimited && (blocked.BlockedUntil == nil || !blocked.BlockedUntil.After(now)) {
			st.Warn = ""
		}
		return nil
	})
	if err != nil {
		return State{}, err
	}
	s.sync(ctx, updated)
	return updated, nil
}

func (s *Service) readQuota(ctx context.Context, now time.Time) (Blocked, error) {
	blocked := Blocked{Scope: strings.Join(s.cfg.Scope, ","), Source: "calibrate", CheckedAt: &now}
	cutoff := now.Add(-s.cfg.CalibrationTTL)
	keepAfter := now.Add(-2 * s.cfg.CalibrationTTL)
	if reply, ok, err := s.latestCalibrationReply(ctx, cutoff); err != nil {
		return blocked, err
	} else if ok {
		remaining, reset := parseQuota(reply.Body, reply.UpdatedAt)
		blocked.Remaining = remaining
		blocked.BlockedUntil = reset
		s.pruneCalibration(ctx, keepAfter, 80)
		return blocked, nil
	}
	asked, err := s.gh.PostIssueComment(ctx, s.cfg.GateRepo, s.cfg.CalibrationPR, s.cfg.RateLimitCommand)
	if err != nil {
		// The calibration PR hit GitHub's 2500-comment cap; prune our old probe
		// comments to drop back under it, then retry once.
		if isCommentCapError(err) {
			if pruned := s.pruneCalibration(ctx, keepAfter, 100); pruned > 0 {
				asked, err = s.gh.PostIssueComment(ctx, s.cfg.GateRepo, s.cfg.CalibrationPR, s.cfg.RateLimitCommand)
			}
		}
		if err != nil {
			return blocked, err
		}
	}
	blocked.CalibAskedAt = &asked.CreatedAt
	for i := 0; i < 6; i++ {
		select {
		case <-ctx.Done():
			return blocked, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		reply, ok, err := s.latestCalibrationReply(ctx, asked.CreatedAt.Add(-time.Second))
		if err != nil {
			return blocked, err
		}
		if ok {
			remaining, reset := parseQuota(reply.Body, reply.UpdatedAt)
			blocked.Remaining = remaining
			blocked.BlockedUntil = reset
			blocked.CalibAskedAt = nil
			s.pruneCalibration(ctx, keepAfter, 80)
			return blocked, nil
		}
	}
	return blocked, nil
}

// pruneCalibration deletes crq's old calibration probe comments and CodeRabbit's
// replies from the calibration PR so it never reaches GitHub's hard 2500-comment
// cap (which silently wedges the whole queue). It reads only the oldest page and
// deletes up to max comments older than keepAfter, so cost stays bounded and the
// most recent reading is preserved.
func (s *Service) pruneCalibration(ctx context.Context, keepAfter time.Time, max int) int {
	if s.cfg.CalibrationPR <= 0 || max <= 0 {
		return 0
	}
	comments, err := s.gh.ListIssueCommentsPage(ctx, s.cfg.GateRepo, s.cfg.CalibrationPR, 1, 100)
	if err != nil {
		if s.log != nil {
			s.log.Printf("calibration prune: list failed: %v", err)
		}
		return 0
	}
	deleted := 0
	for _, c := range comments {
		if deleted >= max {
			break
		}
		if c.CreatedAt.After(keepAfter) || c.UpdatedAt.After(keepAfter) {
			continue
		}
		if !s.isCalibrationNoise(c) {
			continue
		}
		if err := s.gh.DeleteIssueComment(ctx, s.cfg.GateRepo, c.ID); err != nil {
			if s.log != nil {
				s.log.Printf("calibration prune: delete %d failed: %v", c.ID, err)
			}
			break
		}
		deleted++
	}
	if deleted > 0 && s.log != nil {
		s.log.Printf("calibration prune: removed %d old comment(s) from PR #%d", deleted, s.cfg.CalibrationPR)
	}
	return deleted
}

// isCalibrationNoise reports whether a comment is a spent calibration artifact:
// one of crq's "@coderabbitai rate limit" probes or a CodeRabbit auto-reply.
func (s *Service) isCalibrationNoise(c IssueComment) bool {
	if strings.TrimSpace(c.Body) == strings.TrimSpace(s.cfg.RateLimitCommand) {
		return true
	}
	return c.User.Login == s.cfg.Bot && strings.Contains(c.Body, s.cfg.CalibrationMarker)
}

func (s *Service) latestCalibrationReply(ctx context.Context, after time.Time) (IssueComment, bool, error) {
	comments, err := s.gh.ListIssueComments(ctx, s.cfg.GateRepo, s.cfg.CalibrationPR)
	if err != nil {
		return IssueComment{}, false, err
	}
	var best IssueComment
	ok := false
	for _, comment := range comments {
		if comment.User.Login != s.cfg.Bot || !comment.UpdatedAt.After(after) {
			continue
		}
		if !strings.Contains(comment.Body, s.cfg.CalibrationMarker) {
			continue
		}
		if !ok || comment.UpdatedAt.After(best.UpdatedAt) {
			best = comment
			ok = true
		}
	}
	return best, ok, nil
}

type inflightCheck struct {
	Done         bool
	Requeue      bool
	Reason       string
	BlockedUntil *time.Time
}

func (s *Service) inflightStatus(ctx context.Context, state State) (inflightCheck, error) {
	inf := state.InFlight
	if inf == nil {
		return inflightCheck{Done: true, Reason: "none"}, nil
	}
	if inf.Phase == "reserved" && time.Since(inf.ReservedAt) > 2*time.Minute {
		return inflightCheck{Requeue: true, Reason: "reserved review was never posted"}, nil
	}
	if inf.FiredAt == nil {
		return inflightCheck{}, nil
	}
	comments, err := s.gh.ListIssueComments(ctx, inf.Repo, inf.PR)
	if err != nil {
		return inflightCheck{}, err
	}
	for _, comment := range comments {
		if comment.User.Login != s.cfg.Bot || !comment.UpdatedAt.After(*inf.FiredAt) {
			continue
		}
		if s.isRateLimited(comment.Body) {
			reset := parseAvailableIn(comment.Body, comment.UpdatedAt)
			return inflightCheck{Requeue: true, Reason: warnRateLimited, BlockedUntil: reset}, nil
		}
	}
	reviews, err := s.gh.ListReviews(ctx, inf.Repo, inf.PR)
	if err != nil {
		return inflightCheck{}, err
	}
	for _, review := range reviews {
		if review.User.Login == s.cfg.Bot && review.SubmittedAt.After(*inf.FiredAt) {
			return inflightCheck{Done: true, Reason: "review submitted"}, nil
		}
	}
	if inf.FiredCommentID != 0 {
		reactions, err := s.gh.ListCommentReactions(ctx, inf.Repo, inf.FiredCommentID)
		if err == nil {
			for _, reaction := range reactions {
				if reaction.User.Login == s.cfg.Bot {
					return inflightCheck{Done: true, Reason: "bot reacted"}, nil
				}
			}
		}
	}
	for _, comment := range comments {
		if comment.User.Login == s.cfg.Bot && comment.ID != inf.FiredCommentID && comment.UpdatedAt.After(*inf.FiredAt) && !s.isRateLimited(comment.Body) {
			return inflightCheck{Done: true, Reason: "bot comment"}, nil
		}
	}
	if time.Since(*inf.FiredAt) > s.cfg.InflightTimeout {
		return inflightCheck{Requeue: true, Reason: "in-flight timeout"}, nil
	}
	return inflightCheck{}, nil
}

func (s *Service) requeueInflight(st *State, status inflightCheck) {
	if st.InFlight == nil {
		return
	}
	inf := *st.InFlight
	st.Queue = append(st.Queue, QueueItem{
		Seq:        inf.Seq,
		Owner:      ownerOf(inf.Repo),
		Repo:       inf.Repo,
		PR:         inf.PR,
		Host:       inf.ByHost,
		EnqueuedAt: time.Now().UTC(),
	})
	sort.Slice(st.Queue, func(i, j int) bool { return st.Queue[i].Seq < st.Queue[j].Seq })
	delete(st.Fired, QueueKey(inf.Repo, inf.PR))
	st.InFlight = nil
	now := time.Now().UTC()
	if status.Reason == warnRateLimited {
		// A rate limit is shown by the Blocked state (the dashboard's Rate-limit
		// row), not a sticky Warn — otherwise once the window passes the table says
		// "not currently limited" while a stale "rate limited" warning lingers.
		until := status.BlockedUntil
		if until == nil || !until.After(now) {
			t := now.Add(s.cfg.CalibrationTTL) // no parseable reset; re-calibrate soon
			until = &t
		}
		zero := 0
		st.Blocked.BlockedUntil = until
		st.Blocked.Remaining = &zero
		st.Blocked.Source = "warning"
		st.Blocked.CheckedAt = &now
		st.Warn = ""
	} else {
		st.Warn = status.Reason
	}
}

func (s *Service) headShort(ctx context.Context, repo string, pr int) (string, error) {
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return "", err
	}
	if len(pull.Head.SHA) < 9 {
		return "", fmt.Errorf("invalid head sha")
	}
	return pull.Head.SHA[:9], nil
}

// pullHead returns the PR's short head SHA and whether it is still open (neither
// closed nor merged). Pump uses it so a PR that was closed or merged after it was
// queued is dropped instead of having a review fired at a dead PR — which would
// never converge, time out, and requeue forever, wasting the shared slot.
func (s *Service) pullHead(ctx context.Context, repo string, pr int) (head string, open bool, err error) {
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return "", false, err
	}
	open = pull.State == "open" && !pull.Merged
	if len(pull.Head.SHA) < 9 {
		return "", open, fmt.Errorf("invalid head sha")
	}
	return pull.Head.SHA[:9], open, nil
}

func (s *Service) botReviewedHead(ctx context.Context, repo string, pr int, head string) (bool, error) {
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	for _, review := range reviews {
		if review.User.Login == s.cfg.Bot && strings.HasPrefix(review.CommitID, head) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) sync(ctx context.Context, state State) {
	if s.log == nil || s.cfg.DashboardIssue <= 0 {
		return
	}
	if err := s.store.SyncDashboard(ctx, state); err != nil {
		s.log.Printf("warning: dashboard sync failed: %v", err)
	}
}

func removeQueued(st *State, seq int64) {
	for i := range st.Queue {
		if st.Queue[i].Seq == seq {
			st.Queue = append(st.Queue[:i], st.Queue[i+1:]...)
			return
		}
	}
}

func isShortSHA(value string) bool {
	if len(value) < 7 || len(value) > 40 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func randomToken() string {
	var buf [16]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// isRateLimited reports whether a CodeRabbit comment is a rate-limit notice. It
// matches the configured CRQ_RL_MARKER plus CodeRabbit's current phrasings (the
// "Fair Usage Limits Policy" / "currently rate limited" message), which the old
// "rate limited by coderabbit.ai" marker alone misses — so a fired review that
// comes back rate-limited is detected and crq backs off instead of firing on.
func (s *Service) isRateLimited(body string) bool {
	l := strings.ToLower(body)
	if m := strings.ToLower(strings.TrimSpace(s.cfg.RateLimitMarker)); m != "" && strings.Contains(l, m) {
		return true
	}
	return strings.Contains(l, "currently rate limited") ||
		strings.Contains(l, "rate limited under") ||
		strings.Contains(l, "fair usage limits policy")
}

func parseAvailableIn(text string, base time.Time) *time.Time {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "available in ")
	if idx < 0 {
		return nil
	}
	frag := lower[idx+len("available in "):]
	if dot := strings.Index(frag, "."); dot >= 0 {
		frag = frag[:dot]
	}
	fields := strings.Fields(frag)
	var d time.Duration
	for i := 0; i+1 < len(fields); i++ {
		n, err := strconv.Atoi(strings.Trim(fields[i], ","))
		if err != nil {
			continue
		}
		unit := strings.Trim(fields[i+1], ",")
		switch {
		case strings.HasPrefix(unit, "hour"):
			d += time.Duration(n) * time.Hour
		case strings.HasPrefix(unit, "minute"):
			d += time.Duration(n) * time.Minute
		case strings.HasPrefix(unit, "second"):
			d += time.Duration(n) * time.Second
		}
	}
	if d == 0 {
		return nil
	}
	t := base.Add(d)
	return &t
}

func parseQuota(text string, base time.Time) (*int, *time.Time) {
	remaining := parseRemainingReviews(text)
	reset := parseAvailableIn(text, base)
	return remaining, reset
}

func parseRemainingReviews(text string) *int {
	lower := strings.ToLower(text)
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	for i := 0; i < len(words); i++ {
		n, err := strconv.Atoi(words[i])
		if err != nil {
			continue
		}
		if i+2 < len(words) && strings.HasPrefix(words[i+1], "review") && (words[i+2] == "remaining" || words[i+2] == "left") {
			return &n
		}
		if i > 0 && (words[i-1] == "remaining" || words[i-1] == "left") {
			return &n
		}
	}
	return nil
}
