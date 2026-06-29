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

func (f *fakeGitHub) SearchOpenPRs(context.Context, string, bool, int) ([]SearchPR, error) {
	return nil, nil
}

func (f *fakeGitHub) GraphQL(context.Context, string, map[string]any, any) error {
	return errors.New("graphql unavailable")
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

func TestEnqueueDedupesAlreadyFiredHead(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "owner/gate", StateRef: "crq-state", Host: "testhost", FiredMax: 500}
	gh := newFakeGitHub()
	var pull Pull
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
