package serve

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Observer is the expensive half of a PR view: everything that needs a round
// trip to GitHub. It is an interface so this package keeps working without one
// — the cheap state layer still renders, and the findings arrive when they can.
type Observer interface {
	Observe(ctx context.Context, repo string, pr int) (Observation, error)
}

// Observation is what one GitHub read tells us about a pull request.
type Observation struct {
	Head       string            `json:"head"`
	Converged  bool              `json:"converged"`
	Status     string            `json:"status,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	ReviewedBy map[string]bool   `json:"reviewed_by,omitempty"`
	Findings   []dialect.Finding `json:"findings"`
	Dismissed  int               `json:"dismissed,omitempty"`
	CheckedAt  time.Time         `json:"checked_at"`
}

// Coster estimates what one more round on a pull request would cost. Separate
// from Observer because it is a different question with a different failure
// mode: a page can show findings without a price, and a price without findings.
type Coster interface {
	Cost(ctx context.Context, repo string, pr int) (Cost, error)
}

// Cost mirrors crq.RoundCost on the wire. Ranges, per-reviewer reasoning and a
// prices-checked date, because a single confident figure would be the one
// output guaranteed to be wrong.
type Cost struct {
	Low             float64        `json:"low"`
	High            float64        `json:"high"`
	Exact           bool           `json:"exact,omitempty"`
	Unpriced        []string       `json:"unpriced,omitempty"`
	Summary         string         `json:"summary"`
	PricesCheckedAt string         `json:"prices_checked_at"`
	Reviewers       []CostReviewer `json:"reviewers"`
	Diff            CostDiff       `json:"diff"`
}

type CostReviewer struct {
	Bot     string  `json:"bot"`
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
	Exact   bool    `json:"exact,omitempty"`
	Unknown bool    `json:"unknown,omitempty"`
	Basis   string  `json:"basis"`
}

type CostDiff struct {
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

// PRView is the two-layer page: state renders instantly, observation fills in.
type PRView struct {
	Repo  string     `json:"repo"`
	PR    int        `json:"pr"`
	Round *RoundView `json:"round,omitempty"`
	Hold  *HeldRow   `json:"hold,omitempty"`

	// Observed is nil until a GitHub read succeeds. ObserveError explains why
	// it is still nil, so the page can say so rather than looking empty.
	Observed     *Observation `json:"observed,omitempty"`
	ObserveError string       `json:"observe_error,omitempty"`

	// Cost is what the NEXT round would cost. Nil when it could not be worked
	// out; CostError says why rather than leaving a blank where money goes.
	Cost      *Cost  `json:"cost,omitempty"`
	CostError string `json:"cost_error,omitempty"`

	History []HistoryEntry `json:"history"`
}

// RoundView is the live round, straight from state — no network needed.
type RoundView struct {
	Head       string      `json:"head"`
	Phase      string      `json:"phase"`
	Attempts   int         `json:"attempts,omitempty"`
	EnqueuedAt time.Time   `json:"enqueued_at"`
	FiredAt    *time.Time  `json:"fired_at,omitempty"`
	Deadline   *time.Time  `json:"deadline,omitempty"`
	RetryAt    *time.Time  `json:"retry_at,omitempty"`
	Note       string      `json:"note,omitempty"`
	Host       string      `json:"host,omitempty"`
	CoOnly     bool        `json:"co_only,omitempty"`
	Bots       []Bot       `json:"bots"`
	Fixing     *Session    `json:"fixing,omitempty"`
	Dismissed  []Dismissed `json:"dismissed,omitempty"`
	Next       string      `json:"next,omitempty"`
}

type Dismissed struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// HistoryEntry is an earlier head for this PR, newest first.
type HistoryEntry struct {
	Head    string     `json:"head"`
	Outcome string     `json:"outcome"`
	Note    string     `json:"note,omitempty"`
	At      *time.Time `json:"at,omitempty"`
	Current bool       `json:"current,omitempty"`
}

// observeCache keeps one observation per (repo, pr, head). Keying on head
// matters: a new head invalidates findings, and serving the previous head's
// list would be worse than serving none.
type observeCache struct {
	mu      sync.Mutex
	entries map[string]observeEntry
}

type observeEntry struct {
	obs     Observation
	err     string
	fetched time.Time
}

const observeTTL = 60 * time.Second

func (c *observeCache) get(key string) (observeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.fetched) > observeTTL {
		return observeEntry{}, false
	}
	return e, true
}

func (c *observeCache) put(key string, e observeEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]observeEntry{}
	}
	c.entries[key] = e
}

// costCache mirrors observeCache. Keyed on the head too: a price for a head
// that has been superseded is a price for the wrong diff.
type costCache struct {
	mu      sync.Mutex
	entries map[string]costEntry
}

type costEntry struct {
	cost    *Cost
	err     string
	fetched time.Time
}

// Longer than the observation TTL: the diff of a head does not change, so the
// only thing that can move a price is the account allowance running out.
const costTTL = 5 * time.Minute

func (c *costCache) get(key string) (costEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.fetched) > costTTL {
		return costEntry{}, false
	}
	return e, true
}

func (c *costCache) put(key string, e costEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]costEntry{}
	}
	c.entries[key] = e
}

// buildPRView assembles the cheap layer from state.
func buildPRView(st state.State, repo string, pr int, bots []BotName, inflight time.Duration) PRView {
	v := PRView{Repo: repo, PR: pr, History: []HistoryEntry{}}
	key := state.Key(repo, pr)

	if r, ok := st.Rounds[key]; ok {
		row := RoundRow{Key: key, Repo: r.Repo, PR: r.PR, Head: r.Head, Phase: string(r.Phase),
			FiredAt: r.FiredAt, Bots: botMarks(r, bots)}
		rv := &RoundView{
			Head: r.Head, Phase: string(r.Phase), Attempts: r.Attempts,
			EnqueuedAt: r.EnqueuedAt, FiredAt: r.FiredAt, RetryAt: r.RetryAt,
			Note: r.Note, Host: hostOf(r.ByHost), CoOnly: r.CoOnly, Bots: row.Bots,
		}
		if r.FiredAt != nil && inflight > 0 {
			d := r.FiredAt.Add(inflight)
			rv.Deadline = &d
		}
		if r.WaitDeadline != nil {
			rv.Deadline = r.WaitDeadline
		}
		for id, reason := range r.Dismissed {
			rv.Dismissed = append(rv.Dismissed, Dismissed{ID: id, Reason: reason})
		}
		sort.Slice(rv.Dismissed, func(i, j int) bool { return rv.Dismissed[i].ID < rv.Dismissed[j].ID })
		if d, ok := st.Dispatches[key]; ok {
			s := Session{Key: key, Repo: repo, PR: pr, Head: r.Head, Host: hostOf(d.Host),
				Attempt: d.Attempts, Since: d.At}
			if !d.Heartbeat.IsZero() {
				hb := d.Heartbeat
				s.Heartbeat = &hb
			}
			rv.Fixing = &s
		}
		row.Bots = rv.Bots
		rv.Next = nextForRound(r, row)
		v.Round = rv
		v.History = append(v.History, HistoryEntry{Head: r.Head, Outcome: string(r.Phase),
			Note: r.Note, At: r.FiredAt, Current: true})
	}

	if h, ok := st.Holds[key]; ok {
		v.Hold = &HeldRow{Key: key, Repo: repo, PR: pr, Reason: h.Reason, By: h.By, At: h.At}
		if v.Round != nil {
			v.Hold.Head = v.Round.Head
		}
	}

	for _, r := range st.Archive {
		if !strings.EqualFold(r.Repo, repo) || r.PR != pr {
			continue
		}
		v.History = append(v.History, HistoryEntry{Head: r.Head, Outcome: string(r.Phase),
			Note: r.Note, At: r.FiredAt})
	}
	sort.SliceStable(v.History, func(i, j int) bool {
		a, b := v.History[i].At, v.History[j].At
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		return a.After(*b)
	})
	return v
}

// handlePR serves one pull request. The state layer always answers; the
// observation is attempted but never allowed to fail the request — a page that
// shows the round and says "could not reach GitHub" beats a 500.
func (s *Server) handlePR(w http.ResponseWriter, r *http.Request) {
	owner, name := r.PathValue("owner"), r.PathValue("name")
	pr, err := strconv.Atoi(r.PathValue("pr"))
	if owner == "" || name == "" || err != nil || pr <= 0 {
		http.NotFound(w, r)
		return
	}
	repo := owner + "/" + name

	s.mu.RLock()
	st := s.lastState
	s.mu.RUnlock()

	view := buildPRView(st, repo, pr, s.botsFor(&st)(repo), s.opts.Inflight)

	if s.observer != nil {
		head := ""
		if view.Round != nil {
			head = view.Round.Head
		}
		key := strings.ToLower(repo) + "#" + strconv.Itoa(pr) + "@" + head
		if r.URL.Query().Get("refresh") == "1" {
			s.observations.put(key, observeEntry{})
		}
		if e, ok := s.observations.get(key); ok && (e.err != "" || !e.obs.CheckedAt.IsZero()) {
			if e.err != "" {
				view.ObserveError = e.err
			} else {
				obs := e.obs
				view.Observed = &obs
			}
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()
			obs, err := s.observer.Observe(ctx, repo, pr)
			entry := observeEntry{obs: obs, fetched: time.Now()}
			if err != nil {
				entry.err = err.Error()
				view.ObserveError = entry.err
			} else {
				view.Observed = &obs
			}
			s.observations.put(key, entry)
		}
	} else {
		view.ObserveError = "this server was started without GitHub access"
	}

	// Priced on the same trip and cached the same way: it costs one more pull
	// read, which is why the overview does not price every queue row.
	if s.opts.Coster != nil {
		head := ""
		if view.Round != nil {
			head = view.Round.Head
		}
		key := strings.ToLower(repo) + "#" + strconv.Itoa(pr) + "@" + head
		if r.URL.Query().Get("refresh") == "1" {
			s.costs.put(key, costEntry{})
		}
		if e, ok := s.costs.get(key); ok && (e.err != "" || e.cost != nil) {
			view.Cost, view.CostError = e.cost, e.err
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			cost, err := s.opts.Coster.Cost(ctx, repo, pr)
			entry := costEntry{fetched: time.Now()}
			if err != nil {
				entry.err = err.Error()
				view.CostError = entry.err
			} else {
				entry.cost = &cost
				view.Cost = &cost
			}
			s.costs.put(key, entry)
		}
	}

	writeJSON(w, http.StatusOK, view)
}
