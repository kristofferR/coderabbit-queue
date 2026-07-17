package crq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

type Logger interface {
	Printf(string, ...any)
}

type GitHubAPI interface {
	GetPull(context.Context, string, int) (Pull, error)
	GetCommit(context.Context, string, string) (gitCommit, error)
	ListReviews(context.Context, string, int) ([]Review, error)
	ListIssueComments(context.Context, string, int) ([]IssueComment, error)
	ListIssueCommentsPage(context.Context, string, int, int, int) ([]IssueComment, error)
	ListReviewComments(context.Context, string, int) ([]ReviewComment, error)
	ListIssueReactions(context.Context, string, int) ([]Reaction, error)
	ListCommentReactions(context.Context, string, int64) ([]Reaction, error)
	PostIssueComment(context.Context, string, int, string) (IssueComment, error)
	DeleteIssueComment(context.Context, string, int64) error
	CreateIssue(context.Context, string, string, string) (Issue, error)
	SearchOpenPRs(context.Context, string, bool, int) ([]SearchPR, error)
	EachOpenPR(context.Context, string, bool, func(SearchPR) (bool, error)) error
	GraphQL(context.Context, string, map[string]any, any) error
}

type Service struct {
	cfg   Config
	cr    dialect.CodeRabbit
	gh    GitHubAPI
	store StateStore
	log   Logger
}

