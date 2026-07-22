package crq

import (
	"fmt"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
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
func NewGitStateStore(cfg Config, gh *ghapi.GitHub, log Logger) *crqstate.GitStateStore {
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
		Bot:                   s.cfg.Bot,
		RequiredBots:          s.cfg.RequiredBots,
		CodexCommand:          s.cfg.CodexCommand,
		MinInterval:           s.cfg.MinInterval,
		InflightTimeout:       s.cfg.InflightTimeout,
		RateLimitFallback:     s.cfg.RateLimitFallback,
		RateLimitCodexDegrade: s.cfg.RateLimitCodexDegrade,
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
