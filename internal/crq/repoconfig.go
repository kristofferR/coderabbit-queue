package crq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
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
	Lagging []string `json:"lagging_hosts,omitempty"`
	// PrimaryOff says the metered primary does not review this repository —
	// which is why it is absent from Reviewers below. Without it the list reads
	// as a fleet that never had one.
	PrimaryOff bool   `json:"primary_off,omitempty"`
	Primary    string `json:"primary,omitempty"`
	// LaggingPrimaryOff names hosts that understand per-repo overrides but not
	// this switch, and would still fire the primary here.
	LaggingPrimaryOff []string         `json:"lagging_primary_off,omitempty"`
	UpdatedAt         string           `json:"updated_at,omitempty"`
	By                string           `json:"by,omitempty"`
	Reviewers         []ReviewerDetail `json:"reviewers"`
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
	view := ReviewerView{Repo: repo, Reviewers: []ReviewerDetail{}, Primary: cfg.Bot}
	view.Lagging = st.LaggingWriters(CapsRepoOverrides, s.clock().UTC())
	if ov, ok := st.RepoOverride(repo); ok {
		view.Overridden = true
		view.PrimaryOff = ov.PrimaryOff
		if ov.PrimaryOff {
			view.LaggingPrimaryOff = st.LaggingWriters(CapsPrimaryOff, s.clock().UTC())
		}
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
// primary is nil to leave the primary switch alone, or points at whether the
// primary runs here at all. WHICH bot is primary is still not settable: its
// markers and command are injected into the dialect classifiers when the
// Service is built, so a per-repo primary would mean per-repo classifiers.
// Turning it off is a different question, and one a private repository on a
// free plan has to be able to answer.
func (s *Service) SetReviewers(ctx context.Context, repo string, coBots, required []string, primary *bool) (ReviewerView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return ReviewerView{}, err
	}
	if coBots == nil && required == nil && primary == nil {
		// Neither half was named, so there is nothing to set. Writing anyway would
		// stamp a fresh UpdatedAt, and that timestamp IS the override's identity:
		// applyFire revalidates against it, so a no-op call would discard every
		// in-flight fire decision for the repository.
		return s.Reviewers(ctx, repo)
	}

	// Both halves are resolved here; the MERGE happens inside the CAS closure,
	// because two hosts setting different halves would otherwise derive from the
	// same snapshot and the later write would drop the earlier one's half — and
	// a retry that reuses an already-merged value cannot fix that.
	var setCoBots []string
	if coBots != nil {
		resolved, err := resolveCoBotLogins(coBots)
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
		beforeOverride := ov
		beforeConfig := s.cfgFor(*st, repo)
		if coBots != nil {
			ov.CoBots, ov.SetCoBots = setCoBots, true
		}
		if required != nil {
			// Resolve against the fleet primary from the SAME state revision
			// this override is written to. The process's startup primary may
			// have been replaced through shared settings. Existing custom gates
			// are accepted too: the dashboard has to be able to preserve one
			// inherited from CRQ_REQUIRED_BOTS while editing a registry bot.
			resolved, err := resolveRequiredLoginsPreserving(required, beforeConfig.Bot, beforeConfig.RequiredBots)
			if err != nil {
				return err
			}
			ov.Required, ov.SetRequired = resolved, true
		}
		if primary != nil {
			ov.PrimaryOff = !*primary
		}
		if ov.SetCoBots == beforeOverride.SetCoBots &&
			ov.SetRequired == beforeOverride.SetRequired &&
			ov.PrimaryOff == beforeOverride.PrimaryOff &&
			sameLogins(ov.CoBots, beforeOverride.CoBots) &&
			sameLogins(ov.Required, beforeOverride.Required) {
			return ErrNoChange
		}
		if primary != nil && !*primary && claimedTriggerRepo(st, repo, now) {
			return errors.New("a review trigger is already being posted; wait for it to finish before turning the primary off")
		}
		// Resolved from the FLEET-layered configuration, the one cfgFor hands
		// the queue — not from this host's env. Where the two disagree, the env
		// answer is the wrong one twice over: it can accept a save that leaves
		// the effective required set empty (host env requires Codex, the fleet
		// requires the primary, and turning the primary off passes a check
		// nothing later applies), and it decides the before/after requeue from
		// reviewers this repository does not have.
		base := s.cfg.WithFleet(st.Fleet)
		// Checked against the RESOLVED configuration rather than the lists as
		// typed, because the two disagree in exactly the case that matters: the
		// fleet default requires the primary, so turning it off here empties the
		// gate without anyone naming an empty list. A round gating on nobody
		// converges before any reviewer runs, so refuse it at edit time.
		if len(base.ForRepo(ov).RequiredBots) == 0 {
			return fmt.Errorf("that would leave %s with no required reviewer, so every round would converge before any bot answers — require a co-reviewer first", repo)
		}
		ov.UpdatedAt, ov.By = &now, s.cfg.Host
		before := base.ForRepo(mustOverride(st, repo))
		st.SetRepoOverride(repo, ov)
		s.reopenForChangedReviewers(st, repo, before, base.ForRepo(ov), open)
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
		// Clearing returns the repository to the FLEET default, so that is what
		// the "after" side has to be — s.cfg is this host's env, which is only
		// the same thing when the fleet has recorded nothing.
		base := s.cfg.WithFleet(st.Fleet)
		before := base.ForRepo(mustOverride(st, repo))
		if !st.ClearRepoOverride(repo) {
			return ErrNoChange
		}
		s.reopenForChangedReviewers(st, repo, before, base, open)
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
//
// Exactly two nonempty components: "owner/", "/name" and "owner/name/extra" name
// no repository, and the read path never contacts GitHub — so a typo would
// otherwise print the fleet default and exit 0, reading as a report about a
// project crq follows.
func checkRepoShape(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
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

// reopenForChangedReviewers updates this repository's live rounds when the
// effective reviewer set changed.
//
// A completed round is the "this head was reviewed" dedup marker, so adding a
// required reviewer would otherwise strand the PR: Feedback reports the new bot
// pending, while Enqueue keeps skipping the head because the completed round is
// still there. No eligible round exists to trigger it, and `crq next` waits for
// a push that has no reason to come.
//
// Optional co-reviewers count too: once one has participation evidence,
// Completion waits for it, and its trigger/self-heal needs an active round to
// run. Existing active rounds receive the same one-shot force as reopened ones:
// an in-flight self-heal reviewer with no activity cannot otherwise know it was
// just required. Completed rounds are reopened only when their pull request is
// still open. Rounds are never deleted, so a repository's merged and closed PRs
// stay behind as completed dedup markers: requeueing those would hand Pump
// hundreds of dead rounds to observe and drop one per tick, ahead of every real
// one, and a stranded PR is by definition an open one.
//
// A closed PR's round is marked instead of requeued, because closed is not
// final: reopened at the same head, its completed round would be the dedup
// marker that hides the requirement the operator added while it was shut. The
// mark costs nothing until an enqueue finds the PR alive again.
//
// The primary is not re-asked: DecideFire's already-reviewed gate now counts a
// completion reply paired to the round's command, not only a submitted Review
// object, so a reopened round that the primary already answered dedupes instead
// of buying a second review.
func (s *Service) reopenForChangedReviewers(st *State, repo string, before, after Config, open map[int]bool) {
	beforeCo := before.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
	afterCo := after.reviewerLogins(func(r Reviewer) bool { return !r.Metered() })
	if sameReviewers(before, after) {
		return
	}
	// Only ADDING a reviewer can invalidate a finished round. A round that
	// converged did so with the reviewers it had; taking one away leaves it
	// converged with MORE evidence than the new configuration asks for, and
	// re-reviewing it buys nothing.
	//
	// This is not a nicety. Removing two co-reviewers from a fleet default
	// proposed reopening seventeen completed rounds — seventeen metered
	// reviews, against a shared allowance, to reach the answer already on
	// record. A narrowing must be free.
	added := addedReviewers(before, after, beforeCo, afterCo)
	for _, round := range st.Rounds {
		if NormalizeRepo(round.Repo) != NormalizeRepo(repo) {
			continue
		}
		forced := forcedCoReviewers(round.ForceCoReviewers, before, after)
		switch round.Phase {
		case PhaseQueued, PhaseReserved, PhaseFired, PhaseReviewing, PhaseAwaitingRetry:
			if !sameLogins(round.ForceCoReviewers, forced) {
				updated := round
				updated.ForceCoReviewers = forced
				st.PutRound(updated)
			}
			continue
		case PhaseCompleted:
			if !added {
				// Narrowed only: the round's answer still stands.
				continue
			}
		default:
			continue
		}
		if !open[round.PR] {
			if !round.ReviewersChanged || !sameLogins(round.ForceCoReviewers, forced) {
				marked := round
				marked.ReviewersChanged = true
				marked.ForceCoReviewers = forced
				st.PutRound(marked)
			}
			continue
		}
		reopened := round
		if err := reopened.Reopen(); err != nil {
			continue
		}
		reopened.ForceCoReviewers = forced
		st.PutRound(reopened)
		if s.log != nil {
			s.log.Printf("reviewers: requeued %s#%d@%s — the reviewer set changed", round.Repo, round.PR, round.Head)
		}
	}
}

// forcedCoReviewers carries the one exceptional trigger an existing round
// needs. A newly enabled or required self-heal bot has no activity on that head,
// so its normal mode cannot decide it missed anything; force it once without
// changing the repository's steady-state trigger policy.
func forcedCoReviewers(existing []string, before, after Config) []string {
	var out []string
	for _, cb := range after.CoBots {
		if cb.Trigger != engine.TriggerSelfHeal || cb.Command == "" {
			continue
		}
		newlyEnabled := !containsCoBot(before.CoBots, cb.Login)
		newlyRequired := cb.Required && !containsBot(before.RequiredBots, cb.Login)
		if containsBot(existing, cb.Login) || newlyEnabled || newlyRequired {
			out = append(out, cb.Login)
		}
	}
	return out
}

func containsCoBot(bots []CoBotConfig, login string) bool {
	for _, cb := range bots {
		if sameBot(cb.Login, login) {
			return true
		}
	}
	return false
}

// requeueIfReviewersChanged reopens a completed round that a reviewer change
// marked while its pull request was closed, and reports whether it did. This is
// the other half of reopenForChangedReviewers: the enqueue paths call it when
// the PR turns out to be alive after all, which is the moment the round stops
// being a harmless dead marker and starts being the thing that strands the PR.
func requeueIfReviewersChanged(st *State, r *Round) bool {
	if r == nil || r.Phase != PhaseCompleted || !r.ReviewersChanged {
		return false
	}
	if err := r.Reopen(); err != nil {
		return false
	}
	st.PutRound(*r)
	return true
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

// sameReviewers reports whether two resolved configurations ask the same bots to
// review and gate. It is the question every requeue path starts from, wherever
// the change came from: a per-repo override, a fleet default, or a fleet-wide
// env setting that happens to name the primary.
func sameReviewers(before, after Config) bool {
	return sameLogins(before.RequiredBots, after.RequiredBots) &&
		sameLogins(before.reviewerLogins(func(Reviewer) bool { return true }),
			after.reviewerLogins(func(Reviewer) bool { return true })) &&
		sameTriggerPolicies(before.Reviewers, after.Reviewers)
}

func sameTriggerPolicies(a, b []Reviewer) bool {
	if len(a) != len(b) {
		return false
	}
	triggers := make(map[string]engine.TriggerMode, len(a))
	for _, reviewer := range a {
		triggers[dialect.NormalizeBotName(reviewer.Login)] = reviewer.Trigger
	}
	for _, reviewer := range b {
		trigger, ok := triggers[dialect.NormalizeBotName(reviewer.Login)]
		if !ok || trigger != reviewer.Trigger {
			return false
		}
	}
	return true
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

// resolveCoBotLogins turns whichever spelling a caller used — the login
// (chatgpt-codex-connector[bot]) or the short config name (codex) — into
// logins, rejecting anything the registry does not know.
//
// Resolved against the REGISTRY, not the fleet's enabled list: restricting a
// choice to bots the fleet already runs would make the feature only ever
// subtract, when the point is choosing DIFFERENT reviewers.
func resolveCoBotLogins(list []string) ([]string, error) {
	return resolveBotList(coReviewerNames(), list, "--bots")
}

// resolveRequiredLogins additionally accepts the primary, which may gate even
// though it cannot be replaced per repository.
func resolveRequiredLogins(list []string, primary string) ([]string, error) {
	allowed := coReviewerNames()
	if primary != "" {
		allowed[dialect.NormalizeBotName(primary)] = primary
	}
	return resolveBotList(allowed, list, "--required")
}

// resolveRequiredLoginsPreserving additionally accepts non-registry gates that
// already exist in the repository's effective configuration. They cannot be
// added through the per-repository editor, but an unrelated edit must not make
// an inherited CRQ_REQUIRED_BOTS login impossible to retain.
func resolveRequiredLoginsPreserving(list []string, primary string, existing []string) ([]string, error) {
	allowed := coReviewerNames()
	if primary != "" {
		allowed[dialect.NormalizeBotName(primary)] = primary
	}
	for _, login := range existing {
		login = strings.TrimSpace(login)
		if login == "" {
			continue
		}
		allowed[dialect.NormalizeBotName(login)] = login
		allowed[strings.ToLower(login)] = login
	}
	return resolveBotList(allowed, list, "--required")
}

// coReviewerNames maps every accepted spelling to its login. Each bot appears
// twice, which is why knownLogins deduplicates by value.
func coReviewerNames() map[string]string {
	known := map[string]string{}
	for _, co := range dialect.KnownCoReviewers() {
		known[dialect.NormalizeBotName(co.Login)] = co.Login
		known[strings.ToLower(strings.TrimSpace(co.Name))] = co.Login
	}
	return known
}

func resolveBotList(allowed map[string]string, list []string, what string) ([]string, error) {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
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
		// Both spellings of one bot resolve to one login, so a caller that sent
		// "codex" and "chatgpt-codex-connector[bot]" named one reviewer, not two.
		// Storing it twice would render it twice and read as a configuration
		// mistake nobody made.
		if seen[dialect.NormalizeBotName(login)] {
			continue
		}
		seen[dialect.NormalizeBotName(login)] = true
		out = append(out, login)
	}
	return out, nil
}

// addedReviewers reports whether the new configuration asks for evidence the
// old one did not — a newly required reviewer, a newly enabled co-reviewer, or
// a trigger policy that newly asks an existing reviewer to run. A pure removal
// returns false, which is what makes narrowing free.
func addedReviewers(before, after Config, beforeCo, afterCo []string) bool {
	for _, login := range after.RequiredBots {
		if !containsBot(before.RequiredBots, login) {
			return true
		}
	}
	for _, login := range afterCo {
		if !containsBot(beforeCo, login) {
			return true
		}
	}
	for _, reviewer := range after.Reviewers {
		if reviewer.Metered() {
			if !containsBot(before.reviewerLogins(func(r Reviewer) bool { return r.Metered() }), reviewer.Login) {
				return true
			}
			continue
		}
		if reviewer.Trigger == engine.TriggerNever {
			continue
		}
		for _, old := range before.Reviewers {
			if sameBot(old.Login, reviewer.Login) &&
				triggerPolicyRank(reviewer.Trigger) > triggerPolicyRank(old.Trigger) {
				return true
			}
		}
	}
	return false
}

func triggerPolicyRank(mode engine.TriggerMode) int {
	switch mode {
	case engine.TriggerAlways:
		return 2
	case engine.TriggerSelfHeal:
		return 1
	default:
		return 0
	}
}