func NewService(cfg Config, gh GitHubAPI, store StateStore, log Logger) *Service {
	cr := dialect.CodeRabbit{
		CompletionMarker:  cfg.CompletionMarker,
		RateLimitMarker:   cfg.RateLimitMarker,
		CalibrationMarker: cfg.CalibrationMarker,
	}
	return &Service{cfg: cfg, cr: cr, gh: gh, store: store, log: log}
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
		head := ""
		if wait := state.AwaitingFeedback[key]; wait.Head != "" {
			var err error
			head, err = s.headShort(ctx, repo, pr)
			if err == nil && head == wait.Head {
				result.Deduped = true
				result.Head = head
				return ErrNoChange
			}
		}
		if fired := state.Fired[key]; fired != "" {
			var err error
			if head == "" {
				head, err = s.headShort(ctx, repo, pr)
			}
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
					// The review round is over. If every required bot's feedback
					// arrived, the wait is satisfied — clear it here, because in
					// autoreview flows no Loop is running to call clearFeedbackWait
					// and the entry would linger forever. A bare acknowledgement
					// (bot reacted) or an outstanding required bot means reviewing
					// is still underway, so that wait stays until feedback lands or
					// its deadline expires.
					if status.FeedbackComplete {
						key := QueueKey(st.InFlight.Repo, st.InFlight.PR)
						if wait := st.AwaitingFeedback[key]; wait.Head == st.InFlight.Head {
							delete(st.AwaitingFeedback, key)
						}
					}
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
	if pruned := s.pruneExpiredWaits(ctx, state); pruned != nil {
		state = *pruned
	}
	state = s.sweepFeedbackWaits(ctx, state)
	queue := state.SortedQueue()
	if len(queue) == 0 {
		return PumpResult{Action: "idle"}, nil
	}
	// Terminal PR cleanup is independent of review quota and pacing. Check the
	// queue head before either gate so a PR merged while CodeRabbit is blocked (or
	// while MinInterval is active) leaves the queue on the next pump instead of
	// lingering until another review slot becomes available.
	item := queue[0]
	if _, open, err := s.pullHead(ctx, item.Repo, item.PR); err != nil {
		return PumpResult{}, err
	} else if !open {
		return s.dropClosedQueueItem(ctx, item)
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
	item = queue[0]
	head, open, err := s.pullHead(ctx, item.Repo, item.PR)
	if err != nil {
		return PumpResult{}, err
	}
	if s.cfg.DryRun {
		// A dry-run pump only reports the action it would take. Every branch
		// below this point mutates persisted state (dropping closed PRs,
		// deduping reviewed heads, adopting commands, firing), so simulate the
		// same decisions read-only instead of falling through to them.
		key := QueueKey(item.Repo, item.PR)
		switch {
		case !open:
			return PumpResult{Action: "skipped", Repo: item.Repo, PR: item.PR, Reason: "pr closed"}, nil
		case !isShortSHA(head):
			return PumpResult{Action: "skipped", Repo: item.Repo, PR: item.PR, Reason: "could not read head"}, nil
		case state.Fired[key] == head || state.AwaitingFeedback[key].Head == head:
			return PumpResult{Action: "deduped", Repo: item.Repo, PR: item.PR, Head: head}, nil
		}
		if reviewed, err := s.botReviewedHead(ctx, item.Repo, item.PR, head); err == nil && reviewed {
			return PumpResult{Action: "deduped", Repo: item.Repo, PR: item.PR, Head: head, Reason: "bot already reviewed head"}, nil
		} else if err != nil {
			return PumpResult{}, err
		}
		return PumpResult{Action: "dry_run", Repo: item.Repo, PR: item.PR, Head: head}, nil
	}
	if !open {
		return s.dropClosedQueueItem(ctx, item)
	}
	if !isShortSHA(head) {
		return PumpResult{Action: "skipped", Repo: item.Repo, PR: item.PR, Reason: "could not read head"}, nil
	}
	key := QueueKey(item.Repo, item.PR)
	pending := state.AwaitingFeedback[key]
	if state.Fired[key] == head || pending.Head == head {
		deduped := false
		updated, err := s.store.Update(ctx, func(st *State) error {
			deduped = false
			q := st.SortedQueue()
			if len(q) == 0 || q[0].Seq != item.Seq {
				return ErrNoChange
			}
			currentPending := st.AwaitingFeedback[key]
			if st.Fired[key] != head && currentPending.Head != head {
				return ErrNoChange
			}
			removeQueued(st, item.Seq)
			if currentPending.Head == head {
				st.Fired[key] = head
			}
			deduped = true
			return nil
		})
		if err != nil {
			return PumpResult{}, err
		}
		if !deduped {
			return PumpResult{Action: "lost_race"}, nil
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
	if existing, ok, err := s.existingReviewCommand(ctx, item.Repo, item.PR, head, item.adoptCutoff()); err != nil {
		return PumpResult{}, err
	} else if ok {
		firedAt := existing.CreatedAt.UTC()
		if firedAt.IsZero() {
			firedAt = existing.UpdatedAt.UTC()
		}
		if firedAt.IsZero() {
			firedAt = time.Now().UTC()
		}
		updated, err := s.recordExistingReviewPosted(ctx, item, head, existing.ID, firedAt)
		if err != nil {
			if errors.Is(err, ErrNoChange) {
				return PumpResult{Action: "lost_race"}, nil
			}
			return PumpResult{}, err
		}
		s.sync(ctx, updated)
		if s.log != nil {
			s.log.Printf("fire %s@%s (adopted existing review command)", key, head)
		}
		return PumpResult{Action: "fired", Repo: item.Repo, PR: item.PR, Head: head, Reason: "review command already posted"}, nil
	}

	token := randomToken()
	reserved, err := s.store.Update(ctx, func(st *State) error {
		// Another worker already holds an in-flight slot, or won the race for this
		// queue head (or it was cancelled) since we picked it. These are benign lost
		// races, not write conflicts — return ErrNoChange so Update reports lost_race
		// rather than failing the loop with "state changed while writing".
		if st.InFlight != nil {
			return ErrNoChange
		}
		q := st.SortedQueue()
		if len(q) == 0 || q[0].Seq != item.Seq {
			return ErrNoChange
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
	// Baseline completion detection on the trigger comment's GitHub timestamp, not a
	// local clock that may run ahead of GitHub's: a completion landing in the same
	// second (or before a fast local clock) would otherwise fail the strict After
	// check in inflightStatus and get missed, refiring a duplicate review.
	firedAt := comment.CreatedAt.UTC()
	if firedAt.IsZero() {
		firedAt = time.Now().UTC()
	}
	updated, err := s.markReviewPosted(ctx, token, item, head, comment.ID, firedAt)
	if err != nil {
		retryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		updated, err = s.markReviewPosted(retryCtx, token, item, head, comment.ID, firedAt)
		if err != nil {
			if errors.Is(err, ErrNoChange) {
				return PumpResult{Action: "lost_race"}, nil
			}
			return PumpResult{}, err
		}
	}
	s.sync(ctx, updated)
	if s.log != nil {
		s.log.Printf("fire %s@%s (posted %s)", key, head, strings.TrimSpace(s.cfg.ReviewCommand))
	}
	return PumpResult{Action: "fired", Repo: item.Repo, PR: item.PR, Head: head}, nil
}

// dropClosedQueueItem removes a closed or merged queue entry without consuming
// review readiness. The sequence check makes a concurrent cancel/pump a benign
// lost race instead of writing a no-op state revision.
func (s *Service) dropClosedQueueItem(ctx context.Context, item QueueItem) (PumpResult, error) {
	result := PumpResult{Action: "skipped", Repo: item.Repo, PR: item.PR, Reason: "pr closed"}
	if s.cfg.DryRun {
		return result, nil
	}
	removed := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		removed = false
		for _, queued := range st.Queue {
			if queued.Seq == item.Seq {
				removeQueued(st, item.Seq)
				removed = true
				return nil
			}
		}
		return ErrNoChange
	})
	if err != nil {
		return PumpResult{}, err
	}
	if !removed {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, updated)
	return result, nil
}

func (s *Service) markReviewPosted(ctx context.Context, token string, item QueueItem, head string, commentID int64, firedAt time.Time) (State, error) {
	key := QueueKey(item.Repo, item.PR)
	recorded := false
	state, err := s.store.Update(ctx, func(st *State) error {
		recorded = false
		if st.InFlight == nil || st.InFlight.Token != token {
			return ErrNoChange
		}
		recorded = true
		st.InFlight.Phase = "posted"
		st.InFlight.FiredAt = &firedAt
		st.InFlight.FiredCommentID = commentID
		st.LastFired = &firedAt
		st.Warn = ""
		st.Fired[key] = head
		if st.AwaitingFeedback == nil {
			st.AwaitingFeedback = map[string]FeedbackWait{}
		}
		st.AwaitingFeedback[key] = s.newFeedbackWait(item.Repo, item.PR, head, firedAt, commentID)
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
		return State{}, err
	}
	if !recorded {
		return state, ErrNoChange
	}
	return state, nil
}

// existingReviewCommand looks for a review command already posted at the PR's
// current head so Pump can adopt it instead of posting a duplicate. notBefore
// bounds adoption: comments created before it (e.g. the stale command left
// behind by a requeued fire) are never adopted — re-adopting one would replay
// the very rate-limit reply or timeout that caused the requeue, looping forever
// without ever posting a fresh command.
func (s *Service) existingReviewCommand(ctx context.Context, repo string, pr int, expectedHead string, notBefore time.Time) (IssueComment, bool, error) {
	comments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		return IssueComment{}, false, err
	}
	command := strings.TrimSpace(s.cfg.ReviewCommand)
	if command == "" {
		return IssueComment{}, false, nil
	}
	// The head-guard and cutoff lookups below cost REST/GraphQL calls; in the
	// common case — no command comment on the PR at all — skip them entirely.
	hasCandidate := false
	for _, comment := range comments {
		if strings.TrimSpace(comment.Body) == command {
			hasCandidate = true
			break
		}
	}
	if !hasCandidate {
		return IssueComment{}, false, nil
	}
	cutoff := notBefore
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return IssueComment{}, false, err
	}
	if pull.Head.SHA != "" {
		if shortOID(pull.Head.SHA) != expectedHead {
			return IssueComment{}, false, nil
		}
		commit, err := s.gh.GetCommit(ctx, repo, pull.Head.SHA)
		if err != nil {
			if _, ok := rateLimitWait(err); ok {
				return IssueComment{}, false, err
			}
			// No head-commit cutoff available (e.g. an unreadable or 404 head):
			// skip adoption rather than wedging the queue on this PR — the worst
			// case is posting a command that already exists, which is exactly the
			// pre-adoption behavior.
			return IssueComment{}, false, nil
		}
		if commit.Committer.Date.After(cutoff) {
			cutoff = commit.Committer.Date
		}
	}
	// A force-push can point the PR at a commit object whose committer date
	// predates commands made for an earlier head, so the commit date alone
	// is not a safe cutoff — any command older than the last force-push
	// belongs to a previous head and must not be adopted.
	if fp := s.headForcePushCutoff(ctx, repo, pr); fp.After(cutoff) {
		cutoff = fp
	}
	var best IssueComment
	var bestAt time.Time
	ok := false
	for _, comment := range comments {
		if strings.TrimSpace(comment.Body) != command {
			continue
		}
		when := comment.CreatedAt
		if when.IsZero() {
			when = comment.UpdatedAt
		}
		if !cutoff.IsZero() && when.Before(cutoff) {
			continue
		}
		if !ok || when.After(bestAt) {
			best = comment
			bestAt = when
			ok = true
		}
	}
	if !ok {
		return IssueComment{}, false, nil
	}
	// A command the bot has already answered with a review belongs to a
	// completed round for an earlier head. No cutoff can prove this case away:
	// a regular push has no timestamped head-update event, and the new head's
	// committer date can predate an old command (a local commit pushed later).
	// Adopting a consumed command would mark the new head fired without ever
	// reviewing it — skip adoption instead; the worst case is a duplicate
	// command, the pre-adoption behavior.
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		if _, ok := rateLimitWait(err); ok {
			return IssueComment{}, false, err
		}
		return IssueComment{}, false, nil
	}
	for _, review := range reviews {
		if s.isConfiguredBot(review.User.Login) && !review.SubmittedAt.Before(bestAt) {
			return IssueComment{}, false, nil
		}
	}
	if s.reviewCommandHasCompletionReply(comments, reviews, best.ID) {
		return IssueComment{}, false, nil
	}
	return best, ok, nil
}

// headForcePushCutoff returns when the PR head was last force-pushed, zero if
// unknown or never. Best-effort: on GraphQL failure adoption falls back to the
// commit-date cutoff rather than blocking the pump.
func (s *Service) headForcePushCutoff(ctx context.Context, repo string, pr int) time.Time {
	owner, name, found := strings.Cut(repo, "/")
	if !found {
		return time.Time{}
	}
	var result struct {
		Repository struct {
			PullRequest struct {
				TimelineItems struct {
					Nodes []struct {
						CreatedAt time.Time `json:"createdAt"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	query := `query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      timelineItems(itemTypes: HEAD_REF_FORCE_PUSHED_EVENT, last: 1) {
        nodes { ... on HeadRefForcePushedEvent { createdAt } }
      }
    }
  }
}`
	if err := s.gh.GraphQL(ctx, query, map[string]any{"owner": owner, "name": name, "number": pr}, &result); err != nil {
		return time.Time{}
	}
	nodes := result.Repository.PullRequest.TimelineItems.Nodes
	if len(nodes) == 0 {
		return time.Time{}
	}
	return nodes[len(nodes)-1].CreatedAt.UTC()
}

func (s *Service) recordExistingReviewPosted(ctx context.Context, item QueueItem, head string, commentID int64, firedAt time.Time) (State, error) {
	key := QueueKey(item.Repo, item.PR)
	recorded := false
	state, err := s.store.Update(ctx, func(st *State) error {
		recorded = false
		if st.InFlight != nil {
			return ErrNoChange
		}
		q := st.SortedQueue()
		if len(q) == 0 || q[0].Seq != item.Seq {
			return ErrNoChange
		}
		recorded = true
		removeQueued(st, item.Seq)
		st.InFlight = &InFlight{
			Seq:            item.Seq,
			Repo:           item.Repo,
			PR:             item.PR,
			Head:           head,
			Token:          randomToken(),
			Phase:          "posted",
			ReservedAt:     firedAt,
			FiredAt:        &firedAt,
			FiredCommentID: commentID,
			ByHost:         firstNonEmpty(item.Host, s.cfg.Host),
		}
		st.LastFired = &firedAt
		st.Warn = ""
		st.Fired[key] = head
		if st.AwaitingFeedback == nil {
			st.AwaitingFeedback = map[string]FeedbackWait{}
		}
		st.AwaitingFeedback[key] = s.newFeedbackWait(item.Repo, item.PR, head, firedAt, commentID)
		st.History = append([]HistoryItem{{
			Repo:   item.Repo,
			PR:     item.PR,
			Commit: head,
			At:     firedAt,
			Host:   firstNonEmpty(item.Host, s.cfg.Host),
		}}, st.History...)
		if len(st.History) > 20 {
			st.History = st.History[:20]
		}
		return nil
	})
	if err != nil {
		return State{}, err
	}
	if !recorded {
		return state, ErrNoChange
	}
	return state, nil
}

func (s *Service) Wait(ctx context.Context, repo string, pr int) (PumpResult, int, error) {
	repo = NormalizeRepo(repo)
	start := time.Now()
	enqueued := false
	var lastLog time.Time
	var lastFeedbackCheck time.Time
	feedbackCheckEvery := queuedFeedbackCheckEvery(s.cfg.PollInterval)
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
				state, _, err := s.store.Load(ctx)
				if err != nil {
					return PumpResult{}, 1, err
				}
				key := QueueKey(repo, pr)
				if state.AwaitingFeedback[key].Head == result.Head {
					return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
				}
				report, err := s.Feedback(ctx, repo, pr)
				if err != nil {
					return PumpResult{}, 1, err
				}
				if len(findingsReportedOnHead(report.Findings, report.Head)) > 0 || allReviewed(report.ReviewedBy) {
					return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
				}
				// Older versions could mark a head fired after mistaking carried-over
				// review prompts or a faster non-required bot for completion. With no
				// active wait or completed required-bot gate, remove that poisoned
				// marker and enqueue the real replacement review.
				updated, err := s.store.Update(ctx, func(st *State) error {
					if st.Fired[key] != result.Head || st.AwaitingFeedback[key].Head == result.Head || st.Contains(repo, pr) {
						return ErrNoChange
					}
					delete(st.Fired, key)
					return nil
				})
				if err != nil {
					return PumpResult{}, 1, err
				}
				s.sync(ctx, updated)
				enqueued = false
				continue
			}
		}
		if lastFeedbackCheck.IsZero() || time.Since(lastFeedbackCheck) >= feedbackCheckEvery {
			report, err := s.Feedback(ctx, repo, pr)
			if err != nil {
				return PumpResult{}, 1, err
			}
			lastFeedbackCheck = time.Now()
			// Return current-head findings immediately so the caller can fix locally.
			// The queue entry stays active: policy requires holding this head until the
			// account slot and every required reviewer finish.
			if len(findingsReportedOnHead(report.Findings, report.Head)) > 0 {
				if s.log != nil {
					s.log.Printf("%s#%d feedback already available on %s; leaving review slot wait", repo, pr, report.Head)
				}
				return PumpResult{
					Action: "deduped",
					Repo:   repo,
					PR:     pr,
					Head:   report.Head,
					Reason: "feedback already available",
				}, 3, nil
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
			head, open, herr := s.pullHead(ctx, repo, pr)
			if herr == nil && !open {
				// PR was closed/merged and dropped from the queue — nothing to review.
				// Return a terminal result so crq loop stops instead of polling forever.
				return PumpResult{Action: "skipped", Repo: repo, PR: pr, Reason: "pr closed"}, 2, nil
			}
			if herr == nil && head != "" && state.Fired[QueueKey(repo, pr)] == head {
				return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: head}, 3, nil
			}
			if result.Action == "fired" && result.Repo == repo && result.PR == pr {
				return result, 0, nil
			}
			enqueued = false
			continue
		}
		if s.log != nil && time.Since(lastLog) >= 30*time.Second {
			reason := result.Reason
			if reason == "" {
				reason = result.Action
			}
			s.log.Printf("%s#%d waiting for a review slot — %s (%s elapsed)", repo, pr, reason, time.Since(start).Round(time.Second))
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return PumpResult{}, 1, ctx.Err()
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

func queuedFeedbackCheckEvery(poll time.Duration) time.Duration {
	if poll <= 0 {
		return 30 * time.Second
	}
	if poll < 30*time.Second {
		return poll
	}
	return 30 * time.Second
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
		delete(st.AwaitingFeedback, QueueKey(repo, pr))
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
	// Honor the freshness shortcut only when the last reading was conclusive. If a
	// probe is still pending (CalibAskedAt set, no reply yet), keep re-checking so a
	// late "rate-limited" reply isn't ignored for the full TTL — which would let Pump
	// fire straight into the limit.
	if state.Blocked.CalibAskedAt == nil && state.Blocked.CheckedAt != nil && now.Sub(*state.Blocked.CheckedAt) < s.cfg.CalibrationTTL {
		return state, nil
	}
	blocked, err := s.readQuota(ctx, s.calibrationIssue(state), now, state.Blocked.CalibAskedAt)
	if err != nil {
		return state, err
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		if st.Blocked.CalibAskedAt == nil && st.Blocked.CheckedAt != nil && time.Since(*st.Blocked.CheckedAt) < s.cfg.CalibrationTTL {
			return ErrNoChange
		}
		// A fresh calibrate reading replaces the whole Blocked struct; carry the
		// rate-limit comment identity over so requeueInflight can still recognise an
		// edited comment it already accounted for.
		rlID, rlUpdated := st.Blocked.RLCommentID, st.Blocked.RLCommentUpdated
		st.Blocked = blocked
		if st.Blocked.RLCommentID == 0 {
			st.Blocked.RLCommentID = rlID
			st.Blocked.RLCommentUpdated = rlUpdated
		}
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

// calibrationIssue returns the calibration issue crq should probe: the rotated
// replacement recorded in state after a cap wedge, or the configured one.
func (s *Service) calibrationIssue(state State) int {
	if state.CalibrationIssue > 0 {
		return state.CalibrationIssue
	}
	return s.cfg.CalibrationPR
}

const calibrationIssueBody = "crq probes CodeRabbit's account-wide rate-limit state here with `@coderabbitai rate limit` so it never spends a real review to calibrate. Auto-created after a prior calibration thread hit GitHub's 2500-comment cap. Managed by crq — safe to leave alone."

// rotateCalibration creates a fresh calibration issue in the gate repo and
// records its number in the shared state, so the whole fleet abandons a
// calibration thread that hit GitHub's hard 2500-comment cap. A capped thread is
// permanently unpostable — pruning can't recover it once every deletable comment
// is already gone — which otherwise wedges quota calibration and floods the log
// with 403s. Returns the new issue number.
func (s *Service) rotateCalibration(ctx context.Context, oldIssue int) (int, error) {
	issue, err := s.gh.CreateIssue(ctx, s.cfg.GateRepo, "crq rate-limit calibration", calibrationIssueBody)
	if err != nil {
		return 0, err
	}
	if issue.Number <= 0 {
		return 0, fmt.Errorf("calibration rotation: created issue has no number")
	}
	if _, err := s.store.Update(ctx, func(st *State) error {
		if st.CalibrationIssue == issue.Number {
			return ErrNoChange
		}
		st.CalibrationIssue = issue.Number
		return nil
	}); err != nil && !errors.Is(err, ErrNoChange) {
		return 0, err
	}
	if s.log != nil {
		s.log.Printf("calibration issue #%d hit the comment cap; rotated to fresh issue #%d", oldIssue, issue.Number)
	}
	return issue.Number, nil
}

func (s *Service) readQuota(ctx context.Context, issue int, now time.Time, pendingAsked *time.Time) (Blocked, error) {
	blocked := Blocked{Scope: strings.Join(s.cfg.Scope, ","), Source: "calibrate", CheckedAt: &now}
	cutoff := now.Add(-s.cfg.CalibrationTTL)
	keepAfter := now.Add(-2 * s.cfg.CalibrationTTL)
	if reply, ok, err := s.latestCalibrationReply(ctx, issue, cutoff); err != nil {
		return blocked, err
	} else if ok {
		remaining, reset := parseQuota(reply.Body, reply.UpdatedAt)
		blocked.Remaining = remaining
		blocked.BlockedUntil = reset
		s.pruneCalibration(ctx, issue, keepAfter, 80)
		return blocked, nil
	}
	// A probe from a previous call is still pending and not yet stale, and the check
	// above found no reply to it yet: keep waiting for its (possibly late) reply
	// instead of posting another probe every cycle.
	if pendingAsked != nil && pendingAsked.After(cutoff) {
		blocked.CalibAskedAt = pendingAsked
		return blocked, nil
	}
	asked, err := s.gh.PostIssueComment(ctx, s.cfg.GateRepo, issue, s.cfg.RateLimitCommand)
	if err != nil {
		// The calibration thread hit GitHub's 2500-comment cap. Prune our old probe
		// comments and retry once; if pruning can't drop us back under the cap (all
		// deletable comments are already gone), rotate to a fresh issue and retry
		// there instead of failing every cycle.
		if isCommentCapError(err) {
			if pruned := s.pruneCalibration(ctx, issue, keepAfter, 100); pruned > 0 {
				asked, err = s.gh.PostIssueComment(ctx, s.cfg.GateRepo, issue, s.cfg.RateLimitCommand)
			}
			if err != nil && isCommentCapError(err) {
				if newIssue, rerr := s.rotateCalibration(ctx, issue); rerr == nil {
					issue = newIssue
					asked, err = s.gh.PostIssueComment(ctx, s.cfg.GateRepo, issue, s.cfg.RateLimitCommand)
				} else if s.log != nil {
					s.log.Printf("calibration rotation failed: %v", rerr)
				}
			}
		}
		if err != nil {
			if s.log != nil {
				s.log.Printf("calibration probe on #%d failed: %v", issue, err)
			}
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
		reply, ok, err := s.latestCalibrationReply(ctx, issue, asked.CreatedAt.Add(-time.Second))
		if err != nil {
			return blocked, err
		}
		if ok {
			remaining, reset := parseQuota(reply.Body, reply.UpdatedAt)
			blocked.Remaining = remaining
			blocked.BlockedUntil = reset
			blocked.CalibAskedAt = nil
			s.pruneCalibration(ctx, issue, keepAfter, 80)
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
func (s *Service) pruneCalibration(ctx context.Context, issue int, keepAfter time.Time, max int) int {
	if issue <= 0 || max <= 0 {
		return 0
	}
	comments, err := s.gh.ListIssueCommentsPage(ctx, s.cfg.GateRepo, issue, 1, 100)
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
		s.log.Printf("calibration prune: removed %d old comment(s) from #%d", deleted, issue)
	}
	return deleted
}

// isCalibrationNoise reports whether a comment is a spent calibration artifact:
// one of crq's "@coderabbitai rate limit" probes or a CodeRabbit auto-reply.
func (s *Service) isCalibrationNoise(c IssueComment) bool {
	if strings.TrimSpace(c.Body) == strings.TrimSpace(s.cfg.RateLimitCommand) {
		return true
	}
	return s.isConfiguredBot(c.User.Login) && strings.Contains(c.Body, s.cfg.CalibrationMarker)
}

func (s *Service) latestCalibrationReply(ctx context.Context, issue int, after time.Time) (IssueComment, bool, error) {
	comments, err := s.gh.ListIssueComments(ctx, s.cfg.GateRepo, issue)
	if err != nil {
		return IssueComment{}, false, err
	}
	var best IssueComment
	ok := false
	for _, comment := range comments {
		if !s.isConfiguredBot(comment.User.Login) || !comment.UpdatedAt.After(after) {
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

func (s *Service) isConfiguredBot(login string) bool {
	return normalizeBotName(login) == normalizeBotName(s.cfg.Bot)
}

type inflightCheck struct {
	Done         bool
	Requeue      bool
	Reason       string
	BlockedUntil *time.Time
	// FeedbackComplete is set on Done when every required bot has submitted a
	// review for the fired round, so Pump knows whether the feedback wait is
	// satisfied or must survive for the bots that are still reviewing.
	FeedbackComplete bool
	// RLCommentID/RLCommentUpdated identify the rate-limit comment that produced a
	// warnRateLimited requeue, so requeueInflight can tell a fresh rate-limit event
	// from a re-observation of the same edited comment.
	RLCommentID      int64
	RLCommentUpdated time.Time
}

// Done reasons from inflightStatus. Pump distinguishes them: a submitted
// review or bot comment means feedback arrived, while a bare reaction only
// acknowledges the command — the review is still in progress.
const (
	doneReviewSubmitted = "review submitted"
	doneBotReacted      = "bot reacted"
	doneBotComment      = "bot comment"
	doneAlreadyReviewed = "head already reviewed"
)

// notBefore reports whether t is at or after baseline. GitHub timestamps are
// second-granular, so a bot completion in the same second as the trigger must
// still count — a strict After would miss it and refire a duplicate review.
func notBefore(t, baseline time.Time) bool { return !t.Before(baseline) }

// requiredFeedbackComplete reports whether every bot in CRQ_REQUIRED_BOTS has
// submitted a review for the fired round. It mirrors what flips Feedback's
// ReviewedBy — submitted reviews matching the fired head — so Pump never drops
// a wait that Feedback would still report as waiting on another required bot.
// With no required bots configured, the configured reviewer's response that
// produced the Done is all there is to wait for.
func (s *Service) requiredFeedbackComplete(inf *InFlight, reviews []Review, comments []IssueComment) bool {
	if len(s.cfg.RequiredBots) == 0 {
		return true
	}
	return s.feedbackCompleteForRound(reviews, comments, s.cfg.RequiredBots, inf.Head, *inf.FiredAt)
}

// botsReviewedHead reports whether every bot in the set has reviewed head —
// the same check Feedback uses before flipping ReviewedBy: a review whose
// commit matches the fired head counts regardless of when it was submitted,
// because a required bot may have reviewed the commit before this round was
// even triggered. Only when there is no head to match does the submission
// time (at/after since) gate the round, so a review for some other push never
// counts toward it.
func botsReviewedHead(reviews []Review, bots map[string]struct{}, head string, since time.Time) bool {
	return allReviewed(reviewedByForRound(reviews, bots, head, since))
}

func (s *Service) feedbackCompleteForRound(reviews []Review, comments []IssueComment, awaited []string, head string, since time.Time) bool {
	reviewedBy := reviewedByForRound(reviews, botSet(awaited), head, since)
	if needsConfiguredBotReview(reviewedBy, s.cfg.Bot) && s.completionReplyForFiredCommand(comments, reviews, since) {
		markReviewed(reviewedBy, s.cfg.Bot)
	}
	return allReviewed(reviewedBy)
}

func reviewedByForRound(reviews []Review, bots map[string]struct{}, head string, since time.Time) map[string]bool {
	reviewedBy := map[string]bool{}
	for bot := range bots {
		reviewedBy[bot] = false
	}
	for _, review := range reviews {
		if !inBots(bots, review.User.Login) {
			continue
		}
		if head != "" {
			if strings.HasPrefix(review.CommitID, head) {
				markReviewed(reviewedBy, review.User.Login)
			}
			continue
		}
		if notBefore(review.SubmittedAt, since) {
			markReviewed(reviewedBy, review.User.Login)
		}
	}
	return reviewedBy
}

func allReviewed(reviewedBy map[string]bool) bool {
	for _, reviewed := range reviewedBy {
		if !reviewed {
			return false
		}
	}
	return true
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
	reviews, err := s.gh.ListReviews(ctx, inf.Repo, inf.PR)
	if err != nil {
		return inflightCheck{}, err
	}
	// CodeRabbit can post the "does not re-review already reviewed commits"
	// boilerplate after a rate-limited first attempt, even though no review exists.
	// Treat that acknowledgement as terminal only when GitHub has the evidence it
	// claims: a configured-bot review matching this fired head. Without that proof,
	// a co-occurring rate-limit notice must requeue the PR for a real later attempt.
	alreadyReviewedAck := false
	for _, comment := range comments {
		if !s.isConfiguredBot(comment.User.Login) || comment.UpdatedAt.Before(*inf.FiredAt) {
			continue
		}
		if s.isReviewAlreadyDone(comment.Body) {
			alreadyReviewedAck = true
			break
		}
	}
	for _, review := range reviews {
		if !s.isConfiguredBot(review.User.Login) || !reviewMatchesRound(review, inf.Head, *inf.FiredAt) {
			continue
		}
		reason := doneReviewSubmitted
		if alreadyReviewedAck {
			reason = doneAlreadyReviewed
		}
		return inflightCheck{Done: true, Reason: reason, FeedbackComplete: s.requiredFeedbackComplete(inf, reviews, comments)}, nil
	}
	for _, comment := range comments {
		if !s.isConfiguredBot(comment.User.Login) || comment.UpdatedAt.Before(*inf.FiredAt) {
			continue
		}
		if s.isRateLimited(comment.Body) {
			reset := parseAvailableIn(comment.Body, comment.UpdatedAt)
			return inflightCheck{Requeue: true, Reason: warnRateLimited, BlockedUntil: reset, RLCommentID: comment.ID, RLCommentUpdated: comment.UpdatedAt}, nil
		}
	}
	if inf.FiredCommentID != 0 {
		reactions, err := s.gh.ListCommentReactions(ctx, inf.Repo, inf.FiredCommentID)
		if err != nil {
			// Don't treat a transient/rate-limited reactions failure as "no reaction":
			// that can misclassify an acknowledged review as timed out and refire it.
			return inflightCheck{}, err
		}
		for _, reaction := range reactions {
			if s.isConfiguredBot(reaction.User.Login) {
				return inflightCheck{Done: true, Reason: doneBotReacted}, nil
			}
		}
	}
	for _, comment := range comments {
		if s.isConfiguredBot(comment.User.Login) && comment.ID != inf.FiredCommentID && notBefore(comment.UpdatedAt, *inf.FiredAt) && !s.isRateLimited(comment.Body) && !s.isReviewsPaused(comment.Body) && !s.isReviewAlreadyDone(comment.Body) {
			return inflightCheck{Done: true, Reason: doneBotComment, FeedbackComplete: s.requiredFeedbackComplete(inf, reviews, comments)}, nil
		}
	}
	if time.Since(*inf.FiredAt) > s.cfg.InflightTimeout {
		return inflightCheck{Requeue: true, Reason: "in-flight timeout"}, nil
	}
	return inflightCheck{}, nil
}

// reviewMatchesRound reports whether a submitted review is evidence for this
// fired round. A known head must match the review commit; submission time alone
// could otherwise let a delayed review of an older head complete the new one.
func reviewMatchesRound(review Review, head string, firedAt time.Time) bool {
	if head != "" {
		return strings.HasPrefix(review.CommitID, head)
	}
	return notBefore(review.SubmittedAt, firedAt)
}

// pruneExpiredWaits drops AwaitingFeedback entries whose deadline has passed.
// A wait can outlive its review round — a crashed loop, or an autoreview fire
// whose in-flight slot was released on a bot reaction before the review was
// submitted — and nothing else removes it. Any loop resumed after pruning
// reconstructs its start from History and times out immediately, so no wait
// gets a fresh clock out of this. Returns the updated state, or nil if nothing
// was pruned.
func (s *Service) pruneExpiredWaits(ctx context.Context, state State) *State {
	if s.cfg.DryRun {
		return nil
	}
	now := time.Now().UTC()
	stale := false
	for _, wait := range state.AwaitingFeedback {
		if !wait.Deadline.IsZero() && now.After(wait.Deadline) {
			stale = true
			break
		}
	}
	if !stale {
		return nil
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		changed := false
		for key, wait := range st.AwaitingFeedback {
			if !wait.Deadline.IsZero() && now.After(wait.Deadline) {
				delete(st.AwaitingFeedback, key)
				changed = true
			}
		}
		if !changed {
			return ErrNoChange
		}
		return nil
	})
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: failed to prune expired feedback waits: %v", err)
		}
		return nil
	}
	s.sync(ctx, updated)
	return &updated
}

// sweepFeedbackWaits checks at most one lingering feedback wait per pump — the
// oldest — against the PR's submitted reviews, and clears it once every awaited
// bot (CRQ_REQUIRED_BOTS, or the configured reviewer when none are set) has
// reviewed the fired head. This is what finishes a round whose in-flight slot
// was released on a bare bot reaction: once InFlight is nil no Pump path runs
// inflightStatus again, and without a Loop nothing else would clear the wait
// before the deadline prune. One wait per pump bounds the sweep's use of the
// shared REST quota; the deadline prune stays the backstop for rounds whose
// feedback never becomes a head-matched review.
func (s *Service) sweepFeedbackWaits(ctx context.Context, state State) State {
	if s.cfg.DryRun || len(state.AwaitingFeedback) == 0 {
		return state
	}
	var oldest FeedbackWait
	found := false
	for _, wait := range state.AwaitingFeedback {
		if wait.Head == "" {
			continue
		}
		if !found || wait.StartedAt.Before(oldest.StartedAt) {
			oldest = wait
			found = true
		}
	}
	if !found {
		return state
	}
	reviews, err := s.gh.ListReviews(ctx, oldest.Repo, oldest.PR)
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: feedback wait sweep for %s#%d failed: %v", oldest.Repo, oldest.PR, err)
		}
		return state
	}
	awaited := s.cfg.RequiredBots
	if len(awaited) == 0 {
		awaited = []string{s.cfg.Bot}
	}
	reviewedBy := reviewedByForRound(reviews, botSet(awaited), oldest.Head, oldest.StartedAt)
	if allReviewed(reviewedBy) {
		s.clearFeedbackWait(ctx, oldest.Repo, oldest.PR, oldest.Head)
		if updated, _, err := s.store.Load(ctx); err == nil {
			return updated
		}
		return state
	}
	if !needsConfiguredBotReview(reviewedBy, s.cfg.Bot) {
		return state
	}
	comments, err := s.gh.ListIssueComments(ctx, oldest.Repo, oldest.PR)
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: feedback wait completion sweep for %s#%d failed: %v", oldest.Repo, oldest.PR, err)
		}
		return state
	}
	if !s.completionReplyForFiredCommand(comments, reviews, oldest.StartedAt) {
		return state
	}
	s.clearFeedbackWait(ctx, oldest.Repo, oldest.PR, oldest.Head)
	if updated, _, err := s.store.Load(ctx); err == nil {
		return updated
	}
	return state
}

func (s *Service) requeueInflight(st *State, status inflightCheck) {
	if st.InFlight == nil {
		return
	}
	inf := *st.InFlight
	now := time.Now().UTC()
	st.Queue = append(st.Queue, QueueItem{
		Seq:   inf.Seq,
		Owner: ownerOf(inf.Repo),
		Repo:  inf.Repo,
		PR:    inf.PR,
		Host:  inf.ByHost,
		// The command comment from the abandoned fire is still on the PR and
		// still newer than the head commit; RequeuedAt keeps the next fire from
		// adopting it (see existingReviewCommand).
		EnqueuedAt: now,
		RequeuedAt: &now,
	})
	sort.Slice(st.Queue, func(i, j int) bool { return st.Queue[i].Seq < st.Queue[j].Seq })
	key := QueueKey(inf.Repo, inf.PR)
	delete(st.Fired, key)
	delete(st.AwaitingFeedback, key)
	st.InFlight = nil
	if status.Reason == warnRateLimited {
		// A rate limit is shown by the Blocked state (the dashboard's Rate-limit
		// row), not a sticky Warn — otherwise once the window passes the table says
		// "not currently limited" while a stale "rate limited" warning lingers.
		until := status.BlockedUntil
		// CodeRabbit edits one rate-limit comment in place, so a later fire sees the
		// same comment with an advanced UpdatedAt. Don't let that re-observation
		// extend the block on every bounce: if this is the comment that already set
		// the standing block and its window is still open, keep the existing window.
		sameComment := status.RLCommentID != 0 && status.RLCommentID == st.Blocked.RLCommentID
		if sameComment && st.Blocked.BlockedUntil != nil && st.Blocked.BlockedUntil.After(now) {
			until = st.Blocked.BlockedUntil
		}
		if until == nil || !until.After(now) {
			// No parseable "available in" window: back off a conservative fixed
			// interval instead of a short re-calibrate, so an unrecognised phrasing
			// can't drop us into a couple-of-minutes retry against the shared quota.
			t := now.Add(s.rateLimitFallback())
			until = &t
		}
		zero := 0
		st.Blocked.BlockedUntil = until
		st.Blocked.Remaining = &zero
		st.Blocked.Source = "warning"
		st.Blocked.CheckedAt = &now
		if status.RLCommentID != 0 {
			st.Blocked.RLCommentID = status.RLCommentID
			u := status.RLCommentUpdated
			st.Blocked.RLCommentUpdated = &u
		}
		st.Warn = ""
		// Per-head cooldown that survives this requeue: needsReview refuses to
		// re-enqueue inf.Head until the window passes. Fired is cleared above so a
		// genuinely throttled PR can still retry once the window clears, and this
		// cooldown is what keeps that gap from letting the same head be re-fired
		// before then — the guard whose absence let one head fire seven times.
		setCooldown(st, key, inf.Head, *until)
	} else {
		st.Warn = status.Reason
	}
	if s.log != nil {
		blockedUntil := "-"
		if st.Blocked.BlockedUntil != nil {
			blockedUntil = st.Blocked.BlockedUntil.UTC().Format(time.RFC3339)
		}
		s.log.Printf("requeue %s@%s reason=%q blocked_until=%s", key, inf.Head, status.Reason, blockedUntil)
	}
}

// rateLimitFallback is the block window applied when a rate-limit comment carries
// no parseable "available in" duration. CodeRabbit's real windows run to tens of
// minutes, so a conservative floor is far safer than retrying in a minute or two.
func (s *Service) rateLimitFallback() time.Duration {
	if s.cfg.RateLimitFallback > 0 {
		return s.cfg.RateLimitFallback
	}
	return 15 * time.Minute
}

// setCooldown records a per-head fire cooldown, ignoring an empty head or an
// already-passed deadline so the map never carries dead entries.
func setCooldown(st *State, key, head string, until time.Time) {
	if head == "" || !until.After(time.Now().UTC()) {
		return
	}
	if st.Cooldown == nil {
		st.Cooldown = map[string]FireCooldown{}
	}
	st.Cooldown[strings.ToLower(key)] = FireCooldown{Head: head, Until: until}
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
	if !open {
		return "", false, nil
	}
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
		if normalizeBotName(review.User.Login) == normalizeBotName(s.cfg.Bot) && strings.HasPrefix(review.CommitID, head) {
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

// The CodeRabbit comment classifiers live in internal/dialect; these
// forwarders keep call sites and tests stable during the refactor.
func (s *Service) isRateLimited(body string) bool { return s.cr.IsRateLimited(body) }

func (s *Service) isReviewsPaused(body string) bool     { return s.cr.IsReviewsPaused(body) }
func (s *Service) isReviewAlreadyDone(body string) bool { return s.cr.IsReviewAlreadyDone(body) }

// isCommentCapError reports whether err is GitHub's hard cap of 2500 comments per
// issue ("Commenting is disabled on issues with more than 2500 comments").
func isCommentCapError(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	b := strings.ToLower(api.Body)
	return strings.Contains(b, "commenting is disabled") || strings.Contains(b, "more than 2500 comments")
}
