package dialect

import (
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
			if got := ThreadTitle("coderabbitai[bot]", tc.body); got != tc.want {
				t.Errorf("threadTitle = %q, want %q", got, tc.want)
			}
		})
	}

	long := ThreadTitle("coderabbitai[bot]", strings.Repeat("x", 400))
	if len([]rune(long)) > 120 {
		t.Errorf("title is %d chars; a listing line must stay short", len([]rune(long)))
	}

	// Truncation cuts runes, not bytes: splitting a multi-byte character leaves
	// invalid UTF-8, which JSON renders as a replacement character.
	wide := ThreadTitle("coderabbitai[bot]", strings.Repeat("é", 400))
	if !utf8.ValidString(wide) {
		t.Errorf("truncated title is not valid UTF-8: %q", wide)
	}
	if strings.Contains(wide, "\uFFFD") {
		t.Errorf("truncation split a character: %q", wide)
	}
}

// Every bot wraps its summary differently, and the first bold span is the
// SEVERITY for two of them — a listing that says "High Severity" for every
// Bugbot finding has told the reader nothing.
func TestThreadTitleSkipsASeverityLabel(t *testing.T) {
	for _, tc := range []struct {
		name, author, body, want string
	}{
		{
			"bugbot heading then severity",
			"cursor[bot]",
			"### Nil map write in the claim path\n\n**High Severity**\n\nDetail follows.",
			"Nil map write in the claim path",
		},
		{
			"macroscope severity first",
			"macroscopeapp[bot]",
			"🟠 **High** internal/crq/service.go\n\n### Slot released twice\n\nDetail.",
			"Slot released twice",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThreadTitle(tc.author, tc.body); got != tc.want {
				t.Errorf("ThreadTitle = %q, want %q", got, tc.want)
			}
		})
	}
}
