package crq

import (
	"context"
	"errors"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// TidyResult reports what a tidy pass removed, or would have.
type TidyResult struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	// Deleted are the comment IDs removed; Kept explains, per remaining
	// candidate, why it stayed — so a pass that deletes nothing says why rather
	// than looking broken.
	Deleted []int64  `json:"deleted"`
	Kept    []string `json:"kept,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

// Tidy removes the review-trigger comments crq posted that nothing needs any
// more: the bot answered them, and the round that asked has progressed.
//
// A PR driven through a dozen rounds accumulates a dozen "@coderabbitai review"
// comments and a dozen acknowledgements, which buries the conversation a human
// came to read. This deletes crq's own half of that.
//
// It is deliberately narrow in two ways.
//
// Only comments CRQ POSTED. A human's "@coderabbitai review" is someone's
// decision to ask, and not crq's to erase; the candidate list is built from the
// command IDs crq recorded on its own rounds, not by matching text.
//
// Only crq's, not the bot's. An auto-generated reply can be a rate-limit notice
// or a skipped-review notice, which crq classifies as evidence and surfaces as
// a finding — so deleting bot comments is how this feature would quietly destroy
// feedback nobody had read yet.
func (s *Service) Tidy(ctx context.Context, repo string, pr int, dryRun bool) (TidyResult, error) {
	repo = NormalizeRepo(repo)
	result := TidyResult{Repo: repo, PR: pr, Deleted: []int64{}, DryRun: dryRun}

	st, _, err := s.store.Load(ctx)
	if err != nil {
		return result, err
	}
	cfg := s.cfg

	// Candidates come from rounds that have PROGRESSED — archived (superseded,
	// closed, cancelled) or completed. A round still working keeps its command,
	// because that is the comment crq would adopt instead of posting again.
	live := map[int64]bool{}
	var commands []engine.CommandComment
	collect := func(r Round, progressed bool) {
		for _, entry := range roundCommands(r, cfg) {
			if progressed {
				commands = append(commands, entry)
			} else {
				live[entry.ID] = true
			}
		}
	}
	if round := st.Round(repo, pr); round != nil {
		collect(*round, !round.Active())
	}
	for _, archived := range st.Archive {
		if NormalizeRepo(archived.Repo) == repo && archived.PR == pr {
			collect(archived, true)
		}
	}
	if len(commands) == 0 {
		result.Kept = append(result.Kept, "no progressed round has a trigger comment to remove")
		return result, nil
	}

	obs, err := s.observe(ctx, repo, pr, nil, s.clock())
	if err != nil {
		return result, err
	}
	if !obs.eng.Open {
		// A closed PR is nobody's reading material, and deleting from it spends
		// writes for no one's benefit.
		result.Kept = append(result.Kept, "pr is closed")
		return result, nil
	}

	in := engine.TidyInput{
		Commands:   commands,
		Live:       live,
		HeadAt:     obs.eng.HeadAt,
		AnsweredAt: answeredAt(obs, cfg),
	}
	stale := engine.StaleCommands(in)
	if len(stale) == 0 {
		result.Kept = append(result.Kept, "every trigger comment is still live, unanswered, or newer than the head")
		return result, nil
	}
	if dryRun || s.cfg.DryRun {
		result.DryRun = true
		result.Deleted = stale
		return result, nil
	}
	for _, id := range stale {
		err := s.gh.DeleteIssueComment(ctx, repo, id)
		switch {
		case err == nil, errors.Is(err, ghapi.ErrNotFound):
			// Already gone is the outcome we wanted. A recorded command can
			// vanish without crq: the bot removes some of its own command
			// comments, and a person may tidy by hand.
			result.Deleted = append(result.Deleted, id)
		default:
			// One write failing must not abandon the rest.
			if s.log != nil {
				s.log.Printf("tidy: %s#%d comment %d: %v", repo, pr, id, err)
			}
		}
	}
	if s.log != nil && len(result.Deleted) > 0 {
		s.log.Printf("tidy: %s#%d removed %d spent trigger comment(s)", repo, pr, len(result.Deleted))
	}
	return result, nil
}

// roundCommands lists the trigger comments crq recorded on one round: the
// primary's, and each co-reviewer's.
func roundCommands(r Round, cfg Config) []engine.CommandComment {
	var out []engine.CommandComment
	at := time.Time{}
	if r.FiredAt != nil {
		at = r.FiredAt.UTC()
	}
	if r.CommandID != 0 {
		out = append(out, engine.CommandComment{ID: r.CommandID, Bot: dialect.NormalizeBotName(cfg.Bot), CreatedAt: at})
	}
	for login, co := range r.CoBots {
		if co.CommandID == 0 {
			continue
		}
		when := at
		if co.CommandedAt != nil {
			when = co.CommandedAt.UTC()
		}
		out = append(out, engine.CommandComment{ID: co.CommandID, Bot: dialect.NormalizeBotName(login), CreatedAt: when})
	}
	return out
}

// answeredAt is the newest moment each reviewer demonstrably acted on this PR.
// A command with nothing after it was never read, and only what has been read is
// removed.
func answeredAt(obs observation, cfg Config) map[string]time.Time {
	out := map[string]time.Time{}
	note := func(bot string, at time.Time) {
		key := dialect.NormalizeBotName(bot)
		if key == "" || at.IsZero() {
			return
		}
		if at.After(out[key]) {
			out[key] = at
		}
	}
	for _, review := range obs.reviews {
		note(review.User.Login, review.SubmittedAt)
	}
	for _, event := range obs.eng.Events {
		note(event.Bot, eventAt(event))
	}
	for _, check := range obs.eng.Checks {
		note(check.Bot, check.CompletedAt)
	}
	return out
}

// eventAt is when a classified bot event happened.
func eventAt(e dialect.BotEvent) time.Time {
	if !e.UpdatedAt.IsZero() {
		return e.UpdatedAt
	}
	return e.CreatedAt
}

// tidyAfterPump removes spent trigger comments for a PR whose round just
// progressed, which is what makes tidying happen regularly without a sweep of
// its own: the daemon is already looking at this PR.
//
// It is best-effort by design. Deleting a comment is housekeeping, and a
// housekeeping failure must never break the pass that did the real work.
func (s *Service) tidyAfterPump(ctx context.Context, res PumpResult) {
	if !s.cfg.Tidy || res.Repo == "" || res.PR == 0 {
		return
	}
	switch res.Action {
	case "deduped", "waiting", "requeued", "skipped":
		// The round reached a state where its earlier commands are answered and
		// spent. "fired" is not here: that round is live and owns its command.
	default:
		return
	}
	if _, err := s.Tidy(ctx, res.Repo, res.PR, false); err != nil && s.log != nil {
		s.log.Printf("tidy: %s#%d: %v", res.Repo, res.PR, err)
	}
}
