package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Actor performs the mutations the dashboard offers. Every one is a thin mirror
// of a CLI verb and goes through the same Service method, so the dashboard can
// never become a second way to change state with different rules.
type Actor interface {
	Hold(ctx context.Context, repo string, pr int, reason string) error
	Unhold(ctx context.Context, repo string, pr int) error
	Cancel(ctx context.Context, repo string, pr int) error
	SetAutofix(ctx context.Context, repo string, enabled bool, reason string) error
	ClearAutofix(ctx context.Context, repo string) error
	// SetEnrollment decides whether crq reviews a repository at all, and
	// ClearEnrollment hands it back to the hosts' env files. Like the reviewer
	// override, they report the hosts that will not honour the record.
	SetEnrollment(ctx context.Context, repo string, enabled bool, reason string) (lagging []string, err error)
	ClearEnrollment(ctx context.Context, repo string) error
	// SetReviewers returns the hosts that will not honour the override, so the
	// UI can say so rather than reporting a save that some daemon ignores.
	SetReviewers(ctx context.Context, repo string, coBots, required []string, primary *bool) (lagging []string, err error)
	ClearReviewers(ctx context.Context, repo string) error

	// The three ways a finding stops blocking. They are distinct on purpose:
	// resolving says it was handled, declining says it was considered and
	// rejected (and posts that reasoning back), dismissing accounts for a
	// finding GitHub gives no way to close.
	ResolveThreads(ctx context.Context, threadIDs []string) error
	DeclineThreads(ctx context.Context, threadIDs []string, reason string, resolve bool) error
	DismissFindings(ctx context.Context, repo string, pr int, ids []string, reason string) error
}

type actionRequest struct {
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	Reason  string `json:"reason"`
	Enabled *bool  `json:"enabled"`
	// CoBots and Required are the whole intended sets, not a delta: a save that
	// sent only changes could not express "explicitly none".
	CoBots   []string `json:"cobots"`
	Required []string `json:"required"`
	// Primary is nil to leave the metered reviewer's switch alone, or points at
	// whether it runs on this repository at all.
	Primary *bool `json:"primary"`
	Clear   bool  `json:"clear"`

	ThreadIDs  []string `json:"thread_ids"`
	FindingIDs []string `json:"finding_ids"`
	// KeepOpen declines a finding without resolving its thread, for when the
	// disagreement is worth leaving visible.
	KeepOpen bool `json:"keep_open"`
}

// handleAction runs one action and returns the refreshed snapshot, so the UI
// never has to guess whether the write landed — it sees the new state or an
// error, and nothing in between.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if s.actor == nil || s.opts.ReadOnly {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this dashboard is read-only",
		})
		return
	}
	// A custom header a browser cannot set cross-origin without a preflight.
	// The server is unauthenticated on a tailnet, so this is what stops a page
	// on another site from posting to it behind your back.
	if r.Header.Get("X-CRQ-Dashboard") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing dashboard header"})
		return
	}

	var req actionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" || !strings.Contains(req.Repo, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/name"})
		return
	}

	action := r.PathValue("action")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var err error
	switch action {
	case "hold":
		// The reason is the record: every screen that shows a hold shows why,
		// and an unexplained hold is the one nobody dares lift.
		if strings.TrimSpace(req.Reason) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required"})
			return
		}
		err = s.needPR(req, func() error { return s.actor.Hold(ctx, req.Repo, req.PR, req.Reason) })
	case "unhold":
		err = s.needPR(req, func() error { return s.actor.Unhold(ctx, req.Repo, req.PR) })
	case "cancel":
		err = s.needPR(req, func() error { return s.actor.Cancel(ctx, req.Repo, req.PR) })
	case "autofix":
		if req.Enabled == nil {
			err = s.actor.ClearAutofix(ctx, req.Repo) // back to the fleet default
		} else {
			err = s.actor.SetAutofix(ctx, req.Repo, *req.Enabled, req.Reason)
		}
	case "reviewers":
		if req.Clear {
			err = s.actor.ClearReviewers(ctx, req.Repo)
			break
		}
		// A nil list was not sent at all (a save that only flips the primary
		// switch), which is different from a sent-but-empty one. The service
		// re-checks the RESOLVED set either way — this is the early, specific
		// error for the case the UI can produce directly.
		if req.Required != nil && len(req.Required) == 0 {
			// Convergence would never be reachable, so refuse here rather than
			// letting a round wait for a set nothing can satisfy.
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "at least one reviewer must be required, or convergence can never happen",
			})
			return
		}
		var lagging []string
		lagging, err = s.actor.SetReviewers(ctx, req.Repo, req.CoBots, req.Required, req.Primary)
		if err == nil && len(lagging) > 0 {
			s.refresh(ctx)
			snap, _ := s.snapshot()
			writeJSON(w, http.StatusOK, map[string]any{
				"snapshot": snap,
				// hostOf strips the pid/run suffix the state ref keys writers by:
				// what the reader needs is the machine to upgrade.
				"warning": "saved, but these hosts run an older binary and will ignore it: " +
					strings.Join(hostsOf(lagging), ", "),
			})
			return
		}
	case "enroll":
		if req.Clear {
			err = s.actor.ClearEnrollment(ctx, req.Repo)
			break
		}
		if req.Enabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled must be true or false"})
			return
		}
		// The service refuses an unexplained removal too; asking here means the
		// dialog can say so before the round trip.
		if !*req.Enabled && strings.TrimSpace(req.Reason) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required to stop reviewing a repository"})
			return
		}
		var lagging []string
		lagging, err = s.actor.SetEnrollment(ctx, req.Repo, *req.Enabled, req.Reason)
		if err == nil && len(lagging) > 0 {
			s.refresh(ctx)
			snap, _ := s.snapshot()
			writeJSON(w, http.StatusOK, map[string]any{
				"snapshot": snap,
				"warning": "saved, but these hosts run an older binary and decide from their own env alone: " +
					strings.Join(hostsOf(lagging), ", "),
			})
			return
		}
	case "resolve":
		if len(req.ThreadIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no thread given"})
			return
		}
		err = s.actor.ResolveThreads(ctx, req.ThreadIDs)
	case "decline":
		if len(req.ThreadIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no thread given"})
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			// The reason is posted to the pull request as a reply, so an empty
			// one would leave a bare rejection on someone else's review.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required"})
			return
		}
		err = s.actor.DeclineThreads(ctx, req.ThreadIDs, req.Reason, !req.KeepOpen)
	case "dismiss":
		if len(req.FindingIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no finding given"})
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a reason is required"})
			return
		}
		err = s.needPR(req, func() error {
			return s.actor.DismissFindings(ctx, req.Repo, req.PR, req.FindingIDs, req.Reason)
		})
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// Re-read immediately rather than waiting for the next poll: the person who
	// clicked is watching, and a stale answer reads as a failed click.
	s.refresh(ctx)
	snap, _ := s.snapshot()
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) needPR(req actionRequest, run func() error) error {
	if req.PR <= 0 {
		return errBadPR
	}
	return run()
}

var errBadPR = errPR("a pull request number is required")

type errPR string

func (e errPR) Error() string { return string(e) }

// hostsOf reduces writer keys ("host=X pid=… run=…") to the machine names a
// person would act on.
func hostsOf(writers []string) []string {
	out := make([]string, 0, len(writers))
	seen := map[string]bool{}
	for _, w := range writers {
		h := hostOf(w)
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}
