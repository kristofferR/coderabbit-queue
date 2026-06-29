package crq

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

type fakeGitHub struct {
	mu        sync.Mutex
	pulls     map[string]Pull
	reviews   map[string][]Review
	comments  map[string][]IssueComment
	posted    []string
	deleted   []int64
	commentID int64
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		pulls:    map[string]Pull{},
		reviews:  map[string][]Review{},
		comments: map[string][]IssueComment{},
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

func (f *fakeGitHub) ListReviewComments(context.Context, string, int) ([]ReviewComment, error) {
	return nil, nil
}

func (f *fakeGitHub) ListCommentReactions(context.Context, string, int64) ([]Reaction, error) {
	return nil, nil
}

func (f *fakeGitHub) PostIssueComment(_ context.Context, repo string, pr int, body string) (IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentID++
	f.posted = append(f.posted, repo+"#"+strconv.Itoa(pr)+":"+body)
	comment := IssueComment{ID: f.commentID, Body: body, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	comment.User.Login = "kristofferR"
	return comment, nil
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

func (f *fakeGitHub) EachOpenPR(context.Context, string, bool, func(SearchPR) (bool, error)) error {
	return nil
}

func (f *fakeGitHub) GraphQL(context.Context, string, map[string]any, any) error {
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

	deleted := svc.pruneCalibration(context.Background(), now.Add(-2*time.Minute), 80)
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
