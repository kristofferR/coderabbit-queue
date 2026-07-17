package crq

import (
	"fmt"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
	crqstate "github.com/kristofferR/coderabbit-queue/internal/state"
)

// The persisted schema and its store live in internal/state (v3). These
// aliases keep the crq orchestration code referring to State/Round/… without
// the package qualifier, and without colliding with the many `state`/`st`
// variable names in this package.
type (
	State        = crqstate.State
	Round        = crqstate.Round
	Phase        = crqstate.Phase
	FireSlot     = crqstate.FireSlot
	AccountQuota = crqstate.AccountQuota
	LeaderLease  = crqstate.LeaderLease
	Revision     = crqstate.Revision
	StateStore   = crqstate.StateStore
	StoreConfig  = crqstate.StoreConfig
)

const (
	PhaseQueued        = crqstate.PhaseQueued
	PhaseReserved      = crqstate.PhaseReserved
	PhaseFired         = crqstate.PhaseFired
	PhaseReviewing     = crqstate.PhaseReviewing
	PhaseAwaitingRetry = crqstate.PhaseAwaitingRetry
	PhaseCompleted     = crqstate.PhaseCompleted
	PhaseAbandoned     = crqstate.PhaseAbandoned
)

var (
	ErrCASConflict = crqstate.ErrCASConflict
	ErrNoChange    = crqstate.ErrNoChange
	cloneState     = crqstate.Clone
)

func (c Config) storeConfig() StoreConfig {
	return StoreConfig{
		GateRepo:       c.GateRepo,
		StateRef:       c.StateRef,
		DashboardIssue: c.DashboardIssue,
		Timezone:       c.Timezone,
		Scope:          c.Scope,
	}
}

// NewGitStateStore builds the git-ref-backed store. The logger surfaces the
// loud auto-reinit line when a stale-schema payload is loaded.
func NewGitStateStore(cfg Config, gh *GitHub, log Logger) *crqstate.GitStateStore {
	return crqstate.NewGitStateStore(cfg.storeConfig(), gh, log)
}

func NewMemoryStore(cfg Config) *crqstate.MemoryStore {
	return crqstate.NewMemoryStore(cfg.storeConfig())
}

// DefaultState returns a fresh v3 state seeded with the configured scope, used
// by tests and init.
func DefaultState(cfg Config) State {
	st := crqstate.New()
	st.Account.Scope = strings.Join(cfg.Scope, ",")
	st.Account.Source = "init"
	return st
}

func renderDashboard(st State, cfg Config) string {
	return crqstate.RenderDashboard(st, cfg.storeConfig())
}
func renderTitle(st State) string { return crqstate.RenderTitle(st) }
func issueBody(st State, cfg Config) (string, error) {
	return crqstate.IssueBody(st, cfg.storeConfig())
}

// policy assembles the engine Policy from config.
func (s *Service) policy() engine.Policy {
	return engine.Policy{
		Bot:               s.cfg.Bot,
		RequiredBots:      s.cfg.RequiredBots,
		MinInterval:       s.cfg.MinInterval,
		InflightTimeout:   s.cfg.InflightTimeout,
		RateLimitFallback: s.cfg.RateLimitFallback,
	}
}

func NormalizeRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimSuffix(repo, ".git")
	return strings.ToLower(repo)
}

func QueueKey(repo string, pr int) string {
	return fmt.Sprintf("%s#%d", NormalizeRepo(repo), pr)
}

// --- v2→v3 compatibility shims (consumed by feedback.go / Wait, rewritten in 4b) ---

// FeedbackWait is the v2-shaped view of a fired/reviewing round, retained so
// the feedback/loop code keeps compiling against round state until stage 4b
// rewrites it.
type FeedbackWait struct {
	Repo           string
	PR             int
	Head           string
	StartedAt      time.Time
	Deadline       time.Time
	FiredCommentID int64
	ByHost         string
}

// waitView presents a repo#pr's current round as a FeedbackWait when it is
// fired or reviewing (the v2 "awaiting feedback" states); otherwise the zero
// value, whose empty Head reads as "no wait" at every call site.
func waitView(st *State, repo string, pr int) FeedbackWait {
	r := st.Round(repo, pr)
	if r == nil || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
		return FeedbackWait{}
	}
	w := FeedbackWait{Repo: r.Repo, PR: r.PR, Head: r.Head, FiredCommentID: r.CommandID, ByHost: r.ByHost}
	if r.FiredAt != nil {
		w.StartedAt = r.FiredAt.UTC()
	}
	if r.WaitDeadline != nil {
		w.Deadline = r.WaitDeadline.UTC()
	}
	return w
}

// roundAnchor returns the fire timestamp and command id for repo#pr's current
// round when its head matches — the completion cutoff anchor that v2 read from
// AwaitingFeedback/InFlight/History.
func roundAnchor(st *State, repo string, pr int, head string) (firedAt time.Time, commandID int64, ok bool) {
	r := st.Round(repo, pr)
	if r == nil || r.Head != head || r.FiredAt == nil {
		return time.Time{}, 0, false
	}
	return r.FiredAt.UTC(), r.CommandID, true
}

// containsActive reports whether repo#pr has a round still occupying its slot
// (queued through awaiting_retry) — the v2 State.Contains for the queue/inflight.
func containsActive(st *State, repo string, pr int) bool {
	r := st.Round(repo, pr)
	return r != nil && r.Active()
}

// firedMarker returns the head for which repo#pr has already been requested and
// must not be re-fired without a new head — the v2 Fired[key] dedupe. A
// completed round, or one still fired/reviewing, is such a marker; a parked
// awaiting_retry round is not (Pump re-fires it once RetryAt passes).
func firedMarker(st *State, repo string, pr int) string {
	r := st.Round(repo, pr)
	if r == nil {
		return ""
	}
	switch r.Phase {
	case PhaseFired, PhaseReviewing, PhaseCompleted:
		return r.Head
	}
	return ""
}

// accountBlockedUntil returns the latest active block preventing repo#pr@head
// from firing: the account-wide quota block or this round's own retry window
// (the v2 feedbackBlockedUntil over Blocked + per-head Cooldown).
func accountBlockedUntil(st *State, repo string, pr int, head string, now time.Time) (time.Time, bool) {
	var until time.Time
	if st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now) {
		until = st.Account.BlockedUntil.UTC()
	}
	if r := st.Round(repo, pr); r != nil && r.Phase == PhaseAwaitingRetry && r.Head == head && r.RetryAt != nil && r.RetryAt.After(now) && r.RetryAt.After(until) {
		until = r.RetryAt.UTC()
	}
	return until, !until.IsZero()
}
