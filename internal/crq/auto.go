package crq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type AutoOptions struct {
	Once        bool
	Incremental bool
}

// errLostLeadership aborts a pass when a standby stole the lease mid-scan.
var errLostLeadership = errors.New("lost autoreview leadership mid-pass")

func (s *Service) AutoReview(ctx context.Context, opts AutoOptions) error {
	owner := fmt.Sprintf("host=%s pid=%d", s.cfg.Host, os.Getpid())
	token := randomToken()
	for {
		held, err := s.acquireLeader(ctx, owner, token)
		if err != nil {
			if _, ok := rateLimitWait(err); ok {
				if cont, serr := s.sleepRateLimit(ctx, opts, "leader", err); serr != nil || !cont {
					return serr
				}
				continue
			}
			return err
		}
		if !held {
			if opts.Once {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.cfg.AutoReviewPoll):
				continue
			}
		}
		passErr := s.autoReviewPass(ctx, opts, owner, token)
		var passFailure error
		if errors.Is(passErr, errLostLeadership) {
			if s.log != nil {
				s.log.Printf("autoreview: lost leadership mid-pass; standing by")
			}
		} else {
			// A rate-limited pass means the following API calls will keep failing —
			// sleep out the window instead of immediately pumping and re-scanning,
			// which would just hammer the quota. Skip Pump entirely in that case.
			if _, ok := rateLimitWait(passErr); ok {
				if cont, serr := s.sleepRateLimit(ctx, opts, "pass", passErr); serr != nil || !cont {
					if opts.Once {
						return s.finishAutoReviewOnce(ctx, token, serr)
					}
					return serr
				}
				continue
			}
			if passErr != nil && s.log != nil {
				s.log.Printf("warning: autoreview pass failed: %v", passErr)
			}
			passFailure = passErr
			if _, err := s.Pump(ctx); err != nil {
				if _, ok := rateLimitWait(err); ok {
					if cont, serr := s.sleepRateLimit(ctx, opts, "pump", err); serr != nil || !cont {
						if opts.Once {
							return s.finishAutoReviewOnce(ctx, token, serr)
						}
						return serr
					}
					continue
				}
				if s.log != nil {
					s.log.Printf("warning: autoreview pump failed: %v", err)
				}
				if passFailure == nil {
					passFailure = err
				}
			}
		}
		if opts.Once {
			// A one-shot run must surface a real (non-rate-limit) scan/pump failure —
			// e.g. a permission or owner-lookup error — so cron/CI doesn't see success
			// when nothing was scanned or enqueued. The daemon keeps going (logged).
			return s.finishAutoReviewOnce(ctx, token, passFailure)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.AutoReviewPoll):
		}
	}
}

func (s *Service) finishAutoReviewOnce(ctx context.Context, token string, err error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if rerr := s.releaseLeader(ctx, token); err == nil && rerr != nil {
		return rerr
	}
	return err
}

// rateLimitBackoff bounds how long the autoreview daemon sleeps when GitHub
// rate-limits it: at least one poll interval, plus a small buffer past the
// reset, capped at an hour so a bogus reset header can't wedge the daemon.
func (s *Service) rateLimitBackoff(wait time.Duration) time.Duration {
	if wait <= 0 {
		wait = s.cfg.AutoReviewPoll
	}
	wait += 5 * time.Second
	if wait < s.cfg.AutoReviewPoll {
		wait = s.cfg.AutoReviewPoll
	}
	if wait > time.Hour {
		wait = time.Hour
	}
	return wait
}

// sleepRateLimit waits out a GitHub rate-limit window that an autoreview
// leader/pass/pump step hit, using the same bounded backoff as the leader path so
// a throttle pauses the daemon instead of spinning failing API calls. cause must
// be a rate-limit error (the caller checks rateLimitWait first). It returns
// cont=false (nil error) when opts.Once means we should stop after the wait, and a
// non-nil error only when the context is cancelled mid-wait.
func (s *Service) sleepRateLimit(ctx context.Context, opts AutoOptions, stage string, cause error) (cont bool, err error) {
	wait, _ := rateLimitWait(cause)
	wait = s.rateLimitBackoff(wait)
	if s.log != nil {
		s.log.Printf("autoreview: %s rate-limited (%v); sleeping %s before next pass", stage, cause, wait.Round(time.Second))
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(wait):
	}
	if opts.Once {
		return false, nil
	}
	return true, nil
}

func (s *Service) acquireLeader(ctx context.Context, owner, token string) (bool, error) {
	state, held, err := s.renewLeader(ctx, owner, token)
	if err != nil {
		return false, err
	}
	if held {
		s.sync(ctx, state)
	}
	return held, nil
}

func (s *Service) releaseLeader(ctx context.Context, token string) error {
	released := false
	state, err := s.store.Update(ctx, func(st *State) error {
		if st.Leader == nil || st.Leader.Token != token {
			return ErrNoChange
		}
		st.Leader = nil
		released = true
		return nil
	})
	if err != nil {
		return err
	}
	if released {
		s.sync(ctx, state)
	}
	return nil
}

