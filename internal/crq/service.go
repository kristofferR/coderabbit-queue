package crq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type Logger interface {
	Printf(string, ...any)
}

type GitHubAPI interface {
	GetPull(context.Context, string, int) (ghapi.Pull, error)
	GetCommit(context.Context, string, string) (ghapi.Commit, error)
	ListReviews(context.Context, string, int) ([]ghapi.Review, error)
	ListIssueComments(context.Context, string, int) ([]ghapi.IssueComment, error)
	ListIssueCommentsPage(context.Context, string, int, int, int) ([]ghapi.IssueComment, error)
	ListReviewComments(context.Context, string, int) ([]ghapi.ReviewComment, error)
	ListIssueReactions(context.Context, string, int) ([]ghapi.Reaction, error)
	ListCommentReactions(context.Context, string, int64) ([]ghapi.Reaction, error)
	PostIssueComment(context.Context, string, int, string) (ghapi.IssueComment, error)
	DeleteIssueComment(context.Context, string, int64) error
	CreateIssue(context.Context, string, string, string) (ghapi.Issue, error)
	SearchOpenPRs(context.Context, string, bool, int) ([]ghapi.SearchPR, error)
	EachOpenPR(context.Context, string, bool, func(ghapi.SearchPR) (bool, error)) error
	GraphQL(context.Context, string, map[string]any, any) error
}

type Service struct {
	cfg   Config
	cr    dialect.CodeRabbit
	gh    GitHubAPI
	store StateStore
	log   Logger
	// now overrides the wall clock for the scheduling DECISIONS in the
	// pump/enqueue/sweep/wait paths (see clock). nil in production; the replay
	// suite injects a controllable fake so an incident can be re-enacted
	// deterministically. It intentionally does NOT reach logging/jitter/token or
	// the fake GitHub timestamps, which stay on real time.
	now func() time.Time
}

func NewService(cfg Config, gh GitHubAPI, store StateStore, log Logger) *Service {
	cr := dialect.CodeRabbit{
		CompletionMarker:  cfg.CompletionMarker,
		RateLimitMarker:   cfg.RateLimitMarker,
		CalibrationMarker: cfg.CalibrationMarker,
	}
	return &Service{cfg: cfg, cr: cr, gh: gh, store: store, log: log}
}

// clock is the service's notion of "now" (UTC) for scheduling decisions: retry
// windows, fire pacing, adoption cutoffs, feedback deadlines. Tests inject s.now
// to drive these deterministically; production leaves it nil and reads the wall
// clock.
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// warnRateLimited is the requeue reason for a fire that came back account
// blocked. It matches the engine's Transition.Reason (both reference the one
// dialect constant) and is surfaced via AccountQuota, not the sticky Warn field.
const warnRateLimited = dialect.ReasonRateLimited

type EnqueueResult struct {
	Repo          string `json:"repo"`
	PR            int    `json:"pr"`
	Queued        bool   `json:"queued"`
	AlreadyQueued bool   `json:"already_queued"`
	Deduped       bool   `json:"deduped"`
	Head          string `json:"head,omitempty"`
	Seq           int64  `json:"seq,omitempty"`
}

// Enqueue records a review round for repo#pr's current head. A round already
// tracking the head is reported (queued/deduped) instead of duplicated; a round
// on a stale head is superseded to track the new one.
func (s *Service) Enqueue(ctx context.Context, repo string, pr int) (EnqueueResult, error) {
	repo = NormalizeRepo(repo)
	result := EnqueueResult{Repo: repo, PR: pr}
	head, err := s.headShort(ctx, repo, pr)
	if err != nil {
		return result, err
	}
	state, err := s.store.Update(ctx, func(st *State) error {
		now := s.clock()
		r := st.Round(repo, pr)
		if r != nil && r.Head == head {
			switch r.Phase {
			case PhaseFired, PhaseReviewing, PhaseCompleted:
				result.Deduped = true
				result.Head = head
			default:
				result.AlreadyQueued = true
			}
			return ErrNoChange
		}
		var nr *Round
		if r != nil {
			// The tracked head is stale — supersede to the current one.
			nr, err = st.Supersede(repo, pr, head, now)
		} else {
			nr, err = st.NewRound(repo, pr, head, now)
		}
		if err != nil {
			return err
		}
		result.Queued = true
		result.Seq = nr.Seq
		return nil
	})
	if err != nil {
		return result, err
	}
	s.sync(ctx, state)
	return result, nil
}

