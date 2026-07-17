package crq

import (
	"context"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// observation bundles the pure engine.Observation the decision functions
// consume with the raw GitHub payloads Feedback's findings extraction needs, so
// a single fetch serves both the daemon (Pump: DecideFire/Progress) and the
// loop (Feedback: engine.Completion + finding parsing) without a second path.
type observation struct {
	eng      engine.Observation
	pull     ghapi.Pull
	reviews  []ghapi.Review
	comments []ghapi.IssueComment
}

// observe is the single place that asks GitHub "what happened on this PR" and
// reduces it to an engine.Observation (plus the raw reviews/comments Feedback
// parses into findings). The daemon's Pump builds it for the slot round
// (Progress) and the next-eligible round (DecideFire); Feedback builds it for
// the current round — so the "is head reviewed?" duplication of v2 collapses to
// one implementation.
//
// round anchors the round-relative facts: reactions target its fired command,
// the adoption cutoff is its LastAttemptAt, and reactions/thumbs-up are fetched
// only for a round that has fired.
func (s *Service) observe(ctx context.Context, repo string, pr int, round *Round, now time.Time) (observation, error) {
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return observation{}, err
	}
	o := observation{pull: pull}
	o.eng.Open = pull.State == "open" && !pull.Merged
	if o.eng.Open && len(pull.Head.SHA) >= 9 {
		o.eng.Head = pull.Head.SHA[:9]
	}

	// Reviews and issue comments are fetched even for a closed PR: the daemon's
	// Progress/DecideFire abandon it regardless, but Feedback still surfaces its
	// findings, and the extra two reads on a to-be-dropped round are negligible.
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return observation{}, err
	}
	o.reviews = reviews
	for _, review := range reviews {
		// CodeRabbit submits empty-bodied COMMENTED review objects as carriers
		// for its inline-comment batches, minutes before the real review (the
		// one with an "Actionable comments posted" body) lands. A shell is not
		// review evidence: counting one converged a round with zero findings at
		// 17:26 while the real review was still posting until 17:32.
		if strings.TrimSpace(review.Body) == "" && strings.EqualFold(review.State, "COMMENTED") {
			continue
		}
		o.eng.Reviews = append(o.eng.Reviews, engine.ReviewSeen{
			Bot:         review.User.Login,
			ReviewID:    review.ID,
			Commit:      dialect.ShortOID(review.CommitID),
			SubmittedAt: review.SubmittedAt,
		})
	}

	comments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		return observation{}, err
	}
	o.comments = comments
	classifier := dialect.Classifier{CodeRabbit: s.cr, Bot: s.cfg.Bot, ReviewCommand: s.cfg.ReviewCommand, CodexCommand: s.cfg.CodexCommand}
	for _, c := range comments {
		o.eng.Events = append(o.eng.Events, classifier.Classify(c.User.Login, c.Body, c.ID, c.CreatedAt, c.UpdatedAt))
	}

	// Reactions and Codex thumbs-up only matter for a round that has fired.
	if round != nil && round.FiredAt != nil {
		cutoff := round.FiredAt.UTC()
		if round.CommandID != 0 {
			reactions, err := s.gh.ListCommentReactions(ctx, repo, round.CommandID)
			if err != nil {
				return observation{}, err
			}
			for _, reaction := range reactions {
				if s.isConfiguredBot(reaction.User.Login) {
					o.eng.Reacted = true
				}
				if isCurrentCodexThumbsUp(reaction, cutoff) {
					o.eng.CodexThumbsUp = true
				}
			}
		}
		if !o.eng.CodexThumbsUp && s.codexRelevant(o.eng) {
			reactions, err := s.gh.ListIssueReactions(ctx, repo, pr)
			if err != nil {
				return observation{}, err
			}
			for _, reaction := range reactions {
				if isCurrentCodexThumbsUp(reaction, cutoff) {
					o.eng.CodexThumbsUp = true
					break
				}
			}
		}
	}

	// Codex activity is derived from the same snapshot: whether Codex reviews the
	// PR unprompted (drives the fire decision) and whether it participates in the
	// current round (drives the dynamic completion gate).
	o.eng.CodexAutoActive = engine.CodexAutoActive(o.eng)
	if round != nil {
		o.eng.CodexActiveThisRound = engine.CodexActiveThisRound(*round, o.eng)
	}

	// Adoptable commands are only consulted for a fire-eligible round.
	if round != nil && round.FireEligible(now) {
		cr, codex, err := s.reviewCommands(ctx, repo, pr, o.eng, adoptCutoff(*round), pull, comments, reviews)
		if err != nil {
			return observation{}, err
		}
		o.eng.Commands = cr
		o.eng.CodexCommands = codex
	}
	return o, nil
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

