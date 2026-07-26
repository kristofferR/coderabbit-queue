package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// ReviewerView is how one repository's reviewers read after its override is
// applied — the answer to "which bots run on this project".
type ReviewerView struct {
	Repo string `json:"repo"`
	// Overridden says whether this repository has its own configuration or is
	// simply following the fleet default.
	Overridden bool `json:"overridden"`
	// Lagging names hosts that are driving this queue without understanding
	// per-repo overrides — an older binary loads the field, writes it back
	// untouched, and keeps deciding from its own fleet-wide configuration. The
	// override is real; those hosts will not honour it until they are upgraded.
	Lagging   []string         `json:"lagging_hosts,omitempty"`
	UpdatedAt string           `json:"updated_at,omitempty"`
	By        string           `json:"by,omitempty"`
	Reviewers []ReviewerDetail `json:"reviewers"`
}

// ReviewerDetail is one reviewer as it will actually be used.
type ReviewerDetail struct {
	Login string `json:"login"`
	// Budget is the only property the queue cares about: "account" is serialized
	// against the shared allowance, "none" runs immediately.
	Budget   string `json:"budget"`
	Required bool   `json:"required"`
	Trigger  string `json:"trigger,omitempty"`
}

// Reviewers reports the reviewers that will run on repo.
func (s *Service) Reviewers(ctx context.Context, repo string) (ReviewerView, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return ReviewerView{}, err
	}
	repo = NormalizeRepo(repo)
	cfg := s.cfgFor(st, repo)
	view := ReviewerView{Repo: repo, Reviewers: []ReviewerDetail{}}
	view.Lagging = st.LaggingWriters(CapsRepoOverrides, s.clock().UTC())
	if ov, ok := st.RepoOverride(repo); ok {
		view.Overridden = true
		view.By = ov.By
		if ov.UpdatedAt != nil {
			view.UpdatedAt = ov.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	for _, r := range cfg.Reviewers {
		view.Reviewers = append(view.Reviewers, ReviewerDetail{
			Login:    r.Login,
			Budget:   string(r.Budget),
			Required: r.Required,
			Trigger:  string(r.Trigger),
		})
	}
	return view, nil
}

// SetReviewers records which co-reviewers run on repo and which of them gate
// convergence. A nil list means "leave that half alone"; an empty non-nil list
// means "none here", which is a different thing and has to survive as one.
//
// The primary is not settable. Its markers and command are injected into the
// dialect classifiers when the Service is built, so a per-repo primary would
// mean per-repo classifiers.
func (s *Service) SetReviewers(ctx context.Context, repo string, coBots, required []string) (ReviewerView, error) {
	repo = NormalizeRepo(repo)
	if repo == "" || !strings.Contains(repo, "/") {
		return ReviewerView{}, fmt.Errorf("repo must be owner/name, got %q", repo)
	}
	// Accept either spelling: the login (chatgpt-codex-connector[bot]) or the
	// short config name (codex), which is what CRQ_COBOTS already takes.
	// Resolve against the REGISTRY, not the fleet's enabled list. Restricting a
	// project to bots the fleet already runs would make this feature only ever
	// subtract, when the point is choosing different reviewers per project.
	// Either spelling works: the login, or the short name CRQ_COBOTS takes.
	known := map[string]string{}
	for _, co := range dialect.KnownCoReviewers() {
		known[dialect.NormalizeBotName(co.Login)] = co.Login
		known[strings.ToLower(strings.TrimSpace(co.Name))] = co.Login
	}
	resolve := func(allowed map[string]string, list []string, what string) ([]string, error) {
		out := make([]string, 0, len(list))
		for _, name := range list {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			login, ok := allowed[dialect.NormalizeBotName(name)]
			if !ok {
				login, ok = allowed[strings.ToLower(name)]
			}
			if !ok {
				return nil, fmt.Errorf("%s: unknown reviewer %q (known: %s)", what, name, strings.Join(knownLogins(allowed), ", "))
			}
			out = append(out, login)
		}
		return out, nil
	}

	// Start from the existing override so a call that names one half leaves the
	// other alone — the contract the nil/empty distinction promises.
	ov := RepoReviewers{}
	if st, _, err := s.store.Load(ctx); err == nil {
		if existing, ok := st.RepoOverride(repo); ok {
			ov = existing
		}
	} else {
		return ReviewerView{}, err
	}

	if coBots != nil {
		resolved, err := resolve(known, coBots, "--bots")
		if err != nil {
			return ReviewerView{}, err
		}
		ov.CoBots, ov.SetCoBots = resolved, true
	}
	if required != nil {
		if len(required) == 0 {
			// Gating on nobody means Feedback reports converged before anything
			// runs, so `crq next` says done and the reviewers chosen by --bots
			// never review at all.
			return ReviewerView{}, errors.New("--required cannot be empty: a round that gates on nobody converges before any reviewer runs (crq reviewers clear <repo> to drop the override)")
		}
		// The primary may gate here even though it cannot be replaced here.
		allowed := map[string]string{dialect.NormalizeBotName(s.cfg.Bot): s.cfg.Bot}
		for k, v := range known {
			allowed[k] = v
		}
		resolved, err := resolve(allowed, required, "--required")
		if err != nil {
			return ReviewerView{}, err
		}
		ov.Required, ov.SetRequired = resolved, true
	}
	now := s.clock().UTC()
	ov.UpdatedAt = &now
	ov.By = s.cfg.Host

	state, err := s.store.Update(ctx, func(st *State) error {
		st.SetRepoOverride(repo, ov)
		return nil
	})
	if err != nil {
		return ReviewerView{}, err
	}
	s.sync(ctx, state)
	return s.Reviewers(ctx, repo)
}

// ClearReviewers returns repo to the fleet default.
func (s *Service) ClearReviewers(ctx context.Context, repo string) (ReviewerView, error) {
	repo = NormalizeRepo(repo)
	state, err := s.store.Update(ctx, func(st *State) error {
		if !st.ClearRepoOverride(repo) {
			return ErrNoChange
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return ReviewerView{}, err
	}
	if err == nil {
		s.sync(ctx, state)
	}
	return s.Reviewers(ctx, repo)
}

// knownLogins is the deduplicated set of accepted reviewers, for an error a
// caller can act on. The map holds each bot twice (login and short name), so it
// is the values that must be deduplicated, not the keys.
func knownLogins(known map[string]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(known))
	for _, login := range known {
		if seen[login] {
			continue
		}
		seen[login] = true
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}