// queueCandidate is one PR the autoreview pass decided to enqueue, carrying the
// head it resolved so enqueueBatch can create the round without re-fetching.
type queueCandidate struct {
	Repo string
	PR   int
	Head string
}

// enqueueBatch appends several PRs in a single compare-and-swap write plus one
// dashboard sync, so a large autoreview pass doesn't produce N separate state
// writes / issue edits. A PR already tracked at the same head is skipped; a
// stale head is superseded. The DecideFire dedup still backstops at pump time.
func (s *Service) enqueueBatch(ctx context.Context, items []queueCandidate) error {
	if len(items) == 0 {
		return nil
	}
	state, err := s.store.Update(ctx, func(st *State) error {
		now := s.clock()
		added := 0
		for _, it := range items {
			repo := NormalizeRepo(it.Repo)
			if r := st.Round(repo, it.PR); r != nil {
				if r.Head == it.Head {
					continue
				}
				if _, err := st.Supersede(repo, it.PR, it.Head, now); err != nil {
					return err
				}
				added++
				continue
			}
			if _, err := st.NewRound(repo, it.PR, it.Head, now); err != nil {
				return err
			}
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

// Pump advances the queue by one observe → engine → apply step: it progresses
// the round holding the fire slot, sweeps one reviewing round toward
// completion, then fires the next eligible round. In DryRun it computes the
// same decisions but writes and posts nothing.
func (s *Service) Pump(ctx context.Context) (PumpResult, error) {
	now := s.clock()
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return PumpResult{}, err
	}

	// 1. The round holding the fire slot: progress it and return, mirroring v2's
	//    "handle in-flight first" so a single pump never both progresses and fires.
	if slot := st.SlotRound(); slot != nil {
		return s.progressSlotRound(ctx, *slot)
	}

	// 2. Reviewing rounds no longer hold the slot; sweep the oldest one toward
	//    completion/retry (bounded to one per pump, like v2's feedback sweep).
	if updated, err := s.sweepReviewing(ctx, st, now); err != nil {
		return PumpResult{}, err
	} else {
		st = updated
	}

	// 3. Fire the next eligible round.
	next := st.NextEligible(now)
	if next == nil {
		return PumpResult{Action: "idle"}, nil
	}
	// Terminal cleanup is independent of quota and pacing: drop a closed/merged
	// PR before either gate so it leaves on this pump instead of lingering.
	if _, open, err := s.pullHead(ctx, next.Repo, next.PR); err != nil {
		return PumpResult{}, err
	} else if !open {
		return s.abandonRound(ctx, *next, "pr closed", "skipped")
	}
	if refreshed, err := s.RefreshQuota(ctx); err == nil {
		st = refreshed
	} else {
		return PumpResult{}, err
	}
	now = s.clock()
	if st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now) {
		return PumpResult{Action: "blocked", Reason: st.Account.BlockedUntil.Format(time.RFC3339)}, nil
	}
	if st.LastFired != nil && now.Sub(*st.LastFired) < s.cfg.MinInterval {
		return PumpResult{Action: "min_interval", Reason: s.cfg.MinInterval.String()}, nil
	}
	next = st.NextEligible(now)
	if next == nil {
		return PumpResult{Action: "idle"}, nil
	}
	obs, err := s.observe(ctx, next.Repo, next.PR, next, now)
	if err != nil {
		return PumpResult{}, err
	}
	decision := engine.DecideFire(s.global(st, now), *next, obs.eng, now, s.policy())
	return s.applyFire(ctx, *next, obs.eng, decision, now)
}

func (s *Service) global(st State, now time.Time) engine.Global {
	return engine.Global{
		SlotFree:     st.SlotRound() == nil,
		BlockedUntil: st.Account.BlockedUntil,
		LastFired:    st.LastFired,
	}
}

// progressSlotRound observes and progresses the round holding the fire slot.
func (s *Service) progressSlotRound(ctx context.Context, slot Round) (PumpResult, error) {
	now := s.clock()
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return PumpResult{}, err
	}
	obs, err := s.observe(ctx, slot.Repo, slot.PR, &slot, now)
	if err != nil {
		return PumpResult{}, err
	}
	s.selfHealCodex(ctx, slot, obs.eng, now)
	tr := engine.Progress(slot, st.Account, obs.eng, now, s.policy())
	if tr.Outcome == engine.KeepWaiting {
		return PumpResult{Action: "waiting", Repo: slot.Repo, PR: slot.PR, Reason: tr.Reason}, nil
	}
	if s.cfg.DryRun {
		return slotResult(slot, tr), nil
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(slot.Repo, slot.PR)
		if r == nil || st.FireSlot == nil || st.FireSlot.Token != slot.Token {
			return ErrNoChange
		}
		return s.applyTransition(st, r, tr, now)
	})
	if err != nil {
		return PumpResult{}, err
	}
	s.sync(ctx, updated)
	if s.log != nil && (tr.Outcome == engine.OutRetry || tr.Outcome == engine.OutReleaseSlot) {
		blockedUntil := "-"
		if updated.Account.BlockedUntil != nil {
			blockedUntil = updated.Account.BlockedUntil.UTC().Format(time.RFC3339)
		}
		s.log.Printf("requeue %s@%s reason=%q blocked_until=%s", QueueKey(slot.Repo, slot.PR), slot.Head, tr.Reason, blockedUntil)
	}
	return slotResult(slot, tr), nil
}

