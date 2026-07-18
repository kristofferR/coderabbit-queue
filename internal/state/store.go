package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	gh "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// ErrCASConflict is returned when the state ref moved between load and write.
var ErrCASConflict = errors.New("state changed while writing")

// ErrNoChange lets a mutate closure report "nothing to persist", so Update
// returns the current state without writing a new revision.
var ErrNoChange = errors.New("state unchanged")

const (
	statePath     = "state.json"
	dashboardPath = "dashboard.md"
)

// Logger is the minimal logging surface the store uses (for the loud
// auto-reinit line when a stale schema payload is loaded).
type Logger interface {
	Printf(string, ...any)
}

// StoreConfig carries the fields the store and dashboard need from crq's
// Config, so internal/state stays free of an import cycle back to crq.
type StoreConfig struct {
	GateRepo       string
	StateRef       string
	DashboardIssue int
	Timezone       string
	Scope          []string
}

func (c StoreConfig) requireState() error {
	if c.GateRepo == "" {
		return errors.New("CRQ_REPO is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

func (c StoreConfig) requireDashboard() error {
	if err := c.requireState(); err != nil {
		return err
	}
	if c.DashboardIssue <= 0 {
		return errors.New("CRQ_ISSUE is not set (run 'crq init' or configure ~/.config/crq/env)")
	}
	return nil
}

// Revision identifies the git commit/tree the loaded state came from, so the
// compare-and-swap can build the next commit on top of it.
type Revision struct {
	CommitSHA string
	TreeSHA   string
}

// StateStore is the persistence surface crq consumes. Load reads the current
// state, Update applies a mutate closure under compare-and-swap, and
// SyncDashboard mirrors the state to the dashboard issue.
type StateStore interface {
	Load(context.Context) (State, Revision, error)
	Update(context.Context, func(*State) error) (State, error)
	SyncDashboard(context.Context, State) error
}

// GitStateStore persists v3 state as state.json in a git ref, with the same
// compare-and-swap mechanism as v2 (12 retries on UpdateRef 409/422). Only the
// payload shape changed; a payload whose schema version isn't 3 (or won't
// parse) is logged loudly and auto-reinitialized — crq is pre-release, there is
// no migration.
type GitStateStore struct {
	cfg StoreConfig
	gh  *gh.GitHub
	log Logger
}

func NewGitStateStore(cfg StoreConfig, client *gh.GitHub, log Logger) *GitStateStore {
	return &GitStateStore{cfg: cfg, gh: client, log: log}
}

func (s *GitStateStore) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

func (s *GitStateStore) Load(ctx context.Context) (State, Revision, error) {
	if err := s.cfg.requireState(); err != nil {
		return State{}, Revision{}, err
	}
	ref, err := s.gh.GetRef(ctx, s.cfg.GateRepo, s.cfg.StateRef)
	if errors.Is(err, gh.ErrNotFound) {
		st := s.fresh()
		return st, Revision{}, nil
	}
	if err != nil {
		return State{}, Revision{}, err
	}
	commit, err := s.gh.GetCommit(ctx, s.cfg.GateRepo, ref)
	if err != nil {
		return State{}, Revision{}, err
	}
	tree, err := s.gh.GetTree(ctx, s.cfg.GateRepo, commit.Tree.SHA)
	if err != nil {
		return State{}, Revision{}, err
	}
	var stateBlob string
	for _, item := range tree.Tree {
		if item.Path == statePath && item.Type == "blob" {
			stateBlob = item.SHA
			break
		}
	}
	rev := Revision{CommitSHA: ref, TreeSHA: commit.Tree.SHA}
	if stateBlob == "" {
		s.logf("state ref %s has no %s — reinitializing to a fresh v%d state", s.cfg.StateRef, statePath, SchemaVersion)
		return s.fresh(), rev, nil
	}
	raw, err := s.gh.GetBlob(ctx, s.cfg.GateRepo, stateBlob)
	if err != nil {
		return State{}, Revision{}, err
	}
	// Peek at the schema version before a full decode: a v2 (or unknown) payload
	// must not be coerced field-by-field into v3, it must be discarded.
	var probe struct {
		Version int `json:"v"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Version != SchemaVersion {
		reason := fmt.Sprintf("schema v%d", probe.Version)
		if err != nil {
			reason = "an unparseable payload"
		}
		s.logf("state ref %s holds %s (want v%d) — reinitializing to a fresh state (no migration; crq is pre-release)", s.cfg.StateRef, reason, SchemaVersion)
		return s.fresh(), rev, nil
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		s.logf("state ref %s payload failed to decode (%v) — reinitializing to a fresh state", s.cfg.StateRef, err)
		return s.fresh(), rev, nil
	}
	st.Normalize(time.Now().UTC())
	return st, rev, nil
}

func (s *GitStateStore) fresh() State {
	st := New()
	st.Account.Scope = joinScope(s.cfg.Scope)
	st.Account.Source = "init"
	st.Normalize(time.Now().UTC())
	return st
}

func (s *GitStateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	const attempts = 12
	for i := 0; i < attempts; i++ {
		st, rev, err := s.Load(ctx)
		if err != nil {
			return State{}, err
		}
		if err := mutate(&st); err != nil {
			if errors.Is(err, ErrNoChange) {
				return st, nil
			}
			return State{}, err
		}
		now := time.Now().UTC()
		st.Rev++
		st.UpdatedAt = &now
		st.Normalize(now)
		if err := s.compareAndSwap(ctx, &st, rev); err != nil {
			if errors.Is(err, ErrCASConflict) {
				continue
			}
			return State{}, err
		}
		return st, nil
	}
	return State{}, ErrCASConflict
}

func (s *GitStateStore) compareAndSwap(ctx context.Context, st *State, rev Revision) error {
	dashboard := RenderDashboard(*st, s.cfg)
	st.DashboardSHA = hashString(dashboard)
	stateJSON, err := json.MarshalIndent(*st, "", "  ")
	if err != nil {
		return err
	}
	stateBlob, err := s.gh.CreateBlob(ctx, s.cfg.GateRepo, append(stateJSON, '\n'))
	if err != nil {
		return err
	}
	dashboardBlob, err := s.gh.CreateBlob(ctx, s.cfg.GateRepo, []byte(dashboard))
	if err != nil {
		return err
	}
	treeSHA, err := s.gh.CreateTree(ctx, s.cfg.GateRepo, rev.TreeSHA, []map[string]any{
		{"path": statePath, "mode": "100644", "type": "blob", "sha": stateBlob},
		{"path": dashboardPath, "mode": "100644", "type": "blob", "sha": dashboardBlob},
	})
	if err != nil {
		return err
	}
	parents := []string{}
	if rev.CommitSHA != "" {
		parents = []string{rev.CommitSHA}
	}
	commitSHA, err := s.gh.CreateCommit(ctx, s.cfg.GateRepo, fmt.Sprintf("crq: state rev %d", st.Rev), treeSHA, parents)
	if err != nil {
		return err
	}
	if rev.CommitSHA == "" {
		err = s.gh.CreateRef(ctx, s.cfg.GateRepo, s.cfg.StateRef, commitSHA)
	} else {
		err = s.gh.UpdateRef(ctx, s.cfg.GateRepo, s.cfg.StateRef, commitSHA, false)
	}
	if err == nil {
		return nil
	}
	var apiErr *gh.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnprocessableEntity || apiErr.Status == http.StatusConflict) {
		return ErrCASConflict
	}
	return err
}

func (s *GitStateStore) SyncDashboard(ctx context.Context, st State) error {
	if err := s.cfg.requireDashboard(); err != nil {
		return err
	}
	body, err := IssueBody(st, s.cfg)
	if err != nil {
		return err
	}
	return s.gh.PatchIssue(ctx, s.cfg.GateRepo, s.cfg.DashboardIssue, RenderTitle(st), body)
}

// MemoryStore is the in-memory store used by tests and the fake-GitHub harness.
type MemoryStore struct {
	mu    sync.Mutex
	cfg   StoreConfig
	state State
	rev   int64
}

func NewMemoryStore(cfg StoreConfig) *MemoryStore {
	st := New()
	st.Account.Scope = joinScope(cfg.Scope)
	st.Account.Source = "init"
	st.Normalize(time.Now().UTC())
	return &MemoryStore{cfg: cfg, state: st}
}

func (m *MemoryStore) Load(context.Context) (State, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Clone(m.state)
	st.Normalize(time.Now().UTC())
	return st, Revision{CommitSHA: fmt.Sprintf("%d", m.rev)}, nil
}

func (m *MemoryStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Clone(m.state)
	st.Normalize(time.Now().UTC())
	if err := mutate(&st); err != nil {
		if errors.Is(err, ErrNoChange) {
			return st, nil
		}
		return State{}, err
	}
	now := time.Now().UTC()
	st.Rev++
	st.UpdatedAt = &now
	st.Normalize(now)
	m.rev++
	m.state = st
	return st, nil
}

func (m *MemoryStore) SyncDashboard(context.Context, State) error { return nil }

// Clone deep-copies a State via its JSON representation, so a mutate closure
// can never scribble on the store's retained copy.
func Clone(st State) State {
	raw, _ := json.Marshal(st)
	var out State
	_ = json.Unmarshal(raw, &out)
	return out
}
