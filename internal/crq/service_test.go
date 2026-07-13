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
)

type fakeGitHub struct {
	mu              sync.Mutex
	pulls           map[string]Pull
	commits         map[string]gitCommit
	commitErrs      map[string]error
	reviews         map[string][]Review
	comments        map[string][]IssueComment
	reviewComments  map[string][]ReviewComment
	issueReactions  map[string][]Reaction
	reactions       map[int64][]Reaction
	posted          []string
	deleted         []int64
	commentID       int64
	createdIssues   []int
	nextIssueNumber int
	postErrs        map[string]error
	graphQL         func(query string, vars map[string]any, out any) error
	searchPRs       []SearchPR
}

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

type retryNoChangeStore struct{}

func (retryNoChangeStore) Load(context.Context) (State, Revision, error) {
	return DefaultState(Config{}), Revision{}, nil
}

func (retryNoChangeStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	first := DefaultState(Config{})
	first.InFlight = &InFlight{Token: "token"}
	if err := mutate(&first); err != nil {
		return State{}, err
	}
	second := DefaultState(Config{})
	if err := mutate(&second); err != nil {
		if errors.Is(err, ErrNoChange) {
			return second, nil
		}
		return State{}, err
	}
	return second, nil
}

func (retryNoChangeStore) SyncDashboard(context.Context, State) error { return nil }

type adoptionRaceStore struct {
	cfg       Config
	loadState State
}

func (s *adoptionRaceStore) Load(context.Context) (State, Revision, error) {
	state := cloneState(s.loadState)
	state.Normalize(s.cfg)
	return state, Revision{}, nil
}

func (s *adoptionRaceStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	state := DefaultState(s.cfg)
	state.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "other", Phase: "posted"}
	if err := mutate(&state); err != nil {
		if errors.Is(err, ErrNoChange) {
			return state, nil
		}
		return State{}, err
	}
	return state, nil
}

func (s *adoptionRaceStore) SyncDashboard(context.Context, State) error { return nil }

type staleDedupeStore struct {
	cfg         Config
	loadState   State
	updateState State
}

func (s *staleDedupeStore) Load(context.Context) (State, Revision, error) {
	state := cloneState(s.loadState)
	state.Normalize(s.cfg)
	return state, Revision{}, nil
}

func (s *staleDedupeStore) Update(_ context.Context, mutate func(*State) error) (State, error) {
	state := cloneState(s.updateState)
	state.Normalize(s.cfg)
	if err := mutate(&state); err != nil {
		if errors.Is(err, ErrNoChange) {
			return state, nil
		}
		return State{}, err
	}
	return state, nil
}

func (s *staleDedupeStore) SyncDashboard(context.Context, State) error { return nil }

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		pulls:          map[string]Pull{},
		commits:        map[string]gitCommit{},
		commitErrs:     map[string]error{},
		reviews:        map[string][]Review{},
		comments:       map[string][]IssueComment{},
		reviewComments: map[string][]ReviewComment{},
		issueReactions: map[string][]Reaction{},
		reactions:      map[int64][]Reaction{},
	}
}

func fakeKey(repo string, pr int) string { return QueueKey(repo, pr) }

func (f *fakeGitHub) GetPull(_ context.Context, repo string, pr int) (Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pull, ok := f.pulls[fakeKey(repo, pr)]
	if !ok {
		return Pull{}, errors.New("missing pull")
	}
	return pull, nil
}

func (f *fakeGitHub) GetCommit(_ context.Context, repo, sha string) (gitCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.commitErrs[sha]; err != nil {
		return gitCommit{}, err
	}
	return f.commits[sha], nil
}