// applyTransition applies a fired/reviewing round's engine Transition to state:
// the round transition plus any fire-slot release and account-quota block.
func (s *Service) applyTransition(st *State, r *Round, tr engine.Transition, now time.Time) error {
	key := QueueKey(r.Repo, r.PR)
	switch tr.Outcome {
	case engine.OutComplete:
		if err := r.Complete(); err != nil {
			return err
		}
	case engine.OutReviewing:
		if err := r.Acknowledge(); err != nil {
			return err
		}
	case engine.OutRetry:
		if tr.Blocked != nil {
			applyAccountBlock(st, tr.Blocked, now)
		}
		if err := r.AwaitRetry(tr.RetryAt, tr.Reason, now); err != nil {
			return err
		}
	case engine.OutReleaseSlot:
		if err := r.ReleaseToQueue(tr.Reason, now); err != nil {
			return err
		}
	case engine.OutAbandon:
		st.EndRound(r.Repo, r.PR, tr.Reason)
		releaseSlot(st, key)
		return nil
	default:
		return nil
	}
	st.PutRound(*r)
	releaseSlot(st, key)
	return nil
}

// releaseSlot clears the fire slot when it points at key.
func releaseSlot(st *State, key string) {
	if st.FireSlot != nil && st.FireSlot.Key == key {
		st.FireSlot = nil
	}
}

// applyAccountBlock ports requeueInflight's account-quota bookkeeping. The window
// (including same-comment reuse) was resolved by the engine, so only the store
// write happens here.
func applyAccountBlock(st *State, blk *engine.AccountBlock, now time.Time) {
	until := blk.Until.UTC()
	zero := 0
	st.Account.BlockedUntil = &until
	st.Account.Remaining = &zero
	st.Account.Source = "warning"
	st.Account.CheckedAt = &now
	if blk.CommentID != 0 {
		st.Account.RLCommentID = blk.CommentID
		u := blk.CommentUpdated.UTC()
		st.Account.RLCommentUpdated = &u
	}
	st.Warn = ""
}

func slotResult(slot Round, tr engine.Transition) PumpResult {
	r := PumpResult{Repo: slot.Repo, PR: slot.PR, Head: slot.Head, Reason: tr.Reason}
	switch tr.Outcome {
	case engine.OutComplete, engine.OutReviewing:
		r.Action = "cleared"
	case engine.OutRetry, engine.OutReleaseSlot:
		r.Action = "requeued"
	case engine.OutAbandon:
		r.Action = "cleared"
	default:
		r.Action = "waiting"
	}
	return r
}

// sweepReviewing progresses the oldest fired/reviewing round that is not holding
// the fire slot, so a round whose slot was released on a bot ack still reaches
// completion (or parks) without a Loop running. Bounded to one per pump.
func (s *Service) sweepReviewing(ctx context.Context, st State, now time.Time) (State, error) {
	if s.cfg.DryRun {
		return st, nil
	}
	var target *Round
	for key := range st.Rounds {
		r := st.Rounds[key]
		if r.Phase != PhaseFired && r.Phase != PhaseReviewing {
			continue
		}
		if target == nil || firedOrEnqueuedAt(r).Before(firedOrEnqueuedAt(*target)) {
			c := r
			target = &c
		}
	}
	if target == nil {
		return st, nil
	}
	obs, err := s.observe(ctx, target.Repo, target.PR, target, now)
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: reviewing-round sweep for %s#%d failed: %v", target.Repo, target.PR, err)
		}
		return st, nil
	}
	s.selfHealCodex(ctx, *target, obs.eng, now)
	tr := engine.Progress(*target, st.Account, obs.eng, now, s.policy())
	if tr.Outcome == engine.KeepWaiting {
		return st, nil
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(target.Repo, target.PR)
		if r == nil || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
			return ErrNoChange
		}
		return s.applyTransition(st, r, tr, now)
	})
	if err != nil {
		return st, err
	}
	s.sync(ctx, updated)
	return updated, nil
}

