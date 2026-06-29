package crq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

var ErrCASConflict = errors.New("state changed while writing")
var ErrNoChange = errors.New("state unchanged")

type Revision struct {
	CommitSHA string
	TreeSHA   string
}

type StateStore interface {
	Load(context.Context) (State, Revision, error)
	Update(context.Context, func(*State) error) (State, error)
	SyncDashboard(context.Context, State) error
}

type GitStateStore struct {
	cfg Config
	gh  *GitHub
}

func NewGitStateStore(cfg Config, gh *GitHub) *GitStateStore {
	return &GitStateStore{cfg: cfg, gh: gh}
}

func (s *GitStateStore) Load(ctx context.Context) (State, Revision, error) {
	if err := s.cfg.RequireState(); err != nil {
		return State{}, Revision{}, err
	}
	ref, err := s.gh.GetRef(ctx, s.cfg.GateRepo, s.cfg.StateRef)
	if errors.Is(err, ErrNotFound) {
		state := DefaultState(s.cfg)
		state.Normalize(s.cfg)
		return state, Revision{}, nil
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
	if stateBlob == "" {
		return State{}, Revision{}, fmt.Errorf("state ref %s has no %s", s.cfg.StateRef, statePath)
	}
	raw, err := s.gh.GetBlob(ctx, s.cfg.GateRepo, stateBlob)
	if err != nil {
		return State{}, Revision{}, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, Revision{}, err
	}
	state.Normalize(s.cfg)
	return state, Revision{CommitSHA: ref, TreeSHA: commit.Tree.SHA}, nil
}

func (s *GitStateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	const attempts = 12
	for i := 0; i < attempts; i++ {
		state, rev, err := s.Load(ctx)
		if err != nil {
			return State{}, err
		}
		if err := mutate(&state); err != nil {
			if errors.Is(err, ErrNoChange) {
				return state, nil
			}
			return State{}, err
		}
		now := time.Now().UTC()
		state.Rev++
		state.UpdatedAt = &now
		state.Normalize(s.cfg)
		if err := s.compareAndSwap(ctx, &state, rev); err != nil {
			if errors.Is(err, ErrCASConflict) {
				continue
			}
			return State{}, err
		}
		return state, nil
	}
	return State{}, ErrCASConflict
}

func (s *GitStateStore) compareAndSwap(ctx context.Context, state *State, rev Revision) error {
	dashboard := renderDashboard(*state, s.cfg)
	state.DashboardSHA = hashString(dashboard)
	stateJSON, err := json.MarshalIndent(*state, "", "  ")
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
	commitSHA, err := s.gh.CreateCommit(ctx, s.cfg.GateRepo, fmt.Sprintf("crq: state rev %d", state.Rev), treeSHA, parents)
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
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnprocessableEntity || apiErr.Status == http.StatusConflict) {
		return ErrCASConflict
	}
	return err
}

func (s *GitStateStore) SyncDashboard(ctx context.Context, state State) error {
	if err := s.cfg.RequireDashboard(); err != nil {
		return err
	}
	body, err := issueBody(state, s.cfg)
	if err != nil {
		return err
	}
	return s.gh.PatchIssue(ctx, s.cfg.GateRepo, s.cfg.DashboardIssue, renderTitle(state), body)
}

type MemoryStore struct {
	mu    sync.Mutex
	cfg   Config
	state State
	rev   int64
}

func NewMemoryStore(cfg Config) *MemoryStore {
	state := DefaultState(cfg)
	state.Normalize(cfg)
	return &MemoryStore{cfg: cfg, state: state}
}

func (m *MemoryStore) Load(context.Context) (State, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := cloneState(m.state)
	return state, Revision{CommitSHA: fmt.Sprintf("%d", m.rev)}, nil
}

func (m *MemoryStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := cloneState(m.state)
	if err := mutate(&state); err != nil {
		if errors.Is(err, ErrNoChange) {
			return state, nil
		}
		return State{}, err
	}
	now := time.Now().UTC()
	state.Rev++
	state.UpdatedAt = &now
	state.Normalize(m.cfg)
	m.rev++
	m.state = state
	return state, nil
}

func (m *MemoryStore) SyncDashboard(context.Context, State) error { return nil }

func cloneState(state State) State {
	raw, _ := json.Marshal(state)
	var out State
	_ = json.Unmarshal(raw, &out)
	return out
}
