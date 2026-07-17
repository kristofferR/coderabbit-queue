package crq

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

type fakeGitHub struct {
	mu              sync.Mutex
	pulls           map[string]ghapi.Pull
	commits         map[string]ghapi.Commit
	commitErrs      map[string]error
	reviews         map[string][]ghapi.Review
	comments        map[string][]ghapi.IssueComment
	reviewComments  map[string][]ghapi.ReviewComment
	issueReactions  map[string][]ghapi.Reaction
	reactions       map[int64][]ghapi.Reaction
	posted          []string
	deleted         []int64
	commentID       int64
	createdIssues   []int
	nextIssueNumber int
	postErrs        map[string]error
	graphQL         func(query string, vars map[string]any, out any) error
	searchPRs       []ghapi.SearchPR
	// now, when set, timestamps posted comments off the same injected clock the
	// service uses, so a fire's recorded FiredAt tracks the fake wall clock the
	// replay suite advances. nil falls back to real time (all existing tests).
	now func() time.Time
}

func (f *fakeGitHub) clock() time.Time {
	if f.now != nil {
		return f.now().UTC()
	}
	return time.Now().UTC()
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		pulls:          map[string]ghapi.Pull{},
		commits:        map[string]ghapi.Commit{},
		commitErrs:     map[string]error{},
		reviews:        map[string][]ghapi.Review{},
		comments:       map[string][]ghapi.IssueComment{},
		reviewComments: map[string][]ghapi.ReviewComment{},
		issueReactions: map[string][]ghapi.Reaction{},
		reactions:      map[int64][]ghapi.Reaction{},
	}
}

func fakeKey(repo string, pr int) string { return QueueKey(repo, pr) }

func (f *fakeGitHub) GetPull(_ context.Context, repo string, pr int) (ghapi.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pull, ok := f.pulls[fakeKey(repo, pr)]
	if !ok {
		return ghapi.Pull{}, errors.New("missing pull")
	}
	return pull, nil
}

func (f *fakeGitHub) GetCommit(_ context.Context, repo, sha string) (ghapi.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.commitErrs[sha]; err != nil {
		return ghapi.Commit{}, err
	}
	return f.commits[sha], nil
}

