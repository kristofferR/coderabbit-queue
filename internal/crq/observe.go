package crq

import (
	"context"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
)

// observe is the single place that asks GitHub "what happened on this PR" and
// reduces it to an engine.Observation. The daemon's Pump builds it once for the
// slot round (Progress) and once for the next-eligible round (DecideFire), so
// the "is head reviewed?" duplication of v2 collapses to one implementation.
//
// round anchors the round-relative facts: reactions target its fired command,
// the adoption cutoff is its LastAttemptAt, and reactions/thumbs-up are fetched
// only for a round that has fired.
func (s *Service) observe(ctx context.Context, repo string, pr int, round *Round, now time.Time) (engine.Observation, error) {
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return engine.Observation{}, err
	}
	obs := engine.Observation{Open: pull.State == "open" && !pull.Merged}
	if obs.Open && len(pull.Head.SHA) >= 9 {
		obs.Head = pull.Head.SHA[:9]
	}
	if !obs.Open {
		// A closed PR needs no further facts; the engine drops/abandons the round.
		return obs, nil
	}

	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return engine.Observation{}, err
	}
	for _, review := range reviews {
		obs.Reviews = append(obs.Reviews, engine.ReviewSeen{
			Bot:         review.User.Login,
			ReviewID:    review.ID,
			Commit:      shortOID(review.CommitID),
			SubmittedAt: review.SubmittedAt,
		})
	}

	comments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		return engine.Observation{}, err
	}
	classifier := dialect.Classifier{CodeRabbit: s.cr, Bot: s.cfg.Bot, ReviewCommand: s.cfg.ReviewCommand}
	for _, c := range comments {
		obs.Events = append(obs.Events, classifier.Classify(c.User.Login, c.Body, c.ID, c.CreatedAt, c.UpdatedAt))
	}

	// Reactions and Codex thumbs-up only matter for a round that has fired.
	if round != nil && round.FiredAt != nil {
		cutoff := round.FiredAt.UTC()
		if round.CommandID != 0 {
			reactions, err := s.gh.ListCommentReactions(ctx, repo, round.CommandID)
			if err != nil {
				return engine.Observation{}, err
			}
			for _, reaction := range reactions {
				if s.isConfiguredBot(reaction.User.Login) {
					obs.Reacted = true
				}
				if isCurrentCodexThumbsUp(reaction, cutoff) {
					obs.CodexThumbsUp = true
				}
			}
		}
		if !obs.CodexThumbsUp && s.codexRelevant(obs) {
			reactions, err := s.gh.ListIssueReactions(ctx, repo, pr)
			if err != nil {
				return engine.Observation{}, err
			}
			for _, reaction := range reactions {
				if isCurrentCodexThumbsUp(reaction, cutoff) {
					obs.CodexThumbsUp = true
					break
				}
			}
		}
	}

	// Adoptable commands are only consulted for a fire-eligible round.
	if round != nil && round.FireEligible(now) {
		cmds, err := s.adoptableCommands(ctx, repo, pr, obs.Head, adoptCutoff(*round), pull, comments, reviews)
		if err != nil {
			return engine.Observation{}, err
		}
		obs.Commands = cmds
	}
	return obs, nil
}

// adoptCutoff is the earliest command timestamp a round may adopt: the most
// recent failed/abandoned attempt, so a stale command from before a requeue is
// never adopted.
func adoptCutoff(r Round) time.Time {
	if r.LastAttemptAt != nil {
		return r.LastAttemptAt.UTC()
	}
	return time.Time{}
}

// codexRelevant reports whether Codex participates in this round, so the extra
// issue-reactions fetch for a Codex thumbs-up is only spent when it can matter.
func (s *Service) codexRelevant(obs engine.Observation) bool {
	if dialect.HasCodexBot(s.cfg.RequiredBots) {
		return true
	}
	for _, review := range obs.Reviews {
		if dialect.IsCodexBot(review.Bot) {
			return true
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCodexClean || dialect.IsCodexBot(ev.Bot) {
			return true
		}
	}
	return false
}

// adoptableCommands ports v2's existingReviewCommand: it returns the newest
// review-command comment safe to adopt as an already-posted fire, or none. The
// cutoffs (LastAttemptAt floor, head-commit date, force-push, already-answered)
// are applied here so the engine only picks the newest survivor.
func (s *Service) adoptableCommands(ctx context.Context, repo string, pr int, head string, notBeforeCutoff time.Time, pull Pull, comments []IssueComment, reviews []Review) ([]engine.CommandSeen, error) {
	command := strings.TrimSpace(s.cfg.ReviewCommand)
	if command == "" {
		return nil, nil
	}
	// The head-guard and cutoff lookups cost REST/GraphQL calls; skip them
	// entirely in the common case of no command comment on the PR at all.
	hasCandidate := false
	for _, comment := range comments {
		if strings.TrimSpace(comment.Body) == command {
			hasCandidate = true
			break
		}
	}
	if !hasCandidate {
		return nil, nil
	}
	cutoff := notBeforeCutoff
	if pull.Head.SHA != "" {
		if shortOID(pull.Head.SHA) != head {
			return nil, nil
		}
		commit, err := s.gh.GetCommit(ctx, repo, pull.Head.SHA)
		if err != nil {
			if _, ok := rateLimitWait(err); ok {
				return nil, err
			}
			// No head-commit cutoff available (unreadable/404 head): skip adoption
			// rather than wedge the queue — the worst case is posting a command that
			// already exists, the pre-adoption behavior.
			return nil, nil
		}
		if commit.Committer.Date.After(cutoff) {
			cutoff = commit.Committer.Date
		}
	}
	// A force-push can point the PR at a commit object whose committer date
	// predates commands made for an earlier head, so any command older than the
	// last force-push belongs to a previous head and must not be adopted.
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
		return nil, nil
	}
	// A command the bot has already answered with a review belongs to a completed
	// round for an earlier head; adopting it would mark the new head fired without
	// reviewing it. Skip adoption — the worst case is a duplicate command.
	for _, review := range reviews {
		if s.isConfiguredBot(review.User.Login) && !review.SubmittedAt.Before(bestAt) {
			return nil, nil
		}
	}
	if s.reviewCommandHasCompletionReply(comments, reviews, best.ID) {
		return nil, nil
	}
	return []engine.CommandSeen{{ID: best.ID, CreatedAt: best.CreatedAt, UpdatedAt: best.UpdatedAt}}, nil
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