func firedOrEnqueuedAt(r Round) time.Time {
	if r.FiredAt != nil {
		return *r.FiredAt
	}
	return r.EnqueuedAt
}

// applyFire executes a DecideFire verdict.
func (s *Service) applyFire(ctx context.Context, round Round, obs engine.Observation, d engine.FireDecision, now time.Time) (PumpResult, error) {
	switch d.Verdict {
	case engine.FireDrop:
		return s.abandonRound(ctx, round, "pr closed", "skipped")
	case engine.FireDedupe:
		return s.dedupeRound(ctx, round, now, d.Reason)
	case engine.FireSupersede:
		return s.supersedeRound(ctx, round, obs.Head, now)
	case engine.FireAdopt:
		return s.fireRound(ctx, round, obs, false, d.AdoptCommandID, d.AdoptAt, d.Reason, d.PostCodex, now)
	case engine.FirePost:
		return s.fireRound(ctx, round, obs, true, 0, time.Time{}, "", d.PostCodex, now)
	default: // FireNo
		return PumpResult{Action: mapFireNo(d.Reason), Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: d.Reason}, nil
	}
}

func mapFireNo(reason string) string {
	switch {
	case strings.Contains(reason, "could not read head"):
		return "skipped"
	case strings.Contains(reason, "min interval"):
		return "min_interval"
	case strings.Contains(reason, "account blocked"):
		return "blocked"
	case strings.Contains(reason, "fire slot busy"):
		return "lost_race"
	default:
		return "waiting"
	}
}

// abandonRound ends a round (closed/merged PR) without consuming review
// readiness. The existence check makes a concurrent cancel a benign lost race.
func (s *Service) abandonRound(ctx context.Context, round Round, reason, action string) (PumpResult, error) {
	result := PumpResult{Action: action, Repo: round.Repo, PR: round.PR, Reason: reason}
	if s.cfg.DryRun {
		return result, nil
	}
	ended := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		if st.Round(round.Repo, round.PR) == nil {
			return ErrNoChange
		}
		st.EndRound(round.Repo, round.PR, reason)
		releaseSlot(st, QueueKey(round.Repo, round.PR))
		ended = true
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	if !ended {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, updated)
	return result, nil
}

// dedupeRound completes a not-yet-fired round because the bot already reviewed
// its head, leaving the completed round as the dedupe marker (v2's Fired[key]).
func (s *Service) dedupeRound(ctx context.Context, round Round, now time.Time, reason string) (PumpResult, error) {
	result := PumpResult{Action: "deduped", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}
	if s.cfg.DryRun {
		return result, nil
	}
	deduped := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		deduped = false
		r := st.Round(round.Repo, round.PR)
		if r == nil || r.Head != round.Head || !r.FireEligible(now) {
			return ErrNoChange
		}
		if err := r.Dedupe(now); err != nil {
			return err
		}
		st.PutRound(*r)
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
	return result, nil
}

// supersedeRound retargets a queued round whose live head moved since it was
// enqueued; the fresh round fires on a later pump.
func (s *Service) supersedeRound(ctx context.Context, round Round, head string, now time.Time) (PumpResult, error) {
	result := PumpResult{Action: "requeued", Repo: round.Repo, PR: round.PR, Head: head, Reason: "head moved"}
	if s.cfg.DryRun || head == "" {
		result.Action = "skipped"
		return result, nil
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(round.Repo, round.PR)
		if r == nil || r.Head == head {
			return ErrNoChange
		}
		_, err := st.Supersede(round.Repo, round.PR, head, now)
		return err
	})
	if err != nil {
		return PumpResult{}, err
	}
	s.sync(ctx, updated)
	return result, nil
}