// reviewCommands ports v2's existingReviewCommand and extends it to Codex. It
// returns the newest CodeRabbit command safe to adopt as an already-posted fire
// (cr) and the live `@codex review` commands present for the head (codex). Both
// share ONE cutoff computation (LastAttemptAt floor, head-commit date,
// force-push) so a stale command from a previous head is excluded from both, and
// the head-guard/cutoff lookups are skipped entirely when neither command is on
// the PR. The Codex list is only gathered when Codex gates the round, since only
// a configured-required Codex is ever fired.
func (s *Service) reviewCommands(ctx context.Context, repo string, pr int, obs engine.Observation, notBeforeCutoff time.Time, pull ghapi.Pull, comments []ghapi.IssueComment, reviews []ghapi.Review) (cr, codex []engine.CommandSeen, err error) {
	command := strings.TrimSpace(s.cfg.ReviewCommand)
	codexCommand := strings.TrimSpace(s.cfg.CodexCommand)
	hasCR := command != "" && hasCommentBody(comments, command)
	hasCodex := codexCommand != "" && dialect.HasCodexBot(s.cfg.RequiredBots) && hasCommentBody(comments, codexCommand)
	if !hasCR && !hasCodex {
		return nil, nil, nil
	}
	cutoff := notBeforeCutoff
	if pull.Head.SHA != "" {
		if dialect.ShortOID(pull.Head.SHA) != obs.Head {
			return nil, nil, nil
		}
		commit, gerr := s.gh.GetCommit(ctx, repo, pull.Head.SHA)
		if gerr != nil {
			if _, ok := ghapi.ThrottleWait(gerr); ok {
				return nil, nil, gerr
			}
			// No head-commit cutoff available (unreadable/404 head): skip adoption
			// rather than wedge the queue — the worst case is posting a command that
			// already exists, the pre-adoption behavior.
			return nil, nil, nil
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
	if hasCR {
		cr = s.adoptableCR(obs, cutoff, command, comments, reviews)
	}
	if hasCodex {
		codex = newestCommandSince(codexCommand, cutoff, comments)
	}
	return cr, codex, nil
}

// adoptableCR returns the newest CodeRabbit command comment safe to adopt as an
// already-posted fire, or none. A command the bot already answered with a review
// or a completion reply belongs to a finished round for an earlier head and is
// never adopted (adopting it would mark the new head fired without reviewing it).
func (s *Service) adoptableCR(obs engine.Observation, cutoff time.Time, command string, comments []ghapi.IssueComment, reviews []ghapi.Review) []engine.CommandSeen {
	best := newestCommandSince(command, cutoff, comments)
	if len(best) == 0 {
		return nil
	}
	bestAt := best[0].CreatedAt
	if bestAt.IsZero() {
		bestAt = best[0].UpdatedAt
	}
	for _, review := range reviews {
		if s.isConfiguredBot(review.User.Login) && !review.SubmittedAt.Before(bestAt) {
			return nil
		}
	}
	if engine.CommandHasCompletionReply(obs, s.policy(), best[0].ID) {
		return nil
	}
	return best
}

// newestCommandSince returns the newest comment whose trimmed body is command
// and which is not older than cutoff, as a single-element CommandSeen slice
// (empty when none).
func newestCommandSince(command string, cutoff time.Time, comments []ghapi.IssueComment) []engine.CommandSeen {
	var best ghapi.IssueComment
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
		return nil
	}
	return []engine.CommandSeen{{ID: best.ID, CreatedAt: best.CreatedAt, UpdatedAt: best.UpdatedAt}}
}

// hasCommentBody reports whether any comment's trimmed body equals body.
func hasCommentBody(comments []ghapi.IssueComment, body string) bool {
	for _, comment := range comments {
		if strings.TrimSpace(comment.Body) == body {
			return true
		}
	}
	return false
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
