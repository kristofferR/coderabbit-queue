package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
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
	repo = NormalizeRepo(repo)
	// The same shape check set and clear do. A malformed target reads no
	// override, so reporting it would answer with the fleet default and exit 0 —
	// telling the caller its typo is a repository crq is following.
	if err := checkRepoShape(repo); err != nil {
		return ReviewerView{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return ReviewerView{}, err
	}
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
	if err := checkRepoShape(repo); err != nil {
		return ReviewerView{}, err
	}
	if coBots == nil && required == nil {
		// Neither half was named, so there is nothing to set. Writing anyway would
		// stamp a fresh UpdatedAt, and that timestamp IS the override's identity:
		// applyFire revalidates against it, so a no-op call would discard every
		// in-flight fire decision for the repository.
		return s.Reviewers(ctx, repo)
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

	// Both halves are resolved here; the MERGE happens inside the CAS closure,
	// because two hosts setting different halves would otherwise derive from the
	// same snapshot and the later write would drop the earlier one's half — and
	// a retry that reuses an already-merged value cannot fix that.
	var setCoBots, setRequired []string
	if coBots != nil {
		resolved, err := resolve(known, coBots, "--bots")
		if err != nil {
			return ReviewerView{}, err
		}
		setCoBots = resolved
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
		setRequired = resolved
	}

	// Read before the write: the requeue below may only touch live pull requests,
	// and the CAS closure cannot ask GitHub. A failed lookup fails the whole
	// command, which is retryable — writing the override and skipping the requeue
	// would strand exactly the PRs the requeue exists for.
	open, err := s.openPRs(ctx, repo)
	if err != nil {
		return ReviewerView{}, err
	}
	now := s.clock().UTC()
	state, err := s.store.Update(ctx, func(st *State) error {
		ov, _ := st.RepoOverride(repo)
		if coBots != nil {
			ov.CoBots, ov.SetCoBots = setCoBots, true
		}
		if required != nil {
			ov.Required, ov.SetRequired = setRequired, true
		}
		ov.UpdatedAt, ov.By = &now, s.cfg.Host
		before := s.cfg.ForRepo(mustOverride(st, repo))
		st.SetRepoOverride(repo, ov)
		s.reopenForChangedReviewers(st, repo, before, s.cfg.ForRepo(ov), open)
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
	// A typo like "owner-repo" would otherwise clear a key nothing uses and exit
	// 0, so automation believes it restored the fleet default while the real
	// override is still in force.
	if err := checkRepoShape(repo); err != nil {
		return ReviewerView{}, err
	}
	open, err := s.openPRs(ctx, repo)
	if err != nil {
		return ReviewerView{}, err
	}
	state, err := s.store.Update(ctx, func(st *State) error {
		before := s.cfg.ForRepo(mustOverride(st, repo))
		if !st.ClearRepoOverride(repo) {
			return ErrNoChange
		}
		s.reopenForChangedReviewers(st, repo, before, s.cfg, open)
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

// checkRepoShape is the one repository-shape check every reviewers path applies,
// so reading a target can never succeed where setting it would fail.
func checkRepoShape(repo string) error {
	if repo == "" || !strings.Contains(repo, "/") {
		return fmt.Errorf("repo must be owner/name, got %q", repo)
	}
	return nil
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

// mustOverride is the repo's override, or the zero value meaning "fleet
// default" — the same thing ForRepo treats as no override.
func mustOverride(st *State, repo string) RepoReviewers {
	ov, _ := st.RepoOverride(repo)
	return ov
}

// reopenForChangedReviewers requeues this repository's completed rounds when the
// set of reviewers that gates them changed.
//
// A completed round is the "this head was reviewed" dedup marker, so adding a
// required reviewer would otherwise strand the PR: Feedback reports the new bot
// pending, while Enqueue keeps skipping the head because the completed round is
// still there. No eligible round exists to trigger it, and `crq next` waits for
// a push that has no reason to come.
//
// Only rounds whose required set actually changed are touched, only completed
// ones — an in-flight round is already going to answer — and only those whose
// pull request is still open. Rounds are never deleted, so a repository's merged
// and closed PRs stay behind as completed dedup markers: requeueing those would
// hand Pump hundreds of dead rounds to observe and drop one per tick, ahead of
// every real one, and a stranded PR is by definition an open one.
//
// The primary is not re-asked: DecideFire's already-reviewed gate now counts a
// completion reply paired to the round's command, not only a submitted Review
// object, so a reopened round that the primary already answered dedupes instead
// of buying a second review.
func (s *Service) reopenForChangedReviewers(st *State, repo string, before, after Config, open map[int]bool) {
	if sameLogins(before.RequiredBots, after.RequiredBots) {
		return
	}
	for _, round := range st.Rounds {
		if NormalizeRepo(round.Repo) != NormalizeRepo(repo) || round.Phase != PhaseCompleted {
			continue
		}
		if !open[round.PR] {
			continue
		}
		reopened := round
		if err := reopened.Reopen(); err != nil {
			continue
		}
		st.PutRound(reopened)
		if s.log != nil {
			s.log.Printf("reviewers: requeued %s#%d@%s — the required set changed", round.Repo, round.PR, round.Head)
		}
	}
}

// openPRs is the set of repo's currently open pull request numbers — the only
// ones a reviewer change can strand.
func (s *Service) openPRs(ctx context.Context, repo string) (map[int]bool, error) {
	open := map[int]bool{}
	err := s.gh.EachOpenPR(ctx, repo, true, func(pr ghapi.SearchPR) (bool, error) {
		if NormalizeRepo(pr.Repo) == repo {
			open[pr.Number] = true
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return open, nil
}

// sameLogins compares two reviewer lists as sets, since order is presentation.
func sameLogins(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, login := range a {
		seen[dialect.NormalizeBotName(login)]++
	}
	for _, login := range b {
		key := dialect.NormalizeBotName(login)
		seen[key]--
		if seen[key] < 0 {
			return false
		}
	}
	return true
}