// fireRound posts (or adopts) the review command and records the fire on the
// round, reserving the global slot under compare-and-swap. When postCodex, it
// also posts the Codex review command alongside (non-fatal on failure — the
// self-heal path retries).
func (s *Service) fireRound(ctx context.Context, round Round, obs engine.Observation, post bool, adoptID int64, adoptAt time.Time, reason string, postCodex bool, now time.Time) (PumpResult, error) {
	key := QueueKey(round.Repo, round.PR)
	if s.cfg.DryRun {
		return PumpResult{Action: "dry_run", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}, nil
	}
	token := randomToken()

	if !post {
		// Adopt an already-posted command: reserve the slot and record the fire in
		// one write (no network post in between).
		firedAt := adoptAt.UTC()
		if firedAt.IsZero() {
			firedAt = now
		}
		recorded := false
		updated, err := s.store.Update(ctx, func(st *State) error {
			recorded = false
			if st.FireSlot != nil {
				return ErrNoChange
			}
			r := st.Round(round.Repo, round.PR)
			if r == nil || !r.FireEligible(now) {
				return ErrNoChange
			}
			if err := r.Reserve(token, s.cfg.Host, now); err != nil {
				return err
			}
			if err := r.Fire(adoptID, firedAt); err != nil {
				return err
			}
			lf := firedAt
			st.LastFired = &lf
			dl := firedAt.Add(s.cfg.FeedbackWaitTimeout)
			r.WaitDeadline = &dl
			st.Warn = ""
			st.FireSlot = &FireSlot{Key: key, Token: token, Since: now}
			st.PutRound(*r)
			recorded = true
			return nil
		})
		if err != nil {
			return PumpResult{}, err
		}
		if !recorded {
			return PumpResult{Action: "lost_race"}, nil
		}
		s.sync(ctx, updated)
		if s.log != nil {
			s.log.Printf("fire %s@%s (adopted existing review command)", key, round.Head)
		}
		if postCodex {
			s.fireCodexReview(ctx, round)
		}
		return PumpResult{Action: "fired", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: reason}, nil
	}

	// Reserve the slot, then post the command.
	reserved, err := s.store.Update(ctx, func(st *State) error {
		if st.FireSlot != nil {
			return ErrNoChange
		}
		r := st.Round(round.Repo, round.PR)
		if r == nil || !r.FireEligible(now) {
			return ErrNoChange
		}
		if err := r.Reserve(token, s.cfg.Host, now); err != nil {
			return err
		}
		st.FireSlot = &FireSlot{Key: key, Token: token, Since: now}
		st.PutRound(*r)
		return nil
	})
	if err != nil {
		return PumpResult{}, err
	}
	if reserved.FireSlot == nil || reserved.FireSlot.Token != token {
		return PumpResult{Action: "lost_race"}, nil
	}
	s.sync(ctx, reserved)

	comment, err := s.gh.PostIssueComment(ctx, round.Repo, round.PR, s.cfg.ReviewCommand)
	if err != nil {
		updated, uerr := s.store.Update(ctx, func(st *State) error {
			r := st.Round(round.Repo, round.PR)
			if r == nil || r.Token != token {
				return ErrNoChange
			}
			if rerr := r.ReleaseToQueue("failed to post review command: "+err.Error(), now); rerr != nil {
				return rerr
			}
			releaseSlot(st, key)
			st.Warn = "failed to post review command: " + err.Error()
			st.PutRound(*r)
			return nil
		})
		if uerr == nil {
			s.sync(ctx, updated)
		}
		return PumpResult{Action: "post_failed", Repo: round.Repo, PR: round.PR, Head: round.Head, Reason: err.Error()}, err
	}
	// Baseline the fire on the comment's GitHub timestamp, not a local clock that
	// may run ahead of GitHub's — a completion landing in the same second must
	// still count against a strict After check.
	firedAt := comment.CreatedAt.UTC()
	if firedAt.IsZero() {
		firedAt = now
	}
	// Post the Codex command before recording so its id lands in the same fire
	// write. A failed post returns 0 (logged) and the self-heal path retries.
	var codexID int64
	if postCodex {
		codexID = s.postCodexReviewComment(ctx, round)
	}
	updated, err := s.recordFire(ctx, round, token, comment.ID, codexID, firedAt, now)
	if err != nil {
		if errors.Is(err, ErrNoChange) {
			return PumpResult{Action: "lost_race"}, nil
		}
		return PumpResult{}, err
	}
	s.sync(ctx, updated)
	if s.log != nil {
		s.log.Printf("fire %s@%s (posted %s)", key, round.Head, strings.TrimSpace(s.cfg.ReviewCommand))
	}
	return PumpResult{Action: "fired", Repo: round.Repo, PR: round.PR, Head: round.Head}, nil
}

