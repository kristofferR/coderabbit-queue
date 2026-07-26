package crq

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// A listing exists to be read. Both bots wrap their title in badge images and
// HTML, and CodeRabbit prefixes a rubric that is identical on every finding, so
// the raw first line shows everything except what the finding says.
func TestThreadTitleReadsAsText(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"codex badge",
			"**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub>  Remove the primary from the legacy view**\n\nWhen CRQ_BOT names a registry bot...",
			"Remove the primary from the legacy view",
		},
		{
			"coderabbit rubric",
			"_🎯 Functional Correctness_ | _🟡 Minor_ | _⚡ Quick win_\n\n**Reject flags supplied as option values.**\n\nDetail follows.",
			"Reject flags supplied as option values.",
		},
		{"plain", "Just a sentence about the bug.", "Just a sentence about the bug."},
		{
			// A pipe is allowed in a title; only the fixed rubric header goes.
			"pipe in the title",
			"**Support A | B configuration**\n\nDetail.",
			"Support A | B configuration",
		},
		{
			// Underscores inside an identifier are content, not emphasis.
			"identifier with underscores",
			"**Handle user_id and api_key**\n\nDetail.",
			"Handle user_id and api_key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := threadTitle(tc.body); got != tc.want {
				t.Errorf("threadTitle = %q, want %q", got, tc.want)
			}
		})
	}

	long := threadTitle(strings.Repeat("x", 400))
	if len([]rune(long)) > 120 {
		t.Errorf("title is %d chars; a listing line must stay short", len([]rune(long)))
	}

	// Truncation cuts runes, not bytes: splitting a multi-byte character leaves
	// invalid UTF-8, which JSON renders as a replacement character.
	wide := threadTitle(strings.Repeat("é", 400))
	if !utf8.ValidString(wide) {
		t.Errorf("truncated title is not valid UTF-8: %q", wide)
	}
	if strings.Contains(wide, "\uFFFD") {
		t.Errorf("truncation split a character: %q", wide)
	}
}

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
}
