package crq

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

type AutoOptions struct {
	Once        bool
	Incremental bool
}

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
		if err := s.autoReviewPass(ctx, opts); err != nil && s.log != nil {
			s.log.Printf("warning: autoreview pass failed: %v", err)
		}
		if _, err := s.Pump(ctx); err != nil && s.log != nil {
			s.log.Printf("warning: autoreview pump failed: %v", err)
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
		return false, err
	}
	if held {
		s.sync(ctx, state)
	}
	return held, nil
}

func (s *Service) autoReviewPass(ctx context.Context, opts AutoOptions) error {
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
	for _, target := range targets {
		if scanned >= s.cfg.AutoReviewMaxScan {
			return nil
		}
		prs, err := s.gh.SearchOpenPRs(ctx, target, byRepo, 1000)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if scanned >= s.cfg.AutoReviewMaxScan {
				return nil
			}
			repo := NormalizeRepo(pr.Repo)
			if repo == NormalizeRepo(s.cfg.GateRepo) || s.cfg.ExcludeRepos[repo] {
				continue
			}
			if len(s.cfg.AllowRepos) > 0 && !s.cfg.AllowRepos[repo] {
				continue
			}
			scanned++
			if err := s.enqueueIfNeedsReview(ctx, repo, pr.Number, opts.Incremental); err != nil && s.log != nil {
				s.log.Printf("warning: autoreview skipped %s#%d: %v", repo, pr.Number, err)
			}
		}
	}
	return nil
}

func (s *Service) enqueueIfNeedsReview(ctx context.Context, repo string, pr int, incremental bool) error {
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	if state.Contains(repo, pr) {
		return nil
	}
	head, err := s.headShort(ctx, repo, pr)
	if err != nil {
		return err
	}
	if state.Fired[QueueKey(repo, pr)] == head {
		return nil
	}
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return err
	}
	lastBotReview := ""
	for _, review := range reviews {
		if review.User.Login == s.cfg.Bot && review.CommitID != "" {
			lastBotReview = shortOID(review.CommitID)
		}
	}
	if incremental {
		if lastBotReview != head {
			_, err = s.Enqueue(ctx, repo, pr)
			return err
		}
		return nil
	}
	if lastBotReview != "" {
		return nil
	}
	comments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if comment.User.Login == s.cfg.Bot && strings.Contains(comment.Body, s.cfg.ReviewDoneMarker) {
			return nil
		}
	}
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return err
	}
	if strings.Contains(pull.Body, s.cfg.ReviewDoneMarker) {
		return nil
	}
	_, err = s.Enqueue(ctx, repo, pr)
	return err
}