// renewLeader claims or extends the leader lease via compare-and-swap on the
// state ref. It does not sync the dashboard, so it's cheap enough to call as an
// in-pass heartbeat. held is false when another live lease holder owns it.
func (s *Service) renewLeader(ctx context.Context, owner, token string) (State, bool, error) {
	now := time.Now().UTC()
	expires := now.Add(s.cfg.LeaderTTL)
	held := false
	state, err := s.store.Update(ctx, func(st *State) error {
		if st.Leader != nil && st.Leader.ExpiresAt.After(now) && st.Leader.Token != token {
			held = false
			return ErrNoChange
		}
		st.Leader = &LeaderLease{Owner: owner, Token: token, ExpiresAt: expires, UpdatedAt: now}
		held = true
		return nil
	})
	if err != nil {
		return State{}, false, err
	}
	return state, held, nil
}

func (s *Service) autoReviewPass(ctx context.Context, opts AutoOptions, owner, token string) error {
	targets := s.cfg.Scope
	byRepo := false
	if len(s.cfg.AllowRepos) > 0 {
		targets = make([]string, 0, len(s.cfg.AllowRepos))
		for repo := range s.cfg.AllowRepos {
			targets = append(targets, repo)
		}
		byRepo = true
	}
	// Load the queue snapshot once per pass and reuse it across candidates: a
	// git-backed Load is GetRef+GetCommit+GetTree+GetBlob, so reloading it per PR
	// would burn the shared REST quota on a large scan. The heartbeat refreshes it,
	// and enqueueBatch re-checks Contains under CAS, so a slightly stale snapshot
	// during collection is safe.
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	var candidates []SearchPR
	lastBeat := time.Now()
	for _, target := range targets {
		// Per-target scan budget so one large scope can't consume the whole budget
		// and starve later scopes when CRQ_SCOPE lists multiple owners/orgs.
		scanned := 0
		// Stream results and stop once the post-filter scan budget is spent, so
		// excluded/gate-repo results can't crowd out in-scope PRs (a fixed pre-filter
		// limit would never reach them) while we still don't over-fetch pages.
		err := s.gh.EachOpenPR(ctx, target, byRepo, func(pr SearchPR) (bool, error) {
			if scanned >= s.cfg.AutoReviewMaxScan {
				return true, nil
			}
			repo := NormalizeRepo(pr.Repo)
			if repo == NormalizeRepo(s.cfg.GateRepo) || s.cfg.ExcludeRepos[repo] {
				return false, nil
			}
			if len(s.cfg.AllowRepos) > 0 && !s.cfg.AllowRepos[repo] {
				return false, nil
			}
			scanned++
			// Heartbeat: renew the lease partway through a long pass so a standby
			// can't steal it mid-scan and cause brief double-leadership (#4).
			if s.cfg.LeaderTTL > 0 && time.Since(lastBeat) >= s.cfg.LeaderTTL/2 {
				st, held, herr := s.renewLeader(ctx, owner, token)
				if herr != nil {
					return false, herr
				}
				if !held {
					return false, errLostLeadership
				}
				state = st // reuse the freshly written snapshot for later candidates
				lastBeat = time.Now()
			}
			need, nerr := s.needsReview(ctx, state, repo, pr.Number, opts.Incremental)
			if nerr != nil {
				// A rate limit must abort the pass so AutoReview's outer backoff kicks
				// in, instead of scanning the rest of the candidates under the same
				// throttle (and skipping them until a later poll).
				if IsRateLimited(nerr) {
					return false, nerr
				}
				if s.log != nil {
					s.log.Printf("warning: autoreview skipped %s#%d: %v", repo, pr.Number, nerr)
				}
				return false, nil
			}
			if need {
				candidates = append(candidates, SearchPR{Repo: repo, Number: pr.Number})
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}
	// One batched write for the whole pass instead of N (#2).
	return s.enqueueBatch(ctx, candidates)
}

// needsReview reports whether an open PR should be enqueued for review: not
// already queued/fired for its current head, and (incremental) the bot hasn't
// reviewed that head yet, or (first-review) it has never been reviewed. It uses
// the caller's preloaded queue snapshot for the queued/fired checks so a pass
// doesn't reload git-backed state once per candidate.
func (s *Service) needsReview(ctx context.Context, state State, repo string, pr int, incremental bool) (bool, error) {
	if state.Contains(repo, pr) {
		return false, nil
	}
	head, err := s.headShort(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	if state.AwaitingFeedback[QueueKey(repo, pr)].Head == head {
		return false, nil
	}
	if state.Fired[QueueKey(repo, pr)] == head {
		return false, nil
	}
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	bot := normalizeBotName(s.cfg.Bot)
	lastBotReview := ""
	for _, review := range reviews {
		if normalizeBotName(review.User.Login) == bot && review.CommitID != "" {
			lastBotReview = shortOID(review.CommitID)
		}
	}
	if incremental {
		return lastBotReview != head, nil
	}
	if lastBotReview != "" {
		return false, nil
	}
	comments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if normalizeBotName(comment.User.Login) == bot && strings.Contains(comment.Body, s.cfg.ReviewDoneMarker) {
			return false, nil
		}
	}
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	if strings.Contains(pull.Body, s.cfg.ReviewDoneMarker) {
		return false, nil
	}
	return true, nil
}