func (f *fakeGitHub) ListReviews(_ context.Context, repo string, pr int) ([]ghapi.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.Review(nil), f.reviews[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListIssueComments(_ context.Context, repo string, pr int) ([]ghapi.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.IssueComment(nil), f.comments[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListReviewComments(_ context.Context, repo string, pr int) ([]ghapi.ReviewComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.ReviewComment(nil), f.reviewComments[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListIssueReactions(_ context.Context, repo string, pr int) ([]ghapi.Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.Reaction(nil), f.issueReactions[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListCommentReactions(_ context.Context, _ string, id int64) ([]ghapi.Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghapi.Reaction(nil), f.reactions[id]...), nil
}

func (f *fakeGitHub) PostIssueComment(_ context.Context, repo string, pr int, body string) (ghapi.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.postErrs[fakeKey(repo, pr)]; err != nil {
		return ghapi.IssueComment{}, err
	}
	f.commentID++
	f.posted = append(f.posted, repo+"#"+strconv.Itoa(pr)+":"+body)
	now := f.clock()
	comment := ghapi.IssueComment{ID: f.commentID, Body: body, CreatedAt: now, UpdatedAt: now}
	comment.User.Login = "kristofferR"
	return comment, nil
}

func (f *fakeGitHub) CreateIssue(_ context.Context, _ string, _ string, _ string) (ghapi.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextIssueNumber == 0 {
		f.nextIssueNumber = 1000
	}
	f.nextIssueNumber++
	f.createdIssues = append(f.createdIssues, f.nextIssueNumber)
	return ghapi.Issue{Number: f.nextIssueNumber, State: "open"}, nil
}

func (f *fakeGitHub) ListIssueCommentsPage(_ context.Context, repo string, pr, page, perPage int) ([]ghapi.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.comments[fakeKey(repo, pr)]
	start := (page - 1) * perPage
	if start < 0 || start >= len(all) {
		return nil, nil
	}
	end := start + perPage
	if end > len(all) {
		end = len(all)
	}
	return append([]ghapi.IssueComment(nil), all[start:end]...), nil
}

func (f *fakeGitHub) DeleteIssueComment(_ context.Context, repo string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, list := range f.comments {
		for i, c := range list {
			if c.ID == id {
				f.comments[key] = append(list[:i], list[i+1:]...)
				f.deleted = append(f.deleted, id)
				return nil
			}
		}
	}
	return nil
}

func (f *fakeGitHub) SearchOpenPRs(context.Context, string, bool, int) ([]ghapi.SearchPR, error) {
	return nil, nil
}

func (f *fakeGitHub) EachOpenPR(_ context.Context, _ string, _ bool, fn func(ghapi.SearchPR) (bool, error)) error {
	f.mu.Lock()
	prs := append([]ghapi.SearchPR(nil), f.searchPRs...)
	f.mu.Unlock()
	for _, pr := range prs {
		stop, err := fn(pr)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

func (f *fakeGitHub) GraphQL(_ context.Context, query string, vars map[string]any, out any) error {
	f.mu.Lock()
	handler := f.graphQL
	f.mu.Unlock()
	if handler != nil {
		return handler(query, vars, out)
	}
	return errors.New("graphql unavailable")
}

// noForcePush is a GraphQL handler reporting no HEAD_REF_FORCE_PUSHED_EVENT, so
// headForcePushCutoff succeeds with a zero cutoff and adoption proceeds. Tests
// exercising successful adoption need it now that a failed force-push lookup
// skips adoption.
func noForcePush(_ string, _ map[string]any, out any) error {
	return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"timelineItems":{"nodes":[]}}}}`), out)
}

// --- test store fakes (v3) ---

type failNthUpdateStore struct {
	StateStore
	n     int
	err   error
	calls int
}

func (s *failNthUpdateStore) Update(ctx context.Context, mutate func(*State) error) (State, error) {
	s.calls++
	if s.calls == s.n {
		return State{}, s.err
	}
	return s.StateStore.Update(ctx, mutate)
}

// retryNoChangeStore invokes the mutate closure twice within one Update: first
// against a state whose round holds the fire slot, then a fresh state with no
// round. It verifies recordFire resets its `recorded` flag between attempts.
type retryNoChangeStore struct{ cfg Config }

func (retryNoChangeStore) Load(context.Context) (State, Revision, error) {
	return DefaultState(Config{}), Revision{}, nil
}

func (s retryNoChangeStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	first := DefaultState(s.cfg)
	firstRound, _ := first.NewRound("owner/repo", 12, "abcdef123", time.Now().UTC())
	_ = firstRound.Reserve("token", "host", time.Now().UTC())
	first.PutRound(*firstRound)
	first.FireSlot = &FireSlot{Key: QueueKey("owner/repo", 12), Token: "token", Since: time.Now().UTC()}
	if err := mutate(&first); err != nil {
		return State{}, err
	}
	second := DefaultState(s.cfg)
	if err := mutate(&second); err != nil {
		if errors.Is(err, ErrNoChange) {
			return second, nil
		}
		return State{}, err
	}
	return second, nil
}

func (retryNoChangeStore) SyncDashboard(context.Context, State) error { return nil }

// adoptionRaceStore loads a queued round with an adoptable command, but every
// Update simulates another worker already holding the fire slot.
type adoptionRaceStore struct {
	cfg       Config
	loadState State
}

func (s *adoptionRaceStore) Load(context.Context) (State, Revision, error) {
	state := cloneState(s.loadState)
	state.Normalize(time.Now().UTC())
	return state, Revision{}, nil
}

func (s *adoptionRaceStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	state := cloneState(s.loadState)
	state.FireSlot = &FireSlot{Key: "owner/repo#99", Token: "other", Since: time.Now().UTC()}
	if err := mutate(&state); err != nil {
		if errors.Is(err, ErrNoChange) {
			return state, nil
		}
		return State{}, err
	}
	return state, nil
}

func (s *adoptionRaceStore) SyncDashboard(context.Context, State) error { return nil }

// --- test helpers ---

func cfgTimeout(cfg Config) time.Duration {
	if cfg.FeedbackWaitTimeout > 0 {
		return cfg.FeedbackWaitTimeout
	}
	return time.Hour
}

// seedRound installs a round for repo#pr at head in the given phase. Fired,
// reviewing, and completed phases record a fire at firedAt with commandID; a
// fired round also holds the global fire slot.
func seedRound(t *testing.T, store StateStore, cfg Config, repo string, pr int, head string, phase Phase, firedAt time.Time, commandID int64) {
	t.Helper()
	_, err := store.Update(context.Background(), func(st *State) error {
		r, err := st.NewRound(repo, pr, head, firedAt)
		if err != nil {
			return err
		}
		switch phase {
		case PhaseQueued:
		case PhaseFired, PhaseReviewing, PhaseCompleted:
			if err := r.Reserve("seedtok", "seedhost", firedAt); err != nil {
				return err
			}
			if err := r.Fire(commandID, firedAt); err != nil {
				return err
			}
			dl := firedAt.Add(cfgTimeout(cfg))
			r.WaitDeadline = &dl
			if phase == PhaseReviewing {
				if err := r.Acknowledge(); err != nil {
					return err
				}
			}
			if phase == PhaseCompleted {
				if err := r.Complete(); err != nil {
					return err
				}
			}
		}
		st.PutRound(*r)
		if phase == PhaseFired {
			st.FireSlot = &FireSlot{Key: QueueKey(repo, pr), Token: "seedtok", Since: firedAt}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func roundPhase(t *testing.T, store StateStore, repo string, pr int) Phase {
	t.Helper()
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := st.Round(repo, pr)
	if r == nil {
		return ""
	}
	return r.Phase
}

func TestPruneCalibrationDeletesOldNoiseKeepsRecent(t *testing.T) {
	gh := newFakeGitHub()
	cfg := Config{
		GateRepo:          "o/gate",
		CalibrationPR:     1,
		Bot:               "coderabbitai[bot]",
		RateLimitCommand:  "@coderabbitai rate limit",
		CalibrationMarker: "auto-generated reply by CodeRabbit",
		Scope:             []string{"o"},
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	mkc := func(id int64, login, body string, at time.Time) ghapi.IssueComment {
		c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		c.User.Login = login
		return c
	}
	key := fakeKey("o/gate", 1)
	gh.comments[key] = []ghapi.IssueComment{
		mkc(1, "kristofferR", "@coderabbitai rate limit", old),
		mkc(2, "coderabbitai[bot]", "0 reviews remaining. auto-generated reply by CodeRabbit", old),
		mkc(3, "someone", "unrelated human comment", old),
		mkc(4, "kristofferR", "@coderabbitai rate limit", now),
	}

	deleted := svc.pruneCalibration(context.Background(), 1, now.Add(-2*time.Minute), 80)
	if deleted != 2 {
		t.Fatalf("expected 2 deletions, got %d", deleted)
	}
	remaining := map[int64]bool{}
	for _, c := range gh.comments[key] {
		remaining[c.ID] = true
	}
	if remaining[1] || remaining[2] {
		t.Fatalf("old calibration noise was not pruned: %v", remaining)
	}
	if !remaining[3] || !remaining[4] {
		t.Fatalf("non-noise or recent comment was wrongly pruned: %v", remaining)
	}
}

func TestAutoReviewOnceReleasesLeader(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h", LeaderTTL: time.Minute}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Leader != nil {
		t.Fatalf("one-shot autoreview should release its leader lease, got %#v", st.Leader)
	}
}

func TestAutoReviewScanSkipsConfiguredAuthors(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:          "o/gate",
		Scope:             []string{"o"},
		Host:              "h",
		Bot:               "coderabbitai[bot]",
		ReviewCommand:     "@coderabbitai review",
		LeaderTTL:         time.Minute,
		AutoReviewMaxScan: 10,
		SkipAuthors:       authorSet("dependabot[bot]"),
	}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{
		{Repo: "o/app", Number: 1, Author: "dependabot[bot]"},
		{Repo: "o/app", Number: 2, Author: "Dependabot"},
		{Repo: "o/app", Number: 3, Author: "alice"},
	}
	for pr := 1; pr <= 3; pr++ {
		var pull ghapi.Pull
		pull.State = "open"
		pull.Head.SHA = "abcdef1234567890"
		gh.pulls[fakeKey("o/app", pr)] = pull
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.FiredMarker("o/app", 3) == "" {
		t.Fatalf("only the human-authored PR should be enqueued and fired, got rounds=%#v", st.Rounds)
	}
	if st.Round("o/app", 1) != nil || st.Round("o/app", 2) != nil {
		t.Fatalf("bot-authored PRs must never be queued/fired, got rounds=%#v", st.Rounds)
	}
}

func TestAutoReviewScanSkipsMarkedPRs(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:          "o/gate",
		Scope:             []string{"o"},
		Host:              "h",
		Bot:               "coderabbitai[bot]",
		ReviewCommand:     "@coderabbitai review",
		LeaderTTL:         time.Minute,
		AutoReviewMaxScan: 10,
		SkipMarker:        "<!-- crq:skip-autoreview -->",
	}
	gh := newFakeGitHub()
	gh.searchPRs = []ghapi.SearchPR{
		{Repo: "o/app", Number: 1, Author: "alice", Body: "Tiny maintenance change.\n\n<!-- crq:skip-autoreview -->"},
		{Repo: "o/app", Number: 2, Author: "alice", Body: "Review this change."},
	}
	for pr := 1; pr <= 2; pr++ {
		var pull ghapi.Pull
		pull.State = "open"
		pull.Head.SHA = "abcdef1234567890"
		gh.pulls[fakeKey("o/app", pr)] = pull
	}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.FiredMarker("o/app", 2) == "" {
		t.Fatalf("only the unmarked PR should be reviewed, got rounds=%#v", st.Rounds)
	}
	if st.Round("o/app", 1) != nil {
		t.Fatalf("marked PR must never fire, got %#v", st.Round("o/app", 1))
	}
}

func TestEnqueueBatchAppendsOncePerPR(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	ctx := context.Background()
	items := []queueCandidate{
		{Repo: "o/a", PR: 1, Head: "aaaaaaaa1"},
		{Repo: "o/b", PR: 2, Head: "bbbbbbbb2"},
		{Repo: "o/a", PR: 1, Head: "aaaaaaaa1"},
	}
	if err := svc.enqueueBatch(ctx, items); err != nil {
		t.Fatal(err)
	}
	st, _, _ := svc.store.Load(ctx)
	queued := st.QueuedRounds(time.Now().UTC())
	if len(queued) != 2 {
		t.Fatalf("expected 2 queued (deduped), got %d", len(queued))
	}
	if queued[0].Seq == queued[1].Seq || queued[0].Seq == 0 {
		t.Fatalf("expected distinct non-zero seqs, got %d and %d", queued[0].Seq, queued[1].Seq)
	}
	if err := svc.enqueueBatch(ctx, items); err != nil {
		t.Fatal(err)
	}
	st2, _, _ := svc.store.Load(ctx)
	if len(st2.QueuedRounds(time.Now().UTC())) != 2 {
		t.Fatalf("expected still 2 after re-batch, got %d", len(st2.QueuedRounds(time.Now().UTC())))
	}
}

func TestLatestCalibrationReplyToleratesBotSuffix(t *testing.T) {
	cfg := Config{Bot: "coderabbitai", GateRepo: "o/gate", CalibrationPR: 1, CalibrationMarker: "auto-generated reply by CodeRabbit"}
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	now := time.Now().UTC()
	comment := ghapi.IssueComment{Body: "0 reviews remaining. auto-generated reply by CodeRabbit", UpdatedAt: now}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/gate", 1)] = []ghapi.IssueComment{comment}

	got, ok, err := svc.latestCalibrationReply(context.Background(), 1, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Body != comment.Body {
		t.Fatalf("expected suffixed bot calibration reply to match suffix-less config, ok=%v got=%#v", ok, got)
	}
}

func TestRenewLeaderRespectsLiveLease(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, LeaderTTL: time.Minute}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	ctx := context.Background()
	if _, held, err := svc.renewLeader(ctx, "ownerA", "tokA"); err != nil || !held {
		t.Fatalf("A should acquire the lease: held=%v err=%v", held, err)
	}
	if _, held, _ := svc.renewLeader(ctx, "ownerA", "tokA"); !held {
		t.Fatal("A should renew its own lease")
	}
	if _, held, _ := svc.renewLeader(ctx, "ownerB", "tokB"); held {
		t.Fatal("B must not steal a live lease")
	}
}

func firingConfig() Config {
	return Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state-v3",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		RateLimitMarker:     "rate limited by coderabbit.ai",
		CalibrationMarker:   "auto-generated reply by CodeRabbit",
		CompletionMarker:    "Review finished",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
	}
}

func TestEnqueueIsIdempotentAndPumpFiresOnce(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	first, err := service.Enqueue(ctx, "Owner/Repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Queued || first.Seq != 1 {
		t.Fatalf("first enqueue mismatch: %#v", first)
	}
	second, err := service.Enqueue(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyQueued || second.Queued {
		t.Fatalf("second enqueue mismatch: %#v", second)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Head != "abcdef123" {
		t.Fatalf("pump mismatch: %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one posted review command, got %d", len(gh.posted))
	}
	waiting, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Action != "waiting" {
		t.Fatalf("second pump should wait on in-flight review, got %#v", waiting)
	}
}

func TestPumpPersistsPostedReviewAfterTransientStateFailure(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	inner := NewMemoryStore(cfg)
	store := &failNthUpdateStore{StateStore: inner, n: 3, err: errors.New("transient state write failure")}
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Head != "abcdef123" {
		t.Fatalf("expected fired result after retrying posted-state write, got %#v", pumped)
	}
	state, _, err := inner.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := state.Round("owner/repo", 12)
	if r == nil || r.Phase != PhaseFired || r.CommandID == 0 {
		t.Fatalf("posted review metadata was not persisted after retry: %#v", r)
	}
	if state.FiredMarker("owner/repo", 12) != "abcdef123" {
		t.Fatalf("fired marker was not persisted after retry")
	}
	if r.FiredAt == nil || r.WaitDeadline == nil {
		t.Fatalf("feedback wait should be set on the fired round: %#v", r)
	}
	if r.WaitDeadline.Sub(*r.FiredAt) != cfg.FeedbackWaitTimeout {
		t.Fatalf("feedback wait deadline should use CRQ_FEEDBACK_WAIT_TIMEOUT, got %s", r.WaitDeadline.Sub(*r.FiredAt))
	}
}

func TestPumpAdoptsExistingReviewCommandWithoutRefiring(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	gh.graphQL = noForcePush
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason != "review command already posted" {
		t.Fatalf("expected pump to adopt the existing review command, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("adopting an existing review command must not post another one, posted=%d", len(gh.posted))
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := state.Round("owner/repo", 12)
	if r == nil || r.Phase != PhaseFired || r.CommandID != comment.ID || r.Head != "abcdef123" {
		t.Fatalf("existing review command should be persisted as a fired round, got %#v", r)
	}
	if state.FiredMarker("owner/repo", 12) != "abcdef123" {
		t.Fatalf("existing review command should restore fired dedupe state")
	}
	if r.FiredAt == nil || !r.FiredAt.Equal(comment.CreatedAt) {
		t.Fatalf("adopted review command should set the fired timestamp from the comment, got %#v", r)
	}
	if r.WaitDeadline == nil || !r.WaitDeadline.Equal(comment.CreatedAt.Add(cfg.FeedbackWaitTimeout)) {
		t.Fatalf("adopted review command should set the feedback wait deadline from the comment timestamp, got %#v", r)
	}
}

func TestAdoptableCommandsRequiresExpectedHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "owner/gate", Host: "testhost", Bot: "coderabbitai[bot]", ReviewCommand: "@coderabbitai review"}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef9994567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	service := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	comments, _ := gh.ListIssueComments(ctx, "owner/repo", 12)
	reviews, _ := gh.ListReviews(ctx, "owner/repo", 12)
	cmds, _, err := service.reviewCommands(ctx, "owner/repo", 12, engine.Observation{Head: "abcdef123", Open: true}, time.Time{}, pull, comments, reviews)
	if err != nil || len(cmds) != 0 {
		t.Fatalf("must not adopt a review command after the PR head changed, cmds=%v err=%v", cmds, err)
	}
}

func TestPumpDryRunDoesNotAdoptExistingCommand(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	// Seed a queued round directly (Enqueue writes, which DryRun would allow, but
	// the point is the pump adopts nothing and posts nothing).
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "dry_run" {
		t.Fatalf("dry-run pump must simulate, not adopt an existing command, got %#v", pumped)
	}
	if roundPhase(t, store, "owner/repo", 12) != PhaseQueued {
		t.Fatalf("dry-run pump must not mutate the round, got phase %s", roundPhase(t, store, "owner/repo", 12))
	}
}

func TestPumpIgnoresStaleCommandAfterRequeue(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(10 * time.Second), UpdatedAt: headTime.Add(10 * time.Second)}
	stale.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{stale}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	// The round was requeued after a failed attempt: LastAttemptAt sits after the
	// stale command, so it must not be adopted.
	requeuedAt := stale.CreatedAt.Add(20 * time.Second)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round("owner/repo", 12)
		r.LastAttemptAt = &requeuedAt
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("a command older than the requeue must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command to be posted after requeue, posted=%v", gh.posted)
	}
	if st, _, _ := store.Load(ctx); st.Round("owner/repo", 12).CommandID == stale.ID {
		t.Fatalf("the round must track the fresh command, not the stale one")
	}
}

func TestPumpDoesNotAdoptCommandOlderThanForcePush(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	staleAt := commitTime.Add(10 * time.Minute)
	forcePushAt := commitTime.Add(30 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: staleAt, UpdatedAt: staleAt}
	stale.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{stale}
	gh.graphQL = func(_ string, _ map[string]any, out any) error {
		payload := `{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"createdAt":"` + forcePushAt.Format(time.RFC3339) + `"}]}}}}`
		return json.Unmarshal([]byte(payload), out)
	}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("a command older than the head force-push must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command for the force-pushed head, posted=%v", gh.posted)
	}
}

func TestPumpDoesNotAdoptCommandAlreadyAnsweredByReview(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command}
	answered := ghapi.Review{SubmittedAt: commandAt.Add(5 * time.Minute), CommitID: "9876543210fedcba"}
	answered.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{answered}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("an already-answered command must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command for the new head, posted=%v", gh.posted)
	}
}

func TestPumpDoesNotAdoptCommandAlreadyAnsweredByCompletionReply(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 78, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: commandAt.Add(time.Minute), UpdatedAt: commandAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command, reply}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("a completion-answered command must not be adopted, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command for the new head, posted=%v", gh.posted)
	}
}

func TestPumpAdoptsCompletionAnsweredCommandWhileTopSummaryIsProcessing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 78, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: commandAt.Add(time.Minute), UpdatedAt: commandAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	summary := ghapi.IssueComment{
		ID:        79,
		Body:      "<!-- review in progress by coderabbit.ai -->\nCurrently processing new changes in this PR. This may take a few minutes, please wait...",
		CreatedAt: commandAt.Add(-time.Hour),
		UpdatedAt: commandAt.Add(2 * time.Minute),
	}
	summary.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{summary, command, reply}
	gh.graphQL = noForcePush
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "fired" || pumped.Reason != "review command already posted" {
		t.Fatalf("the still-processing command must be adopted instead of replaced, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("processing must suppress a duplicate review trigger, posted=%v", gh.posted)
	}
}

func TestPumpDryRunDoesNotDedupeMutably(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	// The bot already reviewed the head, so DecideFire would dedupe.
	review := ghapi.Review{CommitID: "abcdef1234567890", SubmittedAt: time.Now().UTC()}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "deduped" {
		t.Fatalf("dry-run should report the dedupe it would perform, got %#v", pumped)
	}
	if roundPhase(t, store, "owner/repo", 12) != PhaseQueued {
		t.Fatalf("a dry-run dedupe must not mutate the round, got %s", roundPhase(t, store, "owner/repo", 12))
	}
}

func TestPumpSkipsAdoptionWhenCommitLookupFails(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gh.commitErrs[pull.Head.SHA] = errors.New("404 not found")
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: time.Now().UTC().Add(-30 * time.Second), UpdatedAt: time.Now().UTC().Add(-30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatalf("a failed head-commit lookup must not wedge the pump: %v", err)
	}
	if pumped.Action != "fired" || pumped.Reason == "review command already posted" {
		t.Fatalf("expected pump to skip adoption and post a fresh command, got %#v", pumped)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected a fresh review command to be posted, posted=%v", gh.posted)
	}
}

func TestPumpCompletesRoundWhenReviewSubmitted(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	review := ghapi.Review{CommitID: "abcdef1234567890", SubmittedAt: firedAt.Add(time.Minute)}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, 5)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" {
		t.Fatalf("expected the submitted review to clear the in-flight slot, got %#v", pumped)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("a submitted review must complete the round, got %s", p)
	}
}

func TestPumpCompletesRoundOnCompletionReply(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	command := ghapi.IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: firedAt, UpdatedAt: firedAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 6, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: firedAt.Add(time.Minute), UpdatedAt: firedAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command, reply}
	// A completion-only round is a re-review: a prior review must exist.
	prior := ghapi.Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour), Body: "**Actionable comments posted: 2**"}
	prior.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{prior}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, command.ID)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" {
		t.Fatalf("expected the completion reply to clear the in-flight slot, got %#v", pumped)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("a completion reply must complete the round, got %s", p)
	}
}

func TestPumpKeepsRoundReviewingOnCompletionReplyForUnreviewedPR(t *testing.T) {
	// CodeRabbit answered the first-ever review command with an instant "Review
	// finished" while the real review was still queued on its side. With no review
	// ever submitted, the round must not complete: it goes to reviewing (the slot
	// is released, but the round stays open) so the wait survives.
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	command := ghapi.IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: firedAt, UpdatedAt: firedAt}
	command.User.Login = "kristofferR"
	reply := ghapi.IssueComment{ID: 6, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished.", CreatedAt: firedAt.Add(5 * time.Second), UpdatedAt: firedAt.Add(5 * time.Second)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{command, reply}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, command.ID)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("the round must survive a completion reply on a never-reviewed PR (reviewing), got %s", p)
	}
	st, _, _ := store.Load(ctx)
	if st.WaitingHead("owner/repo", 12) != "abcdef123" {
		t.Fatalf("the feedback wait must survive a completion reply on a never-reviewed PR")
	}
}

func TestPumpKeepsRoundReviewingWhenBotOnlyReacted(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	reaction := ghapi.Reaction{}
	reaction.User.Login = cfg.Bot
	gh.reactions[5] = []ghapi.Reaction{reaction}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, 5)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" {
		t.Fatalf("expected the reaction to release the slot, got %#v", pumped)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("a bare reaction means the review is still running — the round must stay reviewing, got %s", p)
	}
}

func TestPumpKeepsRoundOpenUntilAllRequiredBotsReview(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector"}
	cfg.InflightTimeout = time.Hour
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	review := ghapi.Review{SubmittedAt: firedAt.Add(time.Minute), CommitID: "abcdef1234567890"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, firedAt, 5)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("the round must stay open while a required bot has not reviewed, got %s", p)
	}

	// A required-bot review for a different commit must not complete it.
	staleCodex := ghapi.Review{SubmittedAt: firedAt.Add(2 * time.Minute), CommitID: "0123456789abcdef"}
	staleCodex.User.Login = "chatgpt-codex-connector"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review, staleCodex}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("a required-bot review for another commit must not complete the round, got %s", p)
	}

	// The head review completes it.
	codex := ghapi.Review{SubmittedAt: firedAt.Add(3 * time.Minute), CommitID: "abcdef1234567890"}
	codex.User.Login = "chatgpt-codex-connector"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review, staleCodex, codex}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("once every required bot reviewed the head, the round must complete, got %s", p)
	}
}

func TestPumpSweepsReviewingRoundToCompletion(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	// A reviewing round whose slot was already released on a bot reaction.
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, firedAt, 5)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("the round must stay reviewing while the review is still running, got %s", p)
	}

	review := ghapi.Review{SubmittedAt: firedAt.Add(2 * time.Minute), CommitID: "abcdef1234567890"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseCompleted {
		t.Fatalf("the sweep must complete a reviewing round whose review has landed, got %s", p)
	}
}

func TestPumpDryRunDoesNotSweepReviewing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.DryRun = true
	cfg.FeedbackWaitTimeout = time.Hour
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseReviewing, time.Now().UTC().Add(-2*time.Hour), 5)

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if p := roundPhase(t, store, "owner/repo", 12); p != PhaseReviewing {
		t.Fatalf("a dry-run pump must not sweep/mutate a reviewing round, got %s", p)
	}
}

func TestPumpTreatsExistingReviewAdoptionRaceAsLostRace(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	loadState := DefaultState(cfg)
	r, _ := loadState.NewRound("owner/repo", 12, "abcdef123", headTime)
	loadState.PutRound(*r)
	service := NewService(cfg, gh, &adoptionRaceStore{cfg: cfg, loadState: loadState}, nil)

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "lost_race" {
		t.Fatalf("expected adoption race to be a benign lost_race, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("adoption race must not post another review command, posted=%d", len(gh.posted))
	}
}

func TestRecordFireResetsRecordedAcrossRetry(t *testing.T) {
	cfg := firingConfig()
	svc := NewService(cfg, newFakeGitHub(), retryNoChangeStore{cfg: cfg}, nil)
	round := Round{Repo: "owner/repo", PR: 12, Head: "abcdef123"}
	_, err := svc.recordFire(context.Background(), round, "token", 1, 0, time.Now().UTC(), time.Now().UTC())
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("expected no-change after retry lost the fire slot, got %v", err)
	}
}

func TestWaitReenqueuesAfterClearingStaleRound(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	cfg := firingConfig()
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef9994567890"
	gh.pulls["owner/repo#12"] = pull
	review := ghapi.Review{CommitID: "abcdef1234567890", SubmittedAt: now.Add(time.Second)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{review}
	store := NewMemoryStore(cfg)
	// A stale fired round for a head that has since moved (abcdef123 → abcdef999).
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseFired, now, 7)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" || result.Head != "abcdef999" {
		t.Fatalf("expected stale round to be superseded and the new head fired, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one review command for the new head, posted=%d", len(gh.posted))
	}
}

func TestWaitFiresRealReviewWhenOnlyCarriedThreadVisible(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	threadCreated := headTime.Add(-time.Hour).Format(time.RFC3339)
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if strings.Contains(query, "reviewThreads") {
			payload := `{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},` +
				`"nodes":[{"id":"THREAD1","isResolved":false,"isOutdated":false,"path":"a.go","line":1,` +
				`"comments":{"nodes":[{"databaseId":55,"body":"Carried-over finding","url":"http://x","path":"a.go","line":1,` +
				`"createdAt":"` + threadCreated + `","author":{"login":"coderabbitai[bot]"},"commit":{"oid":"fedcba9876543210"}}]}}]}}}}`
			return json.Unmarshal([]byte(payload), out)
		}
		return json.Unmarshal([]byte(`{}`), out)
	}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" {
		t.Fatalf("a carried-over thread on a freshly pushed head must fire a real review, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one review command for the new head, posted=%d", len(gh.posted))
	}
}

func TestWaitReturnsCurrentHeadFeedbackBeforeReviewSlot(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := ghapi.IssueComment{ID: 91, Body: "Actionable Codex finding on the queued head", CreatedAt: headTime.Add(time.Second), UpdatedAt: headTime.Add(time.Second)}
	comment.User.Login = "chatgpt-codex-connector[bot]"
	gh.comments[fakeKey("owner/repo", 12)] = []ghapi.IssueComment{comment}
	service := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || result.Reason != "feedback already available" {
		t.Fatalf("current-head feedback must end the slot wait immediately, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("known-bad head must not spend a review slot, posted=%d", len(gh.posted))
	}
}

func TestWaitFiresRealReviewWhenOnlyCarriedReviewPromptVisible(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: headTime.Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{stale}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" {
		t.Fatalf("a carried-over review prompt on a new head must fire a real review, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one review command for the new head, posted=%d", len(gh.posted))
	}
}

func TestWaitRepairsPoisonedCompletedRoundWithOnlyCarriedReviewPrompt(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.RequiredBots = []string{"coderabbitai[bot]"}
	cfg.FeedbackBots = []string{"coderabbitai[bot]"}
	cfg.InflightTimeout = time.Hour
	cfg.WaitTimeout = time.Second
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := ghapi.Commit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := ghapi.Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: headTime.Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []ghapi.Review{stale}
	store := NewMemoryStore(cfg)
	// A poisoned completed round at the head with no real head review.
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseCompleted, headTime, 0)
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" {
		t.Fatalf("a poisoned completed round must be repaired before firing the real review, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one replacement review command, posted=%d", len(gh.posted))
	}
}

func TestNeedsReviewToleratesBotSuffix(t *testing.T) {
	cfg := Config{Bot: "coderabbitai", GateRepo: "o/gate", Scope: []string{"o"}, ReviewDoneMarker: "summarize by coderabbit.ai"}
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 5)] = pull
	review := ghapi.Review{CommitID: "abcdef1234567890"}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []ghapi.Review{review}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	need, _, err := svc.needsReview(context.Background(), DefaultState(cfg), "o/repo", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("incremental autoreview should not re-enqueue a head already reviewed by a suffixed bot login")
	}

	gh.reviews[fakeKey("o/repo", 5)] = nil
	comment := ghapi.IssueComment{Body: "finished; summarize by coderabbit.ai"}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 5)] = []ghapi.IssueComment{comment}
	need, _, err = svc.needsReview(context.Background(), DefaultState(cfg), "o/repo", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("first-review autoreview should not re-enqueue a PR with a suffixed bot completion comment")
	}
}

func TestNeedsReviewSkipsTrackedHeadButNotNewHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Bot: "coderabbitai", Host: "h"}
	gh := newFakeGitHub()
	head := "a0646f010"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = head + "aaaaaa0"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	// A round parked awaiting retry at the current head: Pump owns re-firing it, so
	// autoreview must not re-enqueue.
	seedRound(t, store, cfg, "o/carrier", 82, head, PhaseFired, time.Now().UTC(), 1)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round("o/carrier", 82)
		if err := r.AwaitRetry(time.Now().UTC().Add(40*time.Minute), "rate limited", time.Now().UTC()); err != nil {
			return err
		}
		st.PutRound(*r)
		st.FireSlot = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.Load(ctx)

	need, _, err := svc.needsReview(ctx, st, "o/carrier", 82, true)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("needsReview must skip a head already tracked by an awaiting_retry round")
	}

	// A new head is not blocked by the prior head's parked round.
	pull.Head.SHA = "bbbbbbbbbccccc"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	need, _, err = svc.needsReview(ctx, st, "o/carrier", 82, true)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("a new head must not be blocked by a prior head's parked round")
	}
}

func TestPumpDropsClosedPRWithoutFiring(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "closed"
	pull.Merged = true
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#12"] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "skipped" || pumped.Reason != "pr closed" {
		t.Fatalf("expected a closed PR to be dropped, got %#v", pumped)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("must not post a review to a closed PR, posted %d", len(gh.posted))
	}
	if containsActiveRound(store, t, "owner/repo", 12) {
		t.Fatal("closed PR should have been removed from the queue")
	}
}

func TestPumpDropsClosedPRWhileReviewQuotaIsBlocked(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "closed"
	pull.Merged = true
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	// The round was enqueued while open; the PR is now merged with a deleted head.
	seedRound(t, store, cfg, "owner/repo", 12, "abcdef123", PhaseQueued, time.Now().UTC(), 0)
	blockedUntil := time.Now().UTC().Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Account.BlockedUntil = &blockedUntil
		st.Account.Source = "calibrate"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "skipped" || pumped.Reason != "pr closed" {
		t.Fatalf("expected merged PR cleanup to bypass the quota block, got %#v", pumped)
	}
	if containsActiveRound(store, t, "owner/repo", 12) {
		t.Fatal("merged PR should be removed even while review quota is blocked")
	}
	if len(gh.posted) != 0 {
		t.Fatalf("must not post a review to a merged PR, posted %d", len(gh.posted))
	}
}

func containsActiveRound(store StateStore, t *testing.T, repo string, pr int) bool {
	t.Helper()
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st.ContainsActive(repo, pr)
}

func TestEnqueueDedupesAlreadyReviewedHead(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#7"] = pull
	store := NewMemoryStore(cfg)
	// A completed round at the head is the dedup marker.
	seedRound(t, store, cfg, "owner/repo", 7, "abcdef123", PhaseCompleted, time.Now().UTC(), 1)
	service := NewService(cfg, gh, store, nil)
	result, err := service.Enqueue(ctx, "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deduped || result.Head != "abcdef123" {
		t.Fatalf("expected dedupe, got %#v", result)
	}
}

func TestRateLimitedRoundParksAndBlocksAccount(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Bot = "coderabbitai"
	cfg.InflightTimeout = time.Hour
	gh := newFakeGitHub()
	head := "a0646f010"
	pull := ghapi.Pull{State: "open"}
	pull.Head.SHA = head + "abcdef0"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	if _, err := svc.Enqueue(ctx, "o/carrier", 82); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "fired" {
		t.Fatalf("expected first pump to fire, got %#v", res)
	}

	// CodeRabbit answers with a rate-limit comment and an already-reviewed claim,
	// but no review object. The round must park (retry later), not complete.
	answer := time.Now().UTC().Add(time.Minute)
	rl := ghapi.IssueComment{ID: 501, Body: "<!-- rate limited by coderabbit.ai -->\n> ## Review limit reached\n> **Next review available in:** **40 minutes**", CreatedAt: answer, UpdatedAt: answer}
	rl.User.Login = "coderabbitai[bot]"
	ack := ghapi.IssueComment{ID: 502, Body: "<details><summary>✅ Action performed</summary>\n\nReview finished.\n\n> Note: CodeRabbit is an incremental review system and does not re-review already reviewed commits.</details>", CreatedAt: answer, UpdatedAt: answer}
	ack.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/carrier", 82)] = []ghapi.IssueComment{rl, ack}

	res, err = svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "requeued" || res.Reason != warnRateLimited {
		t.Fatalf("expected rate-limited requeue, got %#v", res)
	}
	st, _, _ := store.Load(ctx)
	if r := st.Round("o/carrier", 82); r == nil || r.Phase != PhaseAwaitingRetry {
		t.Fatalf("a rate-limited round must park awaiting retry, got %#v", r)
	}
	if st.FiredMarker("o/carrier", 82) != "" {
		t.Fatalf("a parked round must not be a dedup marker")
	}
	if st.Account.BlockedUntil == nil {
		t.Fatal("the real rate-limit response must block the next attempt")
	}
	if !st.ContainsActive("o/carrier", 82) {
		t.Fatal("the unreviewed PR must remain active")
	}

	if _, err := svc.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	fires := 0
	for _, p := range gh.posted {
		if strings.HasSuffix(p, ":@coderabbitai review") {
			fires++
		}
	}
	if fires != 1 {
		t.Fatalf("expected exactly one review fire, got %d (%v)", fires, gh.posted)
	}
}

func TestRotateCalibrationPersistsNewIssue(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "o/gate", CalibrationPR: 1, Scope: []string{"o"}}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	n, err := svc.rotateCalibration(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("expected a fresh issue number, got %d", n)
	}
	st, _, _ := store.Load(ctx)
	if st.CalibrationIssue != n {
		t.Fatalf("expected state.CalibrationIssue=%d, got %d", n, st.CalibrationIssue)
	}
	if svc.calibrationIssue(st) != n {
		t.Fatalf("calibrationIssue should return the rotated issue %d, got %d", n, svc.calibrationIssue(st))
	}
	if len(gh.createdIssues) != 1 {
		t.Fatalf("expected exactly one issue created, got %d", len(gh.createdIssues))
	}
}

func TestMemoryStoreConcurrentUpdatesDoNotLoseMutations(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "owner/gate", StateRef: "crq-state-v3"}
	store := NewMemoryStore(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update(ctx, func(st *State) error {
				st.NextSeq++
				return nil
			})
			if err != nil {
				t.Errorf("update failed: %v", err)
			}
		}()
	}
	wg.Wait()
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.NextSeq != 50 {
		t.Fatalf("lost updates: got %d want 50", state.NextSeq)
	}
}

func TestParseAvailableInHandlesMarkdownAndColon(t *testing.T) {
	base := time.Date(2026, 7, 11, 18, 24, 0, 0, time.UTC)
	body := "## Review limit reached\n\nYou have reached your review limit.\n\n**Next review available in:** **40 minutes**"
	got := dialect.ParseAvailableIn(body, base)
	if got == nil {
		t.Fatal("expected a parsed reset for the verbatim rate-limit body, got nil")
	}
	if want := base.Add(40 * time.Minute); !got.Equal(want) {
		t.Fatalf("expected reset %v (base+40m), got %v", want, *got)
	}
}

func TestParseAvailableInPlainFormatStillWorks(t *testing.T) {
	base := time.Date(2026, 7, 11, 18, 0, 0, 0, time.UTC)
	got := dialect.ParseAvailableIn("You are rate limited. Reviews available in 3 minutes.", base)
	if got == nil || !got.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("expected base+3m for the plain format, got %v", got)
	}
	got = dialect.ParseAvailableIn("available in 1 hour and 30 minutes", base)
	if got == nil || !got.Equal(base.Add(90*time.Minute)) {
		t.Fatalf("expected base+90m for compound duration, got %v", got)
	}
}