// recordFire records the posted command on the reserved round, with a 30s retry
// on a transient state-write failure so a fired command is never lost. codexID
// is the Codex command comment posted alongside (0 when none), recorded in the
// same write.
func (s *Service) recordFire(ctx context.Context, round Round, token string, commandID, codexID int64, firedAt, now time.Time) (State, error) {
	record := func(c context.Context) (State, bool, error) {
		recorded := false
		st, err := s.store.Update(c, func(st *State) error {
			recorded = false
			r := st.Round(round.Repo, round.PR)
			if r == nil || st.FireSlot == nil || st.FireSlot.Token != token || r.Token != token {
				return ErrNoChange
			}
			if err := r.Fire(commandID, firedAt); err != nil {
				return err
			}
			if codexID != 0 {
				r.CodexCommandID = codexID
			}
			lf := firedAt
			st.LastFired = &lf
			dl := firedAt.Add(s.cfg.FeedbackWaitTimeout)
			r.WaitDeadline = &dl
			st.Warn = ""
			st.PutRound(*r)
			recorded = true
			return nil
		})
		return st, recorded, err
	}
	st, recorded, err := record(ctx)
	if err != nil {
		retryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, recorded, err = record(retryCtx)
	}
	if err != nil {
		return State{}, err
	}
	if !recorded {
		return st, ErrNoChange
	}
	return st, nil
}

// postCodexReviewComment posts the Codex review command and returns its comment
// id, or 0 on failure. A failed post is non-fatal: it logs and leaves
// CodexCommandID unset so a later pump's self-heal retries. The fresh-fire path
// folds the returned id into recordFire's write.
func (s *Service) postCodexReviewComment(ctx context.Context, round Round) int64 {
	comment, err := s.gh.PostIssueComment(ctx, round.Repo, round.PR, s.cfg.CodexCommand)
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: Codex review command post failed for %s@%s: %v (will retry on a later pump)", QueueKey(round.Repo, round.PR), round.Head, err)
		}
		return 0
	}
	if s.log != nil {
		s.log.Printf("fire %s@%s (posted %s)", QueueKey(round.Repo, round.PR), round.Head, strings.TrimSpace(s.cfg.CodexCommand))
	}
	return comment.ID
}

// fireCodexReview posts the Codex review command for an already-fired round and
// records its id under CAS. It is used by the adopt fire path and the self-heal
// retry (the fresh-post path records the id inside recordFire instead). The CAS
// guard (same head, CodexCommandID still unset) makes a concurrent post benign.
func (s *Service) fireCodexReview(ctx context.Context, round Round) {
	codexID := s.postCodexReviewComment(ctx, round)
	if codexID == 0 {
		return
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		r := st.Round(round.Repo, round.PR)
		if r == nil || r.Head != round.Head || r.CodexCommandID != 0 {
			return ErrNoChange
		}
		r.CodexCommandID = codexID
		st.PutRound(*r)
		return nil
	})
	if err != nil {
		if s.log != nil && !errors.Is(err, ErrNoChange) {
			s.log.Printf("warning: failed to record Codex command %d for %s: %v", codexID, QueueKey(round.Repo, round.PR), err)
		}
		return
	}
	s.sync(ctx, updated)
}

// selfHealCodex re-posts the Codex review command for a fired/reviewing round
// whose initial Codex post failed (CodexCommandID still 0). It runs on the
// daemon's progress/sweep paths; idempotence comes from the observation — Codex
// evidence, a live `@codex review` command, or an account that reviews on its
// own all suppress it — not a retry counter.
func (s *Service) selfHealCodex(ctx context.Context, round Round, obs engine.Observation, now time.Time) {
	if s.cfg.DryRun || round.CodexCommandID != 0 || round.FiredAt == nil || obs.Head != round.Head {
		return
	}
	commandPresent := engine.CodexCommandSince(obs, round.FiredAt.UTC())
	if !engine.DecideCodexPost(round, obs, s.policy(), commandPresent) {
		return
	}
	s.fireCodexReview(ctx, round)
}

