package serve

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Enrollment is one repository's answer to "does crq review this project", as
// resolved by internal/crq. serve does not decide it: the precedence between a
// shared record and a host's env file is a queue rule, and two answers to it
// would be one too many.
type Enrollment struct {
	Source      string     `json:"source"` // state|env|excluded|scope|off
	Enabled     bool       `json:"enabled"`
	EnvConflict bool       `json:"env_conflict,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	By          string     `json:"by,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

// EnrollFor resolves one repository against a loaded state.
type EnrollFor func(st state.State, repo string) Enrollment

// Candidate is one repository the picker offers. Everything here comes from the
// repository listing itself; whether crq already knows it is filled in locally.
type Candidate struct {
	Repo     string `json:"repo"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
	// Issues is GitHub's open_issues_count, which counts issues AND pull
	// requests. Labelled as what it is rather than as an open-PR count, which
	// would cost one search per repository to know.
	Issues   int        `json:"issues"`
	PushedAt *time.Time `json:"pushed_at,omitempty"`
	Language string     `json:"language,omitempty"`
	// Enrollment is nil for a repository crq has no answer about yet.
	Enrollment *Enrollment `json:"enrollment,omitempty"`
}

// Discoverer lists the repositories in the configured scope. It is a separate
// interface from Observer because it is the one call in the dashboard that is
// expensive enough to cache aggressively and to never make on a page load.
type Discoverer interface {
	Discover(ctx context.Context) ([]Candidate, error)
}

// discoverCache holds the scope listing. Repositories appear when someone
// creates one, which is rare, and the picker has a Refresh button — so a long
// TTL costs a stale row and saves a multi-page REST walk on every open.
type discoverCache struct {
	mu   sync.Mutex
	at   time.Time
	rows []Candidate
	err  error
}

const discoverTTL = 10 * time.Minute

func (c *discoverCache) get(ctx context.Context, d Discoverer, now time.Time, force bool) ([]Candidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.at.After(now.Add(-discoverTTL)) && c.err == nil {
		return c.rows, nil
	}
	rows, err := d.Discover(ctx)
	c.at, c.rows, c.err = now, rows, err
	return rows, err
}

// handleDiscover answers the repository picker. It never blocks the rest of the
// dashboard: nothing else calls it, and a failure here is reported as itself
// rather than as a broken page.
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if s.opts.Discoverer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this dashboard has no repository discovery configured",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	rows, err := s.discovered.get(ctx, s.opts.Discoverer, s.opts.Now(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// Annotate with what crq already knows, from the snapshot the server holds
	// — the picker's whole job is separating "not added yet" from "added, and
	// here is where that answer came from".
	s.mu.RLock()
	st := s.lastState
	s.mu.RUnlock()
	out := make([]Candidate, 0, len(rows))
	for _, c := range rows {
		if s.opts.EnrollFor != nil {
			e := s.opts.EnrollFor(st, c.Repo)
			c.Enrollment = &e
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// Recently pushed first: the repository somebody wants to add is
		// overwhelmingly the one they were just working in.
		switch {
		case a.PushedAt != nil && b.PushedAt != nil && !a.PushedAt.Equal(*b.PushedAt):
			return a.PushedAt.After(*b.PushedAt)
		case a.PushedAt != nil && b.PushedAt == nil:
			return true
		case a.PushedAt == nil && b.PushedAt != nil:
			return false
		}
		return strings.ToLower(a.Repo) < strings.ToLower(b.Repo)
	})
	writeJSON(w, http.StatusOK, map[string]any{"repos": out})
}