func (f *fakeGitHub) ListReviews(_ context.Context, repo string, pr int) ([]Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Review(nil), f.reviews[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListIssueComments(_ context.Context, repo string, pr int) ([]IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]IssueComment(nil), f.comments[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListReviewComments(_ context.Context, repo string, pr int) ([]ReviewComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ReviewComment(nil), f.reviewComments[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListIssueReactions(_ context.Context, repo string, pr int) ([]Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Reaction(nil), f.issueReactions[fakeKey(repo, pr)]...), nil
}

func (f *fakeGitHub) ListCommentReactions(_ context.Context, _ string, id int64) ([]Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Reaction(nil), f.reactions[id]...), nil
}

func (f *fakeGitHub) PostIssueComment(_ context.Context, repo string, pr int, body string) (IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.postErrs[fakeKey(repo, pr)]; err != nil {
		return IssueComment{}, err
	}
	f.commentID++
	f.posted = append(f.posted, repo+"#"+strconv.Itoa(pr)+":"+body)
	comment := IssueComment{ID: f.commentID, Body: body, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	comment.User.Login = "kristofferR"
	return comment, nil
}

func (f *fakeGitHub) CreateIssue(_ context.Context, _ string, _ string, _ string) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextIssueNumber == 0 {
		f.nextIssueNumber = 1000
	}
	f.nextIssueNumber++
	f.createdIssues = append(f.createdIssues, f.nextIssueNumber)
	return Issue{Number: f.nextIssueNumber, State: "open"}, nil
}

func (f *fakeGitHub) ListIssueCommentsPage(_ context.Context, repo string, pr, page, perPage int) ([]IssueComment, error) {
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
	return append([]IssueComment(nil), all[start:end]...), nil
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

func (f *fakeGitHub) SearchOpenPRs(context.Context, string, bool, int) ([]SearchPR, error) {
	return nil, nil
}

func (f *fakeGitHub) EachOpenPR(_ context.Context, _ string, _ bool, fn func(SearchPR) (bool, error)) error {
	f.mu.Lock()
	prs := append([]SearchPR(nil), f.searchPRs...)
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
	mkc := func(id int64, login, body string, at time.Time) IssueComment {
		c := IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		c.User.Login = login
		return c
	}
	key := fakeKey("o/gate", 1)
	gh.comments[key] = []IssueComment{
		mkc(1, "kristofferR", "@coderabbitai rate limit", old),                                      // old probe -> delete
		mkc(2, "coderabbitai[bot]", "0 reviews remaining. auto-generated reply by CodeRabbit", old), // old reply -> delete
		mkc(3, "someone", "unrelated human comment", old),                                           // not calibration noise -> keep
		mkc(4, "kristofferR", "@coderabbitai rate limit", now),                                      // recent -> keep
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
		LeaderTTL:         time.Minute,
		AutoReviewMaxScan: 10,
		SkipAuthors:       authorSet("dependabot[bot]"),
	}
	gh := newFakeGitHub()
	gh.searchPRs = []SearchPR{
		{Repo: "o/app", Number: 1, Author: "dependabot[bot]"},
		{Repo: "o/app", Number: 2, Author: "Dependabot"}, // case + suffix variants match too
		{Repo: "o/app", Number: 3, Author: "alice"},
	}
	// All three PRs are enqueueable if the author filter fails, so a broken
	// filter yields 3 queued items rather than a false pass via missing pulls.
	for pr := 1; pr <= 3; pr++ {
		var pull Pull
		pull.State = "open"
		pull.Head.SHA = "abcdef1234567890"
		gh.pulls[fakeKey("o/app", pr)] = pull
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	// The one-shot pass also pumps, so the enqueued PR moves queue → fired.
	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Fired) != 1 || st.Fired["o/app#3"] == "" {
		t.Fatalf("only the human-authored PR should be enqueued and fired, got fired=%#v", st.Fired)
	}
	for _, item := range st.Queue {
		if item.PR != 3 {
			t.Fatalf("bot-authored PR #%d must not be queued, got %#v", item.PR, st.Queue)
		}
	}
	for _, h := range st.History {
		if h.PR != 3 {
			t.Fatalf("bot-authored PR #%d must never fire, got history %#v", h.PR, st.History)
		}
	}
}

func TestAutoReviewScanSkipsMarkedPRs(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:          "o/gate",
		Scope:             []string{"o"},
		Host:              "h",
		LeaderTTL:         time.Minute,
		AutoReviewMaxScan: 10,
		SkipMarker:        "<!-- crq:skip-autoreview -->",
	}
	gh := newFakeGitHub()
	gh.searchPRs = []SearchPR{
		{Repo: "o/app", Number: 1, Author: "alice", Body: "Tiny maintenance change.\n\n<!-- crq:skip-autoreview -->"},
		{Repo: "o/app", Number: 2, Author: "alice", Body: "Review this change."},
	}
	for pr := 1; pr <= 2; pr++ {
		var pull Pull
		pull.State = "open"
		pull.Head.SHA = "abcdef1234567890"
		gh.pulls[fakeKey("o/app", pr)] = pull
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	if err := svc.AutoReview(ctx, AutoOptions{Once: true, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	st, _, err := svc.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Fired) != 1 || st.Fired["o/app#2"] == "" {
		t.Fatalf("only the unmarked PR should be reviewed, got fired=%#v", st.Fired)
	}
	if st.Fired["o/app#1"] != "" {
		t.Fatalf("marked PR must never fire, got fired=%#v", st.Fired)
	}
}

func TestEnqueueBatchAppendsOncePerPR(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, Host: "h"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	ctx := context.Background()
	items := []SearchPR{
		{Repo: "o/a", Number: 1},
		{Repo: "o/b", Number: 2},
		{Repo: "o/a", Number: 1}, // duplicate within the batch
	}
	if err := svc.enqueueBatch(ctx, items); err != nil {
		t.Fatal(err)
	}
	st, _, _ := svc.store.Load(ctx)
	if len(st.Queue) != 2 {
		t.Fatalf("expected 2 queued (deduped), got %d", len(st.Queue))
	}
	if st.Queue[0].Seq == st.Queue[1].Seq || st.Queue[0].Seq == 0 {
		t.Fatalf("expected distinct non-zero seqs, got %d and %d", st.Queue[0].Seq, st.Queue[1].Seq)
	}
	// Re-batching the same PRs is a no-op since they're already queued.
	if err := svc.enqueueBatch(ctx, items); err != nil {
		t.Fatal(err)
	}
	st2, _, _ := svc.store.Load(ctx)
	if len(st2.Queue) != 2 {
		t.Fatalf("expected still 2 after re-batch, got %d", len(st2.Queue))
	}
}

func TestRequeueInflightRateLimitUsesBlockedNotWarn(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, CalibrationTTL: 2 * time.Minute, Host: "h"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	// Rate-limited requeue: represented via Blocked, not a sticky Warn.
	reset := time.Now().UTC().Add(10 * time.Minute)
	st := &State{InFlight: &InFlight{Repo: "o/a", PR: 1, Seq: 5}, Fired: map[string]string{"o/a#1": "abc"}}
	svc.requeueInflight(st, inflightCheck{Requeue: true, Reason: warnRateLimited, BlockedUntil: &reset})
	if st.Warn != "" {
		t.Fatalf("rate-limit requeue must not set a sticky Warn, got %q", st.Warn)
	}
	if st.Blocked.BlockedUntil == nil || !st.Blocked.BlockedUntil.Equal(reset) {
		t.Fatalf("expected Blocked.BlockedUntil=reset, got %v", st.Blocked.BlockedUntil)
	}
	if st.InFlight != nil {
		t.Fatal("inflight should be cleared on requeue")
	}

	// Rate-limited with no parseable reset: still blocks (briefly), no Warn.
	st = &State{InFlight: &InFlight{Repo: "o/a", PR: 1}, Fired: map[string]string{}}
	svc.requeueInflight(st, inflightCheck{Requeue: true, Reason: warnRateLimited})
	if st.Warn != "" || st.Blocked.BlockedUntil == nil {
		t.Fatalf("no-reset rate limit should block without a Warn; warn=%q blocked=%v", st.Warn, st.Blocked.BlockedUntil)
	}

	// Non-rate-limit requeue keeps an informative Warn.
	st = &State{InFlight: &InFlight{Repo: "o/b", PR: 2}, Fired: map[string]string{}}
	svc.requeueInflight(st, inflightCheck{Requeue: true, Reason: "in-flight timeout"})
	if st.Warn != "in-flight timeout" {
		t.Fatalf("non-rate-limit requeue should set Warn, got %q", st.Warn)
	}
}

func TestLatestCalibrationReplyToleratesBotSuffix(t *testing.T) {
	cfg := Config{Bot: "coderabbitai", GateRepo: "o/gate", CalibrationPR: 1, CalibrationMarker: "auto-generated reply by CodeRabbit"}
	gh := newFakeGitHub()
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	now := time.Now().UTC()
	comment := IssueComment{Body: "0 reviews remaining. auto-generated reply by CodeRabbit", UpdatedAt: now}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/gate", 1)] = []IssueComment{comment}

	got, ok, err := svc.latestCalibrationReply(context.Background(), 1, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Body != comment.Body {
		t.Fatalf("expected suffixed bot calibration reply to match suffix-less config, ok=%v got=%#v", ok, got)
	}
}

func TestInflightStatusToleratesBotSuffix(t *testing.T) {
	now := time.Now().UTC()
	cfg := Config{Bot: "coderabbitai", RateLimitMarker: "rate limited by coderabbit.ai", InflightTimeout: time.Hour}
	baseState := State{InFlight: &InFlight{Repo: "o/repo", PR: 1, Phase: "posted", FiredAt: &now, FiredCommentID: 99}}

	t.Run("review", func(t *testing.T) {
		gh := newFakeGitHub()
		review := Review{SubmittedAt: now.Add(time.Second)}
		review.User.Login = "coderabbitai[bot]"
		gh.reviews[fakeKey("o/repo", 1)] = []Review{review}
		svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
		status, err := svc.inflightStatus(context.Background(), baseState)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Done || status.Reason != "review submitted" {
			t.Fatalf("expected suffixed bot review to complete in-flight review, got %#v", status)
		}
	})

	t.Run("reaction", func(t *testing.T) {
		gh := newFakeGitHub()
		reaction := Reaction{}
		reaction.User.Login = "coderabbitai[bot]"
		gh.reactions[99] = []Reaction{reaction}
		svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
		status, err := svc.inflightStatus(context.Background(), baseState)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Done || status.Reason != "bot reacted" {
			t.Fatalf("expected suffixed bot reaction to complete in-flight review, got %#v", status)
		}
	})

	t.Run("rate-limit-comment", func(t *testing.T) {
		gh := newFakeGitHub()
		comment := IssueComment{Body: "You are rate limited by coderabbit.ai. Reviews available in 3 minutes.", UpdatedAt: now.Add(time.Second)}
		comment.User.Login = "coderabbitai[bot]"
		gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
		svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
		status, err := svc.inflightStatus(context.Background(), baseState)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Requeue || status.Reason != warnRateLimited {
			t.Fatalf("expected suffixed bot rate-limit comment to requeue with blocked state, got %#v", status)
		}
	})

	t.Run("reviews-paused-note-is-not-completion", func(t *testing.T) {
		gh := newFakeGitHub()
		comment := IssueComment{Body: "> [!NOTE]\n> ## Reviews paused\n> It looks like this branch is under active development. CodeRabbit has automatically paused this review. Use `@coderabbitai resume` to resume automatic reviews.", UpdatedAt: now.Add(time.Second)}
		comment.User.Login = "coderabbitai[bot]"
		gh.comments[fakeKey("o/repo", 1)] = []IssueComment{comment}
		svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
		status, err := svc.inflightStatus(context.Background(), baseState)
		if err != nil {
			t.Fatal(err)
		}
		// The auto-pause note is not a review of the fired head — the round must keep
		// waiting for the real review rather than falsely completing with no findings.
		if status.Done || status.Requeue {
			t.Fatalf("expected reviews-paused note to leave the in-flight round pending, got %#v", status)
		}
	})
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

func TestEnqueueIsIdempotentAndPumpFiresOnce(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		RateLimitMarker:     "rate limited by coderabbit.ai",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
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
	cfg := Config{
		GateRepo:        "owner/gate",
		StateRef:        "crq-state",
		Host:            "testhost",
		Bot:             "coderabbitai[bot]",
		ReviewCommand:   "@coderabbitai review",
		RateLimitMarker: "rate limited by coderabbit.ai",
		MinInterval:     0,
		InflightTimeout: time.Minute,
		PollInterval:    time.Millisecond,
		FiredMax:        500,
	}
	gh := newFakeGitHub()
	var pull Pull
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
	if state.InFlight == nil || state.InFlight.Phase != "posted" || state.InFlight.FiredCommentID == 0 {
		t.Fatalf("posted review metadata was not persisted after retry: %#v", state.InFlight)
	}
	if state.Fired[QueueKey("owner/repo", 12)] != "abcdef123" {
		t.Fatalf("fired marker was not persisted after retry: %#v", state.Fired)
	}
	wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]
	if wait.Head != "abcdef123" {
		t.Fatalf("feedback wait marker was not persisted after firing: %#v", state.AwaitingFeedback)
	}
	if state.InFlight.FiredAt == nil || !wait.StartedAt.Equal(*state.InFlight.FiredAt) {
		t.Fatalf("feedback wait should start at the fired timestamp, wait=%#v inflight=%#v", wait, state.InFlight)
	}
	if wait.Deadline.Sub(wait.StartedAt) != cfg.FeedbackWaitTimeout {
		t.Fatalf("feedback wait deadline should use CRQ_FEEDBACK_WAIT_TIMEOUT, got %s", wait.Deadline.Sub(wait.StartedAt))
	}
}

func TestPumpAdoptsExistingReviewCommandWithoutRefiring(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		RateLimitMarker:     "rate limited by coderabbit.ai",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
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
	if state.InFlight == nil || state.InFlight.FiredCommentID != comment.ID || state.InFlight.Head != "abcdef123" {
		t.Fatalf("existing review command should be persisted as in-flight, got %#v", state.InFlight)
	}
	if state.Fired[QueueKey("owner/repo", 12)] != "abcdef123" {
		t.Fatalf("existing review command should restore fired dedupe state: %#v", state.Fired)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "abcdef123" || !wait.StartedAt.Equal(comment.CreatedAt) {
		t.Fatalf("existing review command should create a feedback wait from the comment timestamp, got %#v", wait)
	}
}

func TestExistingReviewCommandRequiresExpectedHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		FeedbackWaitTimeout: time.Minute,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef9994567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
	service := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	if _, ok, err := service.existingReviewCommand(ctx, "owner/repo", 12, "abcdef123", time.Time{}); err != nil || ok {
		t.Fatalf("must not adopt a review command after the PR head changed, ok=%v err=%v", ok, err)
	}
}

func TestPumpDryRunDoesNotAdoptExistingCommand(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
		DryRun:              true,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "dry_run" {
		t.Fatalf("dry-run pump must simulate, not adopt an existing command, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.InFlight != nil || len(state.Queue) != 1 || len(state.AwaitingFeedback) != 0 {
		t.Fatalf("dry-run pump must not mutate queue state, got inflight=%#v queue=%#v awaiting=%#v", state.InFlight, state.Queue, state.AwaitingFeedback)
	}
}

func TestPumpIgnoresStaleCommandAfterRequeue(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(10 * time.Second), UpdatedAt: headTime.Add(10 * time.Second)}
	stale.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{stale}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	requeuedAt := stale.CreatedAt.Add(20 * time.Second)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Queue[0].RequeuedAt = &requeuedAt
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
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.InFlight == nil || state.InFlight.FiredCommentID == stale.ID {
		t.Fatalf("in-flight must track the fresh command, not the stale one, got %#v", state.InFlight)
	}
}

func TestPumpDoesNotAdoptCommandOlderThanForcePush(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	// The PR was force-pushed to a commit object that predates the stale
	// command: the committer date alone would let the command be adopted.
	commitTime := time.Now().UTC().Add(-time.Hour)
	staleAt := commitTime.Add(10 * time.Minute)
	forcePushAt := commitTime.Add(30 * time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	stale := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: staleAt, UpdatedAt: staleAt}
	stale.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{stale}
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
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	// A commit created before an old review command was pushed later: the
	// commit-date cutoff cannot exclude the command, but the bot's review
	// answering it proves the command belongs to a finished round.
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{command}
	answered := Review{SubmittedAt: commandAt.Add(5 * time.Minute), CommitID: "9876543210fedcba"}
	answered.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{answered}
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
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		CalibrationMarker:   "auto-generated reply by CodeRabbit",
		CompletionMarker:    "Review finished",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	// A later regular push can point at a commit whose committer date predates
	// an old no-findings command round. The old completion reply proves that
	// command is consumed and must not be adopted for the new head.
	commitTime := time.Now().UTC().Add(-time.Hour)
	commandAt := commitTime.Add(10 * time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = commitTime
	gh.commits[pull.Head.SHA] = gc
	command := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: commandAt, UpdatedAt: commandAt}
	command.User.Login = "kristofferR"
	reply := IssueComment{ID: 78, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: commandAt.Add(time.Minute), UpdatedAt: commandAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{command, reply}
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

func TestPumpDryRunDoesNotDedupeMutably(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
		DryRun:              true,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: started, Deadline: started.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "deduped" {
		t.Fatalf("dry-run should report the dedupe it would perform, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Queue) != 1 {
		t.Fatalf("a dry-run dedupe must not remove the queued item, got %#v", state.Queue)
	}
}

func TestPumpSkipsAdoptionWhenCommitLookupFails(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gh.commitErrs[pull.Head.SHA] = errors.New("404 not found")
	comment := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: time.Now().UTC().Add(-30 * time.Second), UpdatedAt: time.Now().UTC().Add(-30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
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

func TestPumpClearsFeedbackWaitWhenReviewSubmitted(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		InflightTimeout:     time.Hour,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	review := Review{CommitID: "abcdef1234567890", SubmittedAt: firedAt.Add(time.Minute)}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "tok", Phase: "posted", ReservedAt: firedAt, FiredAt: &firedAt, FiredCommentID: 5}
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: firedAt, Deadline: firedAt.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" || pumped.Reason != doneReviewSubmitted {
		t.Fatalf("expected the submitted review to clear the in-flight slot, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AwaitingFeedback) != 0 {
		t.Fatalf("a submitted review must clear the feedback wait, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpClearsFeedbackWaitWhenCompletionReplySubmitted(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		CalibrationMarker:   "auto-generated reply by CodeRabbit",
		CompletionMarker:    "Review finished",
		InflightTimeout:     time.Hour,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	command := IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: firedAt, UpdatedAt: firedAt}
	command.User.Login = "kristofferR"
	reply := IssueComment{ID: 6, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: firedAt.Add(time.Minute), UpdatedAt: firedAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{command, reply}
	// The completion-only round is a re-review: the bot's earlier review of a
	// previous head must exist for the reply to stand in for a review.
	prior := Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: firedAt.Add(-time.Hour)}
	prior.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{prior}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "tok", Phase: "posted", ReservedAt: firedAt, FiredAt: &firedAt, FiredCommentID: command.ID}
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: firedAt, Deadline: firedAt.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" || pumped.Reason != doneBotComment {
		t.Fatalf("expected the completion reply to clear the in-flight slot, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AwaitingFeedback) != 0 {
		t.Fatalf("a completion reply must clear the feedback wait, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpKeepsFeedbackWaitWhenCompletionReplyOnUnreviewedPR(t *testing.T) {
	// Incident regression: CodeRabbit answered the first-ever review command on
	// a PR with "Review finished" five seconds after the trigger, while the
	// real review (11 findings) was still queued on its side. With no review
	// ever submitted on the PR, the ack must not complete the round: the
	// feedback wait has to survive so the loop keeps waiting for the actual
	// review instead of converging with zero findings (which also let
	// autoreview fire a duplicate command for the same head).
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		CalibrationMarker:   "auto-generated reply by CodeRabbit",
		CompletionMarker:    "Review finished",
		InflightTimeout:     time.Hour,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	command := IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: firedAt, UpdatedAt: firedAt}
	command.User.Login = "kristofferR"
	reply := IssueComment{ID: 6, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\n✅ Action performed\n\nReview finished.", CreatedAt: firedAt.Add(5 * time.Second), UpdatedAt: firedAt.Add(5 * time.Second)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{command, reply}
	// No reviews on the PR at all — the bot has never reviewed it.
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "tok", Phase: "posted", ReservedAt: firedAt, FiredAt: &firedAt, FiredCommentID: command.ID}
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: firedAt, Deadline: firedAt.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" || pumped.Reason != doneBotComment {
		t.Fatalf("the ack still ends the in-flight command round, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "abcdef123" {
		t.Fatalf("the feedback wait must survive a completion reply on a never-reviewed PR, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpKeepsFeedbackWaitWhenBotOnlyReacted(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		InflightTimeout:     time.Hour,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-time.Minute)
	reaction := Reaction{}
	reaction.User.Login = cfg.Bot
	gh.reactions[5] = []Reaction{reaction}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "tok", Phase: "posted", ReservedAt: firedAt, FiredAt: &firedAt, FiredCommentID: 5}
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: firedAt, Deadline: firedAt.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" || pumped.Reason != doneBotReacted {
		t.Fatalf("expected the reaction to clear the in-flight slot, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "abcdef123" {
		t.Fatalf("a bare reaction means the review is still running — the wait must survive, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpKeepsFeedbackWaitUntilAllRequiredBotsReview(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]", "chatgpt-codex-connector"},
		ReviewCommand:       "@coderabbitai review",
		InflightTimeout:     time.Hour,
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	firedAt := time.Now().UTC().Add(-5 * time.Minute)
	review := Review{SubmittedAt: firedAt.Add(time.Minute), CommitID: "abcdef1234567890"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "tok", Phase: "posted", ReservedAt: firedAt, FiredAt: &firedAt, FiredCommentID: 5}
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: firedAt, Deadline: firedAt.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "cleared" || pumped.Reason != doneReviewSubmitted {
		t.Fatalf("expected the submitted review to clear the in-flight slot, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "abcdef123" {
		t.Fatalf("the wait must survive while a required bot has not reviewed, got %#v", state.AwaitingFeedback)
	}

	staleCodex := Review{SubmittedAt: firedAt.Add(2 * time.Minute), CommitID: "0123456789abcdef"}
	staleCodex.User.Login = "chatgpt-codex-connector"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review, staleCodex}
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "tok2", Phase: "posted", ReservedAt: firedAt, FiredAt: &firedAt, FiredCommentID: 5}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "abcdef123" {
		t.Fatalf("a required-bot review for another commit must not complete the round, got %#v", state.AwaitingFeedback)
	}

	codex := Review{SubmittedAt: firedAt.Add(3 * time.Minute), CommitID: "abcdef1234567890"}
	codex.User.Login = "chatgpt-codex-connector"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review, staleCodex, codex}
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "tok3", Phase: "posted", ReservedAt: firedAt, FiredAt: &firedAt, FiredCommentID: 5}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AwaitingFeedback) != 0 {
		t.Fatalf("once every required bot reviewed the head, the wait must be cleared, got %#v", state.AwaitingFeedback)
	}
}

func TestBotsReviewedHeadCountsPreFireReviews(t *testing.T) {
	firedAt := time.Now().UTC()
	// Codex reviewed the head before the CodeRabbit round was triggered; the
	// round must still count it, exactly as Feedback's ReviewedBy would.
	early := Review{SubmittedAt: firedAt.Add(-10 * time.Minute), CommitID: "abcdef1234567890"}
	early.User.Login = "chatgpt-codex-connector"
	late := Review{SubmittedAt: firedAt.Add(time.Minute), CommitID: "abcdef1234567890"}
	late.User.Login = "coderabbitai[bot]"
	bots := botSet([]string{"coderabbitai[bot]", "chatgpt-codex-connector"})
	if !botsReviewedHead([]Review{early, late}, bots, "abcdef123", firedAt) {
		t.Fatal("a required bot's pre-fire review of the same head must complete the round")
	}
	other := Review{SubmittedAt: firedAt.Add(time.Minute), CommitID: "0123456789abcdef"}
	other.User.Login = "chatgpt-codex-connector"
	if botsReviewedHead([]Review{other, late}, bots, "abcdef123", firedAt) {
		t.Fatal("a review of a different commit must not complete the round")
	}
}

func TestPumpSweepsWaitAfterReactedRoundCompletes(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	startedAt := time.Now().UTC().Add(-5 * time.Minute)
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		// The round's in-flight slot was already released on a bot reaction;
		// only the wait remains.
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: startedAt, Deadline: startedAt.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// No review submitted yet: the wait must survive the sweep.
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "abcdef123" {
		t.Fatalf("the wait must survive while the review is still running, got %#v", state.AwaitingFeedback)
	}

	// Once the review lands for the fired head, the next pump sweeps the wait.
	review := Review{SubmittedAt: startedAt.Add(2 * time.Minute), CommitID: "abcdef1234567890"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review}
	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AwaitingFeedback) != 0 {
		t.Fatalf("the sweep must clear a wait whose review has been submitted, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpSweepsWaitAfterCompletionOnlyRoundCompletes(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		CalibrationMarker:   "auto-generated reply by CodeRabbit",
		CompletionMarker:    "Review finished",
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	startedAt := time.Now().UTC().Add(-5 * time.Minute)
	command := IssueComment{ID: 5, Body: cfg.ReviewCommand, CreatedAt: startedAt, UpdatedAt: startedAt}
	command.User.Login = "kristofferR"
	reply := IssueComment{ID: 6, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nReview finished.", CreatedAt: startedAt.Add(time.Minute), UpdatedAt: startedAt.Add(time.Minute)}
	reply.User.Login = cfg.Bot
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{command, reply}
	// The completion-only round is a re-review: the bot's earlier review of a
	// previous head must exist for the sweep to trust the reply.
	prior := Review{ID: 9, CommitID: "0123456fedcba", State: "COMMENTED", SubmittedAt: startedAt.Add(-time.Hour)}
	prior.User.Login = cfg.Bot
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{prior}
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: startedAt, Deadline: startedAt.Add(cfg.FeedbackWaitTimeout)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "idle" {
		t.Fatalf("expected idle pump after sweeping the completed wait, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AwaitingFeedback) != 0 {
		t.Fatalf("the sweep must clear a completion-backed wait, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpDryRunDoesNotPruneWaits(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
		DryRun:              true,
	}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	expired := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: expired, Deadline: expired.Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; wait.Head != "abcdef123" {
		t.Fatalf("a dry-run pump must not prune persisted waits, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpPrunesExpiredFeedbackWaits(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		FeedbackWaitTimeout: time.Hour,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)
	expired := time.Now().UTC().Add(-2 * time.Hour)
	live := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{Repo: "owner/repo", PR: 12, Head: "abcdef123", StartedAt: expired, Deadline: expired.Add(time.Hour)}
		st.AwaitingFeedback[QueueKey("owner/repo", 13)] = FeedbackWait{Repo: "owner/repo", PR: 13, Head: "fedcba321", StartedAt: live, Deadline: live.Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pumped, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pumped.Action != "idle" {
		t.Fatalf("expected idle pump, got %#v", pumped)
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.AwaitingFeedback[QueueKey("owner/repo", 12)]; ok {
		t.Fatalf("expired feedback wait should have been pruned, got %#v", state.AwaitingFeedback)
	}
	if wait := state.AwaitingFeedback[QueueKey("owner/repo", 13)]; wait.Head != "fedcba321" {
		t.Fatalf("live feedback wait must survive pruning, got %#v", state.AwaitingFeedback)
	}
}

func TestPumpTreatsExistingReviewAdoptionRaceAsLostRace(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Minute,
		PollInterval:        time.Millisecond,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	comment := IssueComment{ID: 77, Body: cfg.ReviewCommand, CreatedAt: headTime.Add(30 * time.Second), UpdatedAt: headTime.Add(30 * time.Second)}
	comment.User.Login = "kristofferR"
	gh.comments[fakeKey("owner/repo", 12)] = []IssueComment{comment}
	state := DefaultState(cfg)
	state.NextSeq = 1
	state.Queue = []QueueItem{{Seq: 1, Owner: "owner", Repo: "owner/repo", PR: 12, Host: "testhost", EnqueuedAt: headTime}}
	service := NewService(cfg, gh, &adoptionRaceStore{cfg: cfg, loadState: state}, nil)

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

func TestMarkReviewPostedResetsRecordedAcrossRetry(t *testing.T) {
	svc := NewService(Config{}, newFakeGitHub(), retryNoChangeStore{}, nil)
	_, err := svc.markReviewPosted(context.Background(), "token", QueueItem{Repo: "owner/repo", PR: 12}, "abcdef123", 1, time.Now().UTC())
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("expected no-change after retry lost the in-flight token, got %v", err)
	}
}

func TestWaitReenqueuesAfterClearingStaleInflight(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	cfg := Config{
		GateRepo:        "owner/gate",
		StateRef:        "crq-state",
		Host:            "testhost",
		Bot:             "coderabbitai[bot]",
		ReviewCommand:   "@coderabbitai review",
		RateLimitMarker: "rate limited by coderabbit.ai",
		MinInterval:     0,
		InflightTimeout: time.Hour,
		PollInterval:    time.Millisecond,
		WaitTimeout:     time.Second,
		FiredMax:        500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef9994567890"
	gh.pulls["owner/repo#12"] = pull
	review := Review{CommitID: "abcdef1234567890", SubmittedAt: now.Add(time.Second)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{review}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.InFlight = &InFlight{Repo: "owner/repo", PR: 12, Head: "abcdef123", Token: "old-token", Phase: "posted", FiredAt: &now, FiredCommentID: 7}
		st.Fired[QueueKey("owner/repo", 12)] = "abcdef123"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" || result.Head != "abcdef999" {
		t.Fatalf("expected stale in-flight clear to re-enqueue and fire new head, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one review command for the new head, posted=%d", len(gh.posted))
	}
}

func TestWaitFiresRealReviewWhenOnlyCarriedThreadVisible(t *testing.T) {
	// Regression: a freshly pushed head whose only visible feedback is a
	// carried-over unresolved inline thread from an EARLIER commit must trigger a
	// real review of the new head, not short-circuit the review-slot wait. Before
	// the fix, any finding (including such a stale thread) ended the wait with
	// "feedback already available", so the new head was never actually re-reviewed.
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Hour,
		PollInterval:        time.Millisecond,
		WaitTimeout:         time.Second,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	// No bot review of the new head; only an unresolved CodeRabbit thread whose
	// comment was filed on an earlier commit. Feedback surfaces it across commits.
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

func TestWaitFiresRealReviewWhenOnlyCarriedReviewPromptVisible(t *testing.T) {
	// Review-body prompts intentionally remain visible after a head change until a
	// newer review supersedes them. They must not be mistaken for feedback on the
	// new head or the replacement review will never be requested.
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Hour,
		PollInterval:        time.Millisecond,
		WaitTimeout:         time.Second,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: headTime.Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{stale}
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

func TestWaitRepairsPoisonedFiredMarkerWithOnlyCarriedReviewPrompt(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		RequiredBots:        []string{"coderabbitai[bot]"},
		FeedbackBots:        []string{"coderabbitai[bot]"},
		ReviewCommand:       "@coderabbitai review",
		MinInterval:         0,
		InflightTimeout:     time.Hour,
		PollInterval:        time.Millisecond,
		WaitTimeout:         time.Second,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	headTime := time.Now().UTC().Add(-time.Minute)
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	gc := gitCommit{SHA: pull.Head.SHA}
	gc.Committer.Date = headTime
	gh.commits[pull.Head.SHA] = gc
	stale := Review{
		ID:          7,
		Body:        "<details><summary>Prompt for AI agents</summary>\n\n```\nIn `@a.go`:\n- Around line 1: Carried-over finding.\n```\n</details>",
		CommitID:    "fedcba9876543210",
		SubmittedAt: headTime.Add(-time.Hour),
	}
	stale.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("owner/repo", 12)] = []Review{stale}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Fired[QueueKey("owner/repo", 12)] = "abcdef123"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, gh, store, nil)

	result, code, err := service.Wait(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Action != "fired" {
		t.Fatalf("a poisoned fired marker must be repaired before firing the real review, code=%d result=%#v", code, result)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("expected one replacement review command, posted=%d", len(gh.posted))
	}
}

func TestBotReviewedHeadToleratesBotSuffix(t *testing.T) {
	// CRQ_BOT configured suffix-less, but REST reviews come back as coderabbitai[bot].
	cfg := Config{Bot: "coderabbitai", GateRepo: "o/gate", Scope: []string{"o"}}
	gh := newFakeGitHub()
	review := Review{CommitID: "abcdef1234567890"}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []Review{review}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	ok, err := svc.botReviewedHead(context.Background(), "o/repo", 5, "abcdef123")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a suffix-less CRQ_BOT must still dedupe against a coderabbitai[bot] REST review of the head")
	}
}

func TestNeedsReviewToleratesBotSuffix(t *testing.T) {
	// CRQ_BOT configured suffix-less, but REST reviews/comments come back as coderabbitai[bot].
	cfg := Config{Bot: "coderabbitai", GateRepo: "o/gate", Scope: []string{"o"}, ReviewDoneMarker: "summarize by coderabbit.ai"}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("o/repo", 5)] = pull
	review := Review{CommitID: "abcdef1234567890"}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 5)] = []Review{review}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	need, err := svc.needsReview(context.Background(), State{}, "o/repo", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("incremental autoreview should not re-enqueue a head already reviewed by a suffixed bot login")
	}

	gh.reviews[fakeKey("o/repo", 5)] = nil
	comment := IssueComment{Body: "finished; summarize by coderabbit.ai"}
	comment.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 5)] = []IssueComment{comment}
	need, err = svc.needsReview(context.Background(), State{}, "o/repo", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("first-review autoreview should not re-enqueue a PR with a suffixed bot completion comment")
	}
}

func TestPumpDropsClosedPRWithoutFiring(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:        "owner/gate",
		StateRef:        "crq-state",
		Host:            "testhost",
		Bot:             "coderabbitai[bot]",
		ReviewCommand:   "@coderabbitai review",
		RateLimitMarker: "rate limited by coderabbit.ai",
		PollInterval:    time.Millisecond,
		InflightTimeout: time.Minute,
		FiredMax:        500,
	}
	gh := newFakeGitHub()
	// PR queued while open, then closed/merged before it reached the front.
	var pull Pull
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
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Contains("owner/repo", 12) {
		t.Fatal("closed PR should have been removed from the queue")
	}
}

func TestPumpDropsClosedPRWhileReviewQuotaIsBlocked(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:        "owner/gate",
		StateRef:        "crq-state",
		Host:            "testhost",
		Bot:             "coderabbitai[bot]",
		ReviewCommand:   "@coderabbitai review",
		PollInterval:    time.Millisecond,
		InflightTimeout: time.Minute,
		FiredMax:        500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "closed"
	pull.Merged = true
	// A merged PR with a deleted head must still be removable; no head SHA is
	// needed once GitHub says the PR is terminal.
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	service := NewService(cfg, gh, store, nil)

	if _, err := service.Enqueue(ctx, "owner/repo", 12); err != nil {
		t.Fatal(err)
	}
	blockedUntil := time.Now().UTC().Add(time.Hour)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Blocked.BlockedUntil = &blockedUntil
		st.Blocked.Source = "calibrate"
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
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Contains("owner/repo", 12) {
		t.Fatal("merged PR should be removed even while review quota is blocked")
	}
	if len(gh.posted) != 0 {
		t.Fatalf("must not post a review to a merged PR, posted %d", len(gh.posted))
	}
}

func TestPumpDedupesQueuedHeadAwaitingFeedback(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	store := NewMemoryStore(cfg)
	started := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.NextSeq = 1
		st.Queue = []QueueItem{{Seq: 1, Owner: "owner", Repo: "owner/repo", PR: 12, Host: "testhost", EnqueuedAt: started}}
		st.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{
			Repo:      "owner/repo",
			PR:        12,
			Head:      "abcdef123",
			StartedAt: started,
			Deadline:  started.Add(cfg.FeedbackWaitTimeout),
		}
		delete(st.Fired, QueueKey("owner/repo", 12)) // tolerate older/corrupt state missing the fired marker
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, gh, store, nil)

	result, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "deduped" || result.Head != "abcdef123" {
		t.Fatalf("expected pump to dedupe the queued awaiting head, got %#v", result)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("pump must not post another review for an awaiting head, posted=%d", len(gh.posted))
	}
	state, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Queue) != 0 {
		t.Fatalf("deduped awaiting item should be removed from the queue, got %#v", state.Queue)
	}
	if state.Fired[QueueKey("owner/repo", 12)] != "abcdef123" {
		t.Fatalf("pump should restore the fired marker from awaiting feedback: %#v", state.Fired)
	}
}

func TestPumpDedupeRevalidatesCurrentWaitMarker(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo:            "owner/gate",
		StateRef:            "crq-state",
		Host:                "testhost",
		Bot:                 "coderabbitai[bot]",
		ReviewCommand:       "@coderabbitai review",
		PollInterval:        time.Millisecond,
		InflightTimeout:     time.Minute,
		FeedbackWaitTimeout: time.Minute,
		FiredMax:            500,
	}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls[fakeKey("owner/repo", 12)] = pull
	started := time.Now().UTC()
	loadState := DefaultState(cfg)
	loadState.NextSeq = 1
	loadState.Queue = []QueueItem{{Seq: 1, Owner: "owner", Repo: "owner/repo", PR: 12, Host: "testhost", EnqueuedAt: started}}
	loadState.AwaitingFeedback[QueueKey("owner/repo", 12)] = FeedbackWait{
		Repo:      "owner/repo",
		PR:        12,
		Head:      "abcdef123",
		StartedAt: started,
		Deadline:  started.Add(cfg.FeedbackWaitTimeout),
	}
	updateState := DefaultState(cfg)
	updateState.NextSeq = 1
	updateState.Queue = append([]QueueItem(nil), loadState.Queue...)
	service := NewService(cfg, gh, &staleDedupeStore{cfg: cfg, loadState: loadState, updateState: updateState}, nil)

	result, err := service.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "lost_race" {
		t.Fatalf("stale wait marker must not report a successful dedupe, got %#v", result)
	}
	if len(gh.posted) != 0 {
		t.Fatalf("stale dedupe race must not post another review command, posted=%d", len(gh.posted))
	}
}

func TestEnqueueDedupesAlreadyFiredHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "owner/gate", StateRef: "crq-state", Host: "testhost", FiredMax: 500}
	gh := newFakeGitHub()
	var pull Pull
	pull.State = "open"
	pull.Head.SHA = "abcdef1234567890"
	gh.pulls["owner/repo#7"] = pull
	store := NewMemoryStore(cfg)
	_, err := store.Update(ctx, func(st *State) error {
		st.Fired[QueueKey("owner/repo", 7)] = "abcdef123"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, gh, store, nil)
	result, err := service.Enqueue(ctx, "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deduped || result.Head != "abcdef123" {
		t.Fatalf("expected dedupe, got %#v", result)
	}
}

func TestMemoryStoreConcurrentUpdatesDoNotLoseMutations(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "owner/gate", StateRef: "crq-state", FiredMax: 500}
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

// --- Runaway re-fire loop regression tests (carrier#82) ---

func TestParseAvailableInHandlesMarkdownAndColon(t *testing.T) {
	base := time.Date(2026, 7, 11, 18, 24, 0, 0, time.UTC)
	// Verbatim of CodeRabbit's current rate-limit body: a colon and bold markers
	// sit between "available in" and the duration. The old parser required a
	// literal "available in " (trailing space) and choked on "**40", yielding no
	// reset — which dropped the block to a ~2m fallback and drove the runaway.
	body := "## Review limit reached\n\nYou have reached your review limit.\n\n**Next review available in:** **40 minutes**"
	got := parseAvailableIn(body, base)
	if got == nil {
		t.Fatal("expected a parsed reset for the verbatim rate-limit body, got nil")
	}
	if want := base.Add(40 * time.Minute); !got.Equal(want) {
		t.Fatalf("expected reset %v (base+40m), got %v", want, *got)
	}
}

func TestParseAvailableInPlainFormatStillWorks(t *testing.T) {
	base := time.Date(2026, 7, 11, 18, 0, 0, 0, time.UTC)
	got := parseAvailableIn("You are rate limited. Reviews available in 3 minutes.", base)
	if got == nil || !got.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("expected base+3m for the plain format, got %v", got)
	}
	got = parseAvailableIn("available in 1 hour and 30 minutes", base)
	if got == nil || !got.Equal(base.Add(90*time.Minute)) {
		t.Fatalf("expected base+90m for compound duration, got %v", got)
	}
}

func TestInflightStatusAlreadyReviewedAckWithoutReviewDoesNotOverrideRateLimit(t *testing.T) {
	now := time.Now().UTC()
	cfg := Config{Bot: "coderabbitai", RateLimitMarker: "rate limited by coderabbit.ai", InflightTimeout: time.Hour}
	gh := newFakeGitHub()
	// Incident shape from ha-adjustable-bed#435: CodeRabbit rate-limited the first
	// attempt, then answered the explicit command with misleading already-reviewed
	// boilerplate even though GitHub had no review for the PR.
	rl := IssueComment{ID: 1, Body: "<!-- rate limited by coderabbit.ai -->\n> ## Review limit reached\n> **Next review available in:** **40 minutes**", UpdatedAt: now.Add(time.Second)}
	rl.User.Login = "coderabbitai[bot]"
	ack := IssueComment{ID: 2, Body: "<details>\n<summary>✅ Action performed</summary>\n\nReview finished.\n\n> Note: CodeRabbit is an incremental review system and does not re-review already reviewed commits. This command is applicable only when automatic reviews are paused.\n</details>", UpdatedAt: now.Add(2 * time.Second)}
	ack.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/repo", 82)] = []IssueComment{rl, ack}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)
	// Guard the premises: the two real CodeRabbit comments are cleanly separable —
	// the rate-limit notice carries the RL marker but not the terminal note, and
	// the ack the reverse.
	if !svc.isRateLimited(rl.Body) || svc.isReviewAlreadyDone(rl.Body) {
		t.Fatal("rate-limit comment must classify as rate-limited, not terminal")
	}
	if svc.isRateLimited(ack.Body) || !svc.isReviewAlreadyDone(ack.Body) {
		t.Fatal("ack must classify as terminal already-reviewed, not rate-limited")
	}
	state := State{InFlight: &InFlight{Repo: "o/repo", PR: 82, Head: "a0646f010", Phase: "posted", FiredAt: &now, FiredCommentID: 99}}
	status, err := svc.inflightStatus(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Requeue || status.Reason != warnRateLimited {
		t.Fatalf("an unproven acknowledgement must yield to the rate limit, got %#v", status)
	}
	if status.Done {
		t.Fatal("an acknowledgement with no matching GitHub review must not complete the round")
	}

	// Once GitHub actually has a review for the claimed head, the acknowledgement
	// is trustworthy and wins over the stale rate-limit comment.
	review := Review{CommitID: "a0646f010abcdef", SubmittedAt: now.Add(-time.Minute)}
	review.User.Login = "coderabbitai[bot]"
	gh.reviews[fakeKey("o/repo", 82)] = []Review{review}
	status, err = svc.inflightStatus(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Done || status.Requeue || status.Reason != doneAlreadyReviewed {
		t.Fatalf("a matching GitHub review should validate the acknowledgement, got %#v", status)
	}
}

func TestRateLimitedAlreadyReviewedAckWithoutReviewRequeues(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		GateRepo: "o/gate", Scope: []string{"o"}, Host: "h",
		Bot: "coderabbitai", RateLimitMarker: "rate limited by coderabbit.ai",
		ReviewCommand: "@coderabbitai review", InflightTimeout: time.Hour,
	}
	gh := newFakeGitHub()
	head := "a0646f010"
	pull := Pull{State: "open"}
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
	// but no review object. The PR must remain eligible for a real later attempt.
	answer := time.Now().UTC().Add(time.Minute)
	rl := IssueComment{ID: 501, Body: "<!-- rate limited by coderabbit.ai -->\n> ## Review limit reached\n> **Next review available in:** **40 minutes**", CreatedAt: answer, UpdatedAt: answer}
	rl.User.Login = "coderabbitai[bot]"
	ack := IssueComment{ID: 502, Body: "<details><summary>✅ Action performed</summary>\n\nReview finished.\n\n> Note: CodeRabbit is an incremental review system and does not re-review already reviewed commits.</details>", CreatedAt: answer, UpdatedAt: answer}
	ack.User.Login = "coderabbitai[bot]"
	gh.comments[fakeKey("o/carrier", 82)] = []IssueComment{rl, ack}

	res, err = svc.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "requeued" || res.Reason != warnRateLimited {
		t.Fatalf("expected rate-limited requeue, got %#v", res)
	}
	st, _, _ := store.Load(ctx)
	if st.Fired[QueueKey("o/carrier", 82)] != "" {
		t.Fatalf("a false completion must not retain a fired marker, got %#v", st.Fired)
	}
	if st.Blocked.BlockedUntil == nil {
		t.Fatal("the real rate-limit response must block the next attempt")
	}
	if !st.Contains("o/carrier", 82) {
		t.Fatal("the unreviewed PR must remain queued")
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

func TestNeedsReviewSkipsCooledDownHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Bot: "coderabbitai", Host: "h"}
	gh := newFakeGitHub()
	head := "a0646f010"
	pull := Pull{State: "open"}
	pull.Head.SHA = head + "aaaaaa0"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	st := DefaultState(cfg)
	st.Cooldown[QueueKey("o/carrier", 82)] = FireCooldown{Head: head, Until: time.Now().UTC().Add(40 * time.Minute)}

	need, err := svc.needsReview(ctx, st, "o/carrier", 82, true)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("needsReview must skip a head under an active fire cooldown")
	}

	// A new head is not blocked by the prior head's cooldown.
	pull.Head.SHA = "bbbbbbbbbccccc"
	gh.pulls[fakeKey("o/carrier", 82)] = pull
	need, err = svc.needsReview(ctx, st, "o/carrier", 82, true)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("a new head must not be blocked by a prior head's cooldown")
	}
}

func TestRequeueRateLimitSetsCooldownAndDoesNotExtendSameComment(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, RateLimitFallback: 15 * time.Minute, Host: "h"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	reset := time.Now().UTC().Add(40 * time.Minute)
	st := &State{
		InFlight: &InFlight{Repo: "o/a", PR: 1, Head: "abc123def", Seq: 5},
		Fired:    map[string]string{"o/a#1": "abc123def"},
		Cooldown: map[string]FireCooldown{},
	}
	svc.requeueInflight(st, inflightCheck{Requeue: true, Reason: warnRateLimited, BlockedUntil: &reset, RLCommentID: 77, RLCommentUpdated: time.Now().UTC()})

	cd, ok := st.Cooldown["o/a#1"]
	if !ok || cd.Head != "abc123def" || !cd.Until.Equal(reset) {
		t.Fatalf("expected cooldown until reset for the fired head, got %#v (ok=%v)", cd, ok)
	}
	if st.Blocked.RLCommentID != 77 {
		t.Fatalf("expected rate-limit comment id tracked, got %d", st.Blocked.RLCommentID)
	}
	firstUntil := *st.Blocked.BlockedUntil

	// Re-observe the SAME edited rate-limit comment with an advanced UpdatedAt and a
	// later parsed window: the standing block must not be pushed out again.
	st.InFlight = &InFlight{Repo: "o/a", PR: 1, Head: "abc123def", Seq: 6}
	st.Fired = map[string]string{"o/a#1": "abc123def"}
	later := reset.Add(30 * time.Minute)
	svc.requeueInflight(st, inflightCheck{Requeue: true, Reason: warnRateLimited, BlockedUntil: &later, RLCommentID: 77, RLCommentUpdated: time.Now().UTC()})
	if !st.Blocked.BlockedUntil.Equal(firstUntil) {
		t.Fatalf("re-observed same rate-limit comment must reuse standing block %v, got %v", firstUntil, *st.Blocked.BlockedUntil)
	}
}

func TestRequeueRateLimitNoResetUsesFallbackNotShortTTL(t *testing.T) {
	cfg := Config{GateRepo: "o/gate", Scope: []string{"o"}, CalibrationTTL: 2 * time.Minute, RateLimitFallback: 15 * time.Minute, Host: "h"}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	now := time.Now().UTC()
	st := &State{InFlight: &InFlight{Repo: "o/a", PR: 1, Head: "abc123def"}, Fired: map[string]string{}, Cooldown: map[string]FireCooldown{}}
	svc.requeueInflight(st, inflightCheck{Requeue: true, Reason: warnRateLimited})
	if st.Blocked.BlockedUntil == nil {
		t.Fatal("expected a block window on an unparseable rate limit")
	}
	if got := st.Blocked.BlockedUntil.Sub(now); got < 10*time.Minute {
		t.Fatalf("unparseable rate limit should fall back to the conservative window, got %v", got)
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

func TestNormalizePrunesExpiredCooldowns(t *testing.T) {
	cfg := Config{Scope: []string{"o"}}
	st := DefaultState(cfg)
	now := time.Now().UTC()
	st.Cooldown["o/a#1"] = FireCooldown{Head: "aaa", Until: now.Add(30 * time.Minute)} // live
	st.Cooldown["o/b#2"] = FireCooldown{Head: "bbb", Until: now.Add(-time.Minute)}     // expired
	st.Cooldown["o/c#3"] = FireCooldown{Head: "", Until: now.Add(time.Hour)}           // headless
	st.Normalize(cfg)
	if _, ok := st.Cooldown["o/a#1"]; !ok {
		t.Fatal("live cooldown must survive Normalize")
	}
	if _, ok := st.Cooldown["o/b#2"]; ok {
		t.Fatal("expired cooldown must be pruned")
	}
	if _, ok := st.Cooldown["o/c#3"]; ok {
		t.Fatal("headless cooldown must be pruned")
	}
}
