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
			if wait, ok := rateLimitWait(err); ok {
				wait = s.rateLimitBackoff(wait)
				if s.log != nil {
					s.log.Printf("autoreview: %v; sleeping %s before next pass", err, wait.Round(time.Second))
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
				if opts.Once {
					return nil
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
		if errors.Is(passErr, errLostLeadership) {
			if s.log != nil {
				s.log.Printf("autoreview: lost leadership mid-pass; standing by")
			}
		} else {
			if passErr != nil && s.log != nil {
				s.log.Printf("warning: autoreview pass failed: %v", passErr)
			}
			if _, err := s.Pump(ctx); err != nil && s.log != nil {
				s.log.Printf("warning: autoreview pump failed: %v", err)
			}
		}
		if opts.Once {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.AutoReviewPoll):
		}
	}
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
	scanned := 0
	var candidates []SearchPR
	lastBeat := time.Now()
	for _, target := range targets {
		if scanned >= s.cfg.AutoReviewMaxScan {
			break
		}
		prs, err := s.gh.SearchOpenPRs(ctx, target, byRepo, 1000)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if scanned >= s.cfg.AutoReviewMaxScan {
				break
			}
			repo := NormalizeRepo(pr.Repo)
			if repo == NormalizeRepo(s.cfg.GateRepo) || s.cfg.ExcludeRepos[repo] {
				continue
			}
			if len(s.cfg.AllowRepos) > 0 && !s.cfg.AllowRepos[repo] {
				continue
			}
			scanned++
			// Heartbeat: renew the lease partway through a long pass so a standby
			// can't steal it mid-scan and cause brief double-leadership (#4).
			if s.cfg.LeaderTTL > 0 && time.Since(lastBeat) >= s.cfg.LeaderTTL/2 {
				_, held, err := s.renewLeader(ctx, owner, token)
				if err != nil {
					return err
				}
				if !held {
					return errLostLeadership
				}
				lastBeat = time.Now()
			}
			need, err := s.needsReview(ctx, repo, pr.Number, opts.Incremental)
			if err != nil {
				if s.log != nil {
					s.log.Printf("warning: autoreview skipped %s#%d: %v", repo, pr.Number, err)
				}
				continue
			}
			if need {
				candidates = append(candidates, SearchPR{Repo: repo, Number: pr.Number})
			}
		}
	}
	// One batched write for the whole pass instead of N (#2).
	return s.enqueueBatch(ctx, candidates)
}

// needsReview reports whether an open PR should be enqueued for review: not
// already queued/fired for its current head, and (incremental) the bot hasn't
// reviewed that head yet, or (first-review) it has never been reviewed.
func (s *Service) needsReview(ctx context.Context, repo string, pr int, incremental bool) (bool, error) {
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return false, err
	}
	if state.Contains(repo, pr) {
		return false, nil
	}
	head, err := s.headShort(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	if state.Fired[QueueKey(repo, pr)] == head {
		return false, nil
	}
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	lastBotReview := ""
	for _, review := range reviews {
		if review.User.Login == s.cfg.Bot && review.CommitID != "" {
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
		if comment.User.Login == s.cfg.Bot && strings.Contains(comment.Body, s.cfg.ReviewDoneMarker) {
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
