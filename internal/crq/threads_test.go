package crq

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The point of the command: after a push every thread from the previous head is
// outdated, findings leave those out by design, and an agent then had no way to
// close them through crq at all. They must be listed — and listed last, since
// the threads still pointing at live code matter more.
func TestOpenThreadsIncludesOutdatedAndOrdersThem(t *testing.T) {
	gh := newFakeGitHub()
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if !strings.Contains(query, "reviewThreads") {
			return nil
		}
		return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"reviewThreads":{
		  "pageInfo":{"hasNextPage":false,"endCursor":""},
		  "nodes":[
		    {"id":"T_outdated","isResolved":false,"isOutdated":true,"path":"a.go","line":1,
		     "comments":{"totalCount":1,"nodes":[{"body":"**Stale point**","author":{"login":"coderabbitai[bot]"}}]}},
		    {"id":"T_resolved","isResolved":true,"isOutdated":false,"path":"b.go","line":2,
		     "comments":{"totalCount":1,"nodes":[{"body":"**Done**","author":{"login":"coderabbitai[bot]"}}]}},
		    {"id":"T_current","isResolved":false,"isOutdated":false,"path":"c.go","line":3,
		     "comments":{"totalCount":1,"nodes":[{"body":"**Live point**","author":{"login":"coderabbitai[bot]"}}]}}
		  ]}}}}`), out)
	}
	svc := NewService(firingConfig(), gh, NewMemoryStore(firingConfig()), nil)

	threads, err := svc.OpenThreads(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 {
		t.Fatalf("got %d threads, want the two unresolved ones", len(threads))
	}
	if threads[0].ID != "T_current" || threads[0].Outdated {
		t.Errorf("first = %+v, want the current thread", threads[0])
	}
	if threads[1].ID != "T_outdated" || !threads[1].Outdated {
		t.Errorf("second = %+v, want the outdated thread — omitting it is the bug", threads[1])
	}
	if threads[0].Title != "Live point" || threads[0].Bot != "coderabbitai" {
		t.Errorf("thread = %+v, want a readable title and the bot named", threads[0])
	}
	// A human's thread is listed too — the command promises every unresolved one
	// — but calling a person a bot makes the output plainly wrong.
	if threads[0].Author != "" {
		t.Errorf("thread = %+v, want no author on a bot's thread", threads[0])
	}
}

// A login in CRQ_FEEDBACK_BOTS is a bot to `crq feedback`, which surfaces its
// findings. Listing the same thread as a human's would disagree with the command
// an agent reads next, and would parse the title with the human heuristic.
func TestOpenThreadsNamesAConfiguredFeedbackBot(t *testing.T) {
	gh := newFakeGitHub()
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if !strings.Contains(query, "reviewThreads") {
			return nil
		}
		return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"reviewThreads":{
		  "pageInfo":{"hasNextPage":false,"endCursor":""},
		  "nodes":[{"id":"T_custom","isResolved":false,"isOutdated":false,"path":"a.go","line":1,
		    "comments":{"totalCount":1,"nodes":[{"body":"### Nil deref\n\n**High Severity**\n\nDetail.",
		      "author":{"login":"reviewdog[bot]"}}]}}]
		}}}}`), out)
	}
	cfg := firingConfig()
	cfg.FeedbackBots = append(cfg.FeedbackBots, "reviewdog[bot]")
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	threads, err := svc.OpenThreads(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want the custom reviewer's", len(threads))
	}
	if threads[0].Bot != "reviewdog" || threads[0].Author != "" {
		t.Errorf("thread = %+v, want the configured feedback bot named as a bot", threads[0])
	}
	// Read as a bot's: the heading is the summary and the bold span is severity.
	if threads[0].Title != "Nil deref" {
		t.Errorf("title = %q, want the bot's heading rather than its severity", threads[0].Title)
	}
}

func TestOpenThreadsUsesFleetConfiguredFeedbackBots(t *testing.T) {
	gh := newFakeGitHub()
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if !strings.Contains(query, "reviewThreads") {
			return nil
		}
		return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"reviewThreads":{
		  "pageInfo":{"hasNextPage":false,"endCursor":""},
		  "nodes":[{"id":"T_fleet","isResolved":false,"isOutdated":false,"path":"a.go","line":1,
		    "comments":{"totalCount":1,"nodes":[{"body":"### Fleet finding","author":{"login":"reviewdog[bot]"}}]}}]
		}}}}`), out)
	}
	cfg, err := BuildConfig(map[string]string{"CRQ_REPO": "o/gate", "CRQ_ISSUE": "1"})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(cfg)
	if _, err := store.Update(context.Background(), func(st *State) error {
		st.Fleet.Env = map[string]string{"CRQ_FEEDBACK_BOTS": "reviewdog[bot]"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, gh, store, nil)

	threads, err := svc.OpenThreads(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].Bot != "reviewdog" || threads[0].Author != "" {
		t.Fatalf("thread = %+v, want the fleet-configured reviewer named as a bot", threads)
	}
}

// The command promises every unresolved thread, so it also gets humans'. Copying
// the author into `bot` made the output say `"bot":"alice"`.
func TestOpenThreadsDoesNotCallAPersonABot(t *testing.T) {
	gh := newFakeGitHub()
	gh.graphQL = func(query string, _ map[string]any, out any) error {
		if !strings.Contains(query, "reviewThreads") {
			return nil
		}
		return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"reviewThreads":{
		  "pageInfo":{"hasNextPage":false,"endCursor":""},
		  "nodes":[{"id":"T_human","isResolved":false,"isOutdated":true,"path":"","line":0,
		    "comments":{"totalCount":1,"nodes":[{"body":"Can we rename this?","originalLine":42,
		      "path":"a.go","author":{"login":"alice"}}]}}]
		}}}}`), out)
	}
	cfg := firingConfig()
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	threads, err := svc.OpenThreads(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want the human's", len(threads))
	}
	if threads[0].Bot != "" {
		t.Errorf("thread = %+v, want no bot on a human's thread", threads[0])
	}
	if threads[0].Author != "alice" {
		t.Errorf("thread = %+v, want the human named as the author", threads[0])
	}
	// An outdated thread often keeps only originalLine, and those are the whole
	// reason this command exists — dropping the location loses the only pointer
	// to what the comment was about.
	if threads[0].Line != 42 {
		t.Errorf("line = %d, want the original line 42", threads[0].Line)
	}
}
