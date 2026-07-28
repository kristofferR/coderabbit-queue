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

// EnrollmentView is one repository's enrollment as it will actually be applied.
type EnrollmentView struct {
	Repo string `json:"repo"`
	// Source is where the answer comes from, which matters more than the answer:
	// a repository forced on by a host's env cannot be turned off from here, and
	// saying "managed" would invite someone to try.
	//
	//	state    — a record in shared state decided it
	//	env      — this host's CRQ_REPOS lists it, with no record either way
	//	excluded — this host's CRQ_EXCLUDE names it (absolute; a kill switch)
	//	scope    — no allow-list at all, so everything in CRQ_SCOPE is reviewed
	//	off      — no record, no env mention, and an allow-list that omits it
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	// EnvConflict says the host's env and the shared record disagree. The record
	// wins, but silently overriding a file someone edited is how a fleet grows a
	// mystery, so it is reported.
	EnvConflict bool     `json:"env_conflict,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	By          string   `json:"by,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	Lagging     []string `json:"lagging_hosts,omitempty"`
}

// Enrollment reports whether crq reviews repo, and why.
func (s *Service) Enrollment(ctx context.Context, repo string) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return EnrollmentView{}, err
	}
	return s.enrollmentOf(st, repo), nil
}

// enrollmentOf resolves one repository against the record and this host's env.
//
// Precedence, and the reasoning for it: CRQ_EXCLUDE is absolute, because it is a
// per-host kill switch and the machine that has one usually has a reason the
// fleet does not know. Otherwise an explicit record wins in BOTH directions — an
// Off switch that does nothing but explain which file to go and edit on another
// machine is not a switch. Env alone still enrolls, so nothing changes for a
// fleet that never touches this.
func (s *Service) enrollmentOf(st State, repo string) EnrollmentView {
	repo = NormalizeRepo(repo)
	view := EnrollmentView{Repo: repo}
	inEnv := s.cfg.AllowRepos[repo]
	switch {
	case s.cfg.ExcludeRepos[repo]:
		view.Source, view.Enabled = "excluded", false
		return view
	case repo == NormalizeRepo(s.cfg.GateRepo):
		// The gate repository holds the queue's own state and dashboard;
		// reviewing it would be crq reviewing its own bookkeeping.
		view.Source, view.Enabled = "excluded", false
		return view
	}
	if rec, ok := st.Enrollment(repo); ok {
		view.Source, view.Enabled = "state", rec.Enabled
		view.Reason, view.By = rec.Reason, rec.By
		if rec.UpdatedAt != nil {
			view.UpdatedAt = rec.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		// Only one direction is a disagreement: a record that turns off a
		// repository a host's CRQ_REPOS lists. A record that turns one ON that
		// env never mentioned is the feature working, not a conflict.
		view.EnvConflict = inEnv && !rec.Enabled
		view.Lagging = st.LaggingWriters(CapsEnrollment, s.clock().UTC())
		return view
	}
	switch {
	case inEnv:
		view.Source, view.Enabled = "env", true
	case len(s.cfg.AllowRepos) == 0:
		view.Source, view.Enabled = "scope", true
	default:
		view.Source, view.Enabled = "off", false
	}
	return view
}

// SetEnrollment records whether crq reviews repo. Turning one off needs a
// reason: the repository disappears from every queue, and "why did this stop
// being reviewed" is a question the fleet should be able to answer itself.
func (s *Service) SetEnrollment(ctx context.Context, repo string, enabled bool, reason string) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	if !enabled && strings.TrimSpace(reason) == "" {
		return EnrollmentView{}, errors.New("turning a repository off needs --reason (every screen that shows it will show why)")
	}
	if s.cfg.ExcludeRepos[repo] {
		return EnrollmentView{}, fmt.Errorf("%s is in CRQ_EXCLUDE on %s — that is a per-host kill switch and shared state does not override it", repo, s.cfg.Host)
	}
	if repo == NormalizeRepo(s.cfg.GateRepo) {
		return EnrollmentView{}, fmt.Errorf("%s is the gate repository: it holds crq's own state and dashboard", repo)
	}
	now := s.clock().UTC()
	st, err := s.store.Update(ctx, func(st *State) error {
		if cur, ok := st.Enrollment(repo); ok && cur.Enabled == enabled && cur.Reason == reason {
			return ErrNoChange
		}
		st.SetEnrollment(repo, RepoEnrollment{
			Enabled: enabled, Reason: reason, By: s.cfg.Host, UpdatedAt: &now,
		})
		if !enabled {
			s.abandonPendingRounds(st, repo)
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return EnrollmentView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else {
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return EnrollmentView{}, err
		}
	}
	return s.enrollmentOf(st, repo), nil
}

// abandonPendingRounds drops the rounds a repository being turned off would
// otherwise still fire.
//
// The off switch is advertised as "crq does not go here", and every SCAN path
// honours it — but Pump chooses from Rounds through NextEligible, which asks
// nothing about enrollment. A repository with a queued or awaiting-retry round
// therefore kept its place in the queue and spent the shared allowance on a
// metered review minutes after being stopped.
//
// Only the phases that have not spent anything are touched. A fired or
// reviewing round already posted its command and holds the fire slot; the money
// is gone and the answer is worth having, so it is left to finish.
func (s *Service) abandonPendingRounds(st *State, repo string) {
	for _, round := range st.Rounds {
		if NormalizeRepo(round.Repo) != NormalizeRepo(repo) {
			continue
		}
		switch round.Phase {
		case PhaseQueued, PhaseAwaitingRetry:
		default:
			continue
		}
		// EndRound, not a bare Abandon: it archives the round rather than
		// leaving it in Rounds as a "this head was dealt with" marker. Turning
		// the repository back on then enqueues its current head again, which is
		// what an off switch that can be undone has to mean.
		st.EndRound(round.Repo, round.PR, "repository turned off")
		releaseSlot(st, QueueKey(round.Repo, round.PR))
		if s.log != nil {
			s.log.Printf("enrollment: dropped queued round %s#%d@%s — the repository was turned off",
				round.Repo, round.PR, round.Head)
		}
	}
}

// ClearEnrollment drops the record, handing the repository back to the hosts'
// env files.
func (s *Service) ClearEnrollment(ctx context.Context, repo string) (EnrollmentView, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollmentView{}, err
	}
	st, err := s.store.Update(ctx, func(st *State) error {
		if !st.ClearEnrollment(repo) {
			return ErrNoChange
		}
		// Clearing hands the repository back to this host's env, which may well
		// not list it: a record that said ON becomes an effective OFF without
		// SetEnrollment ever being called. Pump chooses from Rounds without
		// rechecking enrollment, so the queued rounds have to go the same way
		// they do when the switch is thrown explicitly — resolved from the state
		// the write lands on, not from the one before the clear.
		if !s.enrollmentOf(*st, repo).Enabled {
			s.abandonPendingRounds(st, repo)
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNoChange) {
		return EnrollmentView{}, err
	}
	if err == nil {
		s.sync(ctx, st)
	} else {
		st, _, err = s.store.Load(ctx)
		if err != nil {
			return EnrollmentView{}, err
		}
	}
	return s.enrollmentOf(st, repo), nil
}

// Enrollments lists every repository this host would act on, plus every one a
// record mentions — including the ones turned off, because an "off" nobody can
// see is how a repository quietly stops being reviewed.
func (s *Service) Enrollments(ctx context.Context) ([]EnrollmentView, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var repos []string
	add := func(repo string) {
		repo = NormalizeRepo(repo)
		if repo == "" || seen[repo] {
			return
		}
		seen[repo] = true
		repos = append(repos, repo)
	}
	for repo := range s.cfg.AllowRepos {
		add(repo)
	}
	for _, repo := range st.EnrolledRepos() {
		add(repo)
	}
	sort.Strings(repos)
	out := make([]EnrollmentView, 0, len(repos))
	for _, repo := range repos {
		out = append(out, s.enrollmentOf(st, repo))
	}
	return out, nil
}

// reviewsRepo is the one question every scan path asks: may this host enqueue
// work for repo? Sharing it is what keeps autoreview, watch and autofix from
// each growing their own slightly different answer.
func (s *Service) reviewsRepo(st State, repo string) bool {
	return s.enrollmentOf(st, repo).Enabled
}

// scanTargets is the list a pass should search: every repository enrolled by
// env or by record. Empty means "no allow-list anywhere", which is the signal to
// search CRQ_SCOPE owner-wide instead.
func (s *Service) scanTargets(st State) []string {
	// An empty CRQ_REPOS means this host searches CRQ_SCOPE owner-wide. Records
	// must not narrow that to themselves: enrolling one repository would then
	// silently stop every other one from being scanned.
	if len(s.cfg.AllowRepos) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for repo := range s.cfg.AllowRepos {
		if s.reviewsRepo(st, repo) && !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	for _, repo := range st.EnrolledRepos() {
		if s.reviewsRepo(st, repo) && !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	sort.Strings(out)
	return out
}

// EnrollmentIn answers for an already-loaded state, so a caller rendering many
// repositories does not re-read the ref once per row.
func (s *Service) EnrollmentIn(st State, repo string) EnrollmentView {
	return s.enrollmentOf(st, repo)
}

// ScopeRepos lists the repositories in CRQ_SCOPE, for choosing one to enroll.
// It is the one genuinely expensive read in the dashboard — a multi-page REST
// walk per owner — so callers are expected to cache it.
func (s *Service) ScopeRepos(ctx context.Context) ([]ghapi.Repo, error) {
	seen := map[string]bool{}
	var out []ghapi.Repo
	for _, owner := range s.cfg.Scope {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		repos, err := s.gh.ListOwnerRepos(ctx, owner, 300)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			key := NormalizeRepo(r.FullName)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// EnrollImpact is what enrolling a repository would actually do, before it is
// done.
//
// This is the one click in the product that can spend real money: a repository
// with a dozen open pull requests becomes a dozen metered reviews on the next
// pass. The dialog that offers it should say so in the terms the bill arrives
// in, not in "7 pull requests".
type EnrollImpact struct {
	Repo string `json:"repo"`
	// Open is every open pull request; Eligible is what would actually be
	// enqueued once the skip rules are applied. The gap between them is worth
	// showing: "12 open, 9 eligible" answers the question the raw count raises.
	Open     int `json:"open"`
	Eligible int `json:"eligible"`
	// Skipped explains the gap, per reason.
	Skipped map[string]int `json:"skipped,omitempty"`
	// Metered is how many of those would spend the shared review allowance.
	Metered int `json:"metered"`
	// Low/High bound the cost of reviewing the backlog, summed over the
	// eligible pull requests. Estimates, with the same honesty as crq cost.
	Low  float64 `json:"low"`
	High float64 `json:"high"`
	// Unpriced counts the pull requests whose cost could not be read — a spent
	// REST quota, an unreadable diff, or a reviewer crq has no price for. They
	// are reported rather than dropped:
	// leaving them out of the total makes an unknown price look like a free one,
	// which is the one thing this dialog exists to prevent.
	Unpriced        int    `json:"unpriced,omitempty"`
	Summary         string `json:"summary"`
	PricesCheckedAt string `json:"prices_checked_at"`
}

// PreviewEnroll reports what enrolling repo would do. It costs one pull-request
// read per open pull request, which is why it is a separate call the dialog
// makes rather than something every repository row carries.
func (s *Service) PreviewEnroll(ctx context.Context, repo string) (EnrollImpact, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return EnrollImpact{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return EnrollImpact{}, err
	}
	cfg := s.cfgFor(st, repo)
	impact := EnrollImpact{Repo: repo, Skipped: map[string]int{}, PricesCheckedAt: dialect.PricesCheckedAt}

	var eligible []int
	err = s.gh.EachOpenPR(ctx, repo, true, func(pr ghapi.SearchPR) (bool, error) {
		if NormalizeRepo(pr.Repo) != repo {
			return false, nil
		}
		impact.Open++
		switch {
		case cfg.SkipAuthors[dialect.NormalizeBotName(strings.ToLower(pr.Author))]:
			impact.Skipped["author is skipped"]++
		case cfg.SkipsReview(pr.Body):
			impact.Skipped["carries the skip marker"]++
		default:
			eligible = append(eligible, pr.Number)
		}
		return false, nil
	})
	if err != nil {
		return impact, err
	}
	impact.Eligible = len(eligible)

	// One read per eligible pull request, for the diff each would be reviewed
	// at. Bounded, because a dialog that takes a minute to open is a dialog
	// nobody waits for — and an under-count is reported rather than hidden.
	const maxPriced = 25
	priced := eligible
	if len(priced) > maxPriced {
		priced = priced[:maxPriced]
	}
	for _, pr := range priced {
		// costFrom, not Cost: the state is already in hand, and Cost would load
		// the ref again per pull request — four requests each, 100 across the
		// bound, for an answer that cannot have changed since the load above.
		pull, cerr := s.gh.GetPull(ctx, repo, pr)
		if cerr != nil {
			impact.Unpriced++
			continue
		}
		cost := s.costFrom(st, repo, pr, pull.Head.SHA, dialect.DiffStat{
			Additions:    pull.Additions,
			Deletions:    pull.Deletions,
			ChangedFiles: pull.ChangedFiles,
		})
		if len(cost.Unpriced) > 0 {
			// A reviewer crq cannot price makes this pull request's total a
			// floor, not an answer. Adding it in would let the sentence below
			// call an unknown price free, which is what it exists to prevent.
			impact.Unpriced++
		}
		impact.Low += cost.Low
		impact.High += cost.High
		for _, r := range cost.Reviewers {
			if dialect.NormalizeBotName(r.Bot) == dialect.NormalizeBotName(cfg.Bot) {
				impact.Metered++
			}
		}
	}
	impact.Summary = enrollSummary(impact, len(priced) < len(eligible))
	return impact, nil
}

func enrollSummary(i EnrollImpact, partial bool) string {
	if i.Eligible == 0 {
		return fmt.Sprintf("%d open pull request(s), none of which would be enqueued", i.Open)
	}
	// "no per-review cost" is only ever said about a backlog crq actually
	// priced. A pull request whose price could not be read is an unknown, and an
	// unknown that renders as free is the one way this sentence can mislead
	// somebody into spending money.
	cost := "no per-review cost"
	switch {
	case i.High > 0 && i.Low != i.High:
		cost = fmt.Sprintf("roughly $%.2f–$%.2f", i.Low, i.High)
	case i.High > 0:
		cost = fmt.Sprintf("about $%.2f", i.High)
	case i.Unpriced > 0:
		cost = "a cost crq could not read"
	}
	out := fmt.Sprintf("would enqueue %d of %d open pull request(s) on the next pass — %s",
		i.Eligible, i.Open, cost)
	if i.Unpriced > 0 && i.High > 0 {
		out += fmt.Sprintf(", plus %d that could not be priced", i.Unpriced)
	}
	if partial {
		out += ", priced over the first 25"
	}
	return out
}