func (s *Service) Cancel(ctx context.Context, repo string, pr int) error {
	repo = NormalizeRepo(repo)
	state, err := s.store.Update(ctx, func(st *State) error {
		if st.Round(repo, pr) == nil {
			return ErrNoChange
		}
		st.EndRound(repo, pr, "cancelled")
		releaseSlot(st, QueueKey(repo, pr))
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
	// late "account blocked" reply isn't ignored for the full TTL.
	if state.Account.CalibAskedAt == nil && state.Account.CheckedAt != nil && now.Sub(*state.Account.CheckedAt) < s.cfg.CalibrationTTL {
		return state, nil
	}
	quota, err := s.readQuota(ctx, s.calibrationIssue(state), now, state.Account.CalibAskedAt)
	if err != nil {
		return state, err
	}
	updated, err := s.store.Update(ctx, func(st *State) error {
		if st.Account.CalibAskedAt == nil && st.Account.CheckedAt != nil && time.Since(*st.Account.CheckedAt) < s.cfg.CalibrationTTL {
			return ErrNoChange
		}
		// A fresh reading replaces the whole quota; carry the account-quota comment
		// identity over so the engine can still recognise an edited comment it
		// already accounted for.
		rlID, rlUpdated := st.Account.RLCommentID, st.Account.RLCommentUpdated
		st.Account = quota
		if st.Account.RLCommentID == 0 {
			st.Account.RLCommentID = rlID
			st.Account.RLCommentUpdated = rlUpdated
		}
		if st.Warn == warnRateLimited && (quota.BlockedUntil == nil || !quota.BlockedUntil.After(now)) {
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

// calibrationIssue returns the calibration issue to probe: the rotated
// replacement recorded in state after a cap wedge, or the configured one.
func (s *Service) calibrationIssue(state State) int {
	if state.CalibrationIssue > 0 {
		return state.CalibrationIssue
	}
	return s.cfg.CalibrationPR
}

const calibrationIssueBody = "crq probes CodeRabbit's account-wide review quota here with `" + dialect.DefaultRateLimitCommand + "` so it never spends a real review to calibrate. Auto-created after a prior calibration thread hit GitHub's 2500-comment cap. Managed by crq — safe to leave alone."

// rotateCalibration creates a fresh calibration issue and records its number in
// the shared state so the whole fleet abandons a thread that hit GitHub's hard
// 2500-comment cap.
func (s *Service) rotateCalibration(ctx context.Context, oldIssue int) (int, error) {
	issue, err := s.gh.CreateIssue(ctx, s.cfg.GateRepo, "crq account-quota calibration", calibrationIssueBody)
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

func (s *Service) readQuota(ctx context.Context, issue int, now time.Time, pendingAsked *time.Time) (AccountQuota, error) {
	quota := AccountQuota{Scope: strings.Join(s.cfg.Scope, ","), Source: "calibrate", CheckedAt: &now}
	cutoff := now.Add(-s.cfg.CalibrationTTL)
	keepAfter := now.Add(-2 * s.cfg.CalibrationTTL)
	if reply, ok, err := s.latestCalibrationReply(ctx, issue, cutoff); err != nil {
		return quota, err
	} else if ok {
		remaining, reset := dialect.ParseQuota(reply.Body, reply.UpdatedAt)
		quota.Remaining = remaining
		quota.BlockedUntil = reset
		s.pruneCalibration(ctx, issue, keepAfter, 80)
		return quota, nil
	}
	// A probe from a previous call is still pending and not yet stale, and no
	// reply to it was found: keep waiting for its (possibly late) reply instead of
	// posting another probe every cycle.
	if pendingAsked != nil && pendingAsked.After(cutoff) {
		quota.CalibAskedAt = pendingAsked
		return quota, nil
	}
	asked, err := s.gh.PostIssueComment(ctx, s.cfg.GateRepo, issue, s.cfg.RateLimitCommand)
	if err != nil {
		// The calibration thread hit GitHub's 2500-comment cap. Prune old probe
		// comments and retry once; if pruning can't drop under the cap, rotate to a
		// fresh issue and retry there instead of failing every cycle.
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
			return quota, err
		}
	}
	quota.CalibAskedAt = &asked.CreatedAt
	for i := 0; i < 6; i++ {
		select {
		case <-ctx.Done():
			return quota, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		reply, ok, err := s.latestCalibrationReply(ctx, issue, asked.CreatedAt.Add(-time.Second))
		if err != nil {
			return quota, err
		}
		if ok {
			remaining, reset := dialect.ParseQuota(reply.Body, reply.UpdatedAt)
			quota.Remaining = remaining
			quota.BlockedUntil = reset
			quota.CalibAskedAt = nil
			s.pruneCalibration(ctx, issue, keepAfter, 80)
			return quota, nil
		}
	}
	return quota, nil
}

// pruneCalibration deletes crq's old calibration probe comments and CodeRabbit's
// replies so the thread never reaches GitHub's hard 2500-comment cap.
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
// one of crq's account-quota probes or a CodeRabbit auto-reply.
func (s *Service) isCalibrationNoise(c ghapi.IssueComment) bool {
	if strings.TrimSpace(c.Body) == strings.TrimSpace(s.cfg.RateLimitCommand) {
		return true
	}
	return s.isConfiguredBot(c.User.Login) && strings.Contains(c.Body, s.cfg.CalibrationMarker)
}

func (s *Service) latestCalibrationReply(ctx context.Context, issue int, after time.Time) (ghapi.IssueComment, bool, error) {
	comments, err := s.gh.ListIssueComments(ctx, s.cfg.GateRepo, issue)
	if err != nil {
		return ghapi.IssueComment{}, false, err
	}
	var best ghapi.IssueComment
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
	return dialect.NormalizeBotName(login) == dialect.NormalizeBotName(s.cfg.Bot)
}

// notBefore reports whether t is at or after baseline. GitHub timestamps are
// second-granular, so a bot completion in the same second as the trigger must
// still count — a strict After would miss it and refire a duplicate review.
func notBefore(t, baseline time.Time) bool { return !t.Before(baseline) }

func allReviewed(reviewedBy map[string]bool) bool {
	for _, reviewed := range reviewedBy {
		if !reviewed {
			return false
		}
	}
	return true
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
// closed nor merged), so a PR closed after it was queued is dropped instead of
// firing a review at a dead PR.
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

func (s *Service) sync(ctx context.Context, state State) {
	if s.log == nil || s.cfg.DashboardIssue <= 0 {
		return
	}
	if err := s.store.SyncDashboard(ctx, state); err != nil {
		s.log.Printf("warning: dashboard sync failed: %v", err)
	}
}

func randomToken() string {
	var buf [16]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// isCommentCapError reports whether err is GitHub's hard cap of 2500 comments per
// issue ("Commenting is disabled on issues with more than 2500 comments").
func isCommentCapError(err error) bool {
	var api *ghapi.APIError
	if !errors.As(err, &api) {
		return false
	}
	b := strings.ToLower(api.Body)
	return strings.Contains(b, "commenting is disabled") || strings.Contains(b, "more than 2500 comments")
}

// Wait enqueues repo#pr and pumps until a review fires for its head (code 0),
// current-head feedback is already available (code 3), the wait times out (code
// 2), or the PR is closed (code 2). The wait IS the round: a fired/reviewing
// round for the head is the in-flight wait, a completed round is the "already
// reviewed" dedup marker, and firedMarker/waitingHead read those states off the
// round rather than a separate wait record.
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
				if waitingHead(&state, repo, pr) == result.Head {
					return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
				}
				report, err := s.Feedback(ctx, repo, pr)
				if err != nil {
					return PumpResult{}, 1, err
				}
				if len(engine.FindingsOnHead(report.Findings, report.Head)) > 0 || allReviewed(report.ReviewedBy) {
					return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: result.Head}, 3, nil
				}
				// A completed round at this head with no real head review is a poisoned
				// dedup marker (a mistaken completion). Drop it and enqueue the real
				// replacement review.
				updated, err := s.store.Update(ctx, func(st *State) error {
					r := st.Round(repo, pr)
					if r == nil || r.Head != result.Head || r.Phase != PhaseCompleted {
						return ErrNoChange
					}
					st.EndRound(repo, pr, "repair: completed head lacked a real review")
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
			// The round stays active: policy holds this head until the slot and every
			// required reviewer finish.
			if len(engine.FindingsOnHead(report.Findings, report.Head)) > 0 {
				if s.log != nil {
					s.log.Printf("%s#%d feedback already available on %s; leaving review slot wait", repo, pr, report.Head)
				}
				return PumpResult{Action: "deduped", Repo: repo, PR: pr, Head: report.Head, Reason: "feedback already available"}, 3, nil
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
		if r := state.Round(repo, pr); r != nil && r.Phase == PhaseFired {
			return PumpResult{Action: "fired", Repo: repo, PR: pr, Head: r.Head}, 0, nil
		}
		if !containsActive(&state, repo, pr) {
			head, open, herr := s.pullHead(ctx, repo, pr)
			if herr == nil && !open {
				// PR closed/merged and dropped — nothing to review; stop the loop.
				return PumpResult{Action: "skipped", Repo: repo, PR: pr, Reason: "pr closed"}, 2, nil
			}
			if herr == nil && head != "" && firedMarker(&state, repo, pr) == head {
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
