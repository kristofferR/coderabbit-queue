// Package dialect holds every text-format assumption crq makes about review
// bots: CodeRabbit's and Codex's completion phrases, rate-limit notices,
// findings markup, SHA conventions, and severity vocabulary. Each heuristic
// exists because a real bot message broke crq once; testdata/ pins them as a
// golden corpus. The package is pure text → structure: no I/O, no config
// access (markers are injected as struct fields), no GitHub types.
package dialect

import (
	"strings"
	"time"
)

// Finding is one actionable review item surfaced to agents. The JSON shape is
// part of crq's frozen external contract (llms.txt) — do not change the tags.
type Finding struct {
	ID        string    `json:"id"`
	Bot       string    `json:"bot"`
	Severity  string    `json:"severity"`
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	ThreadID  string    `json:"thread_id,omitempty"`
	CommentID int64     `json:"comment_id,omitempty"`
	ReviewID  int64     `json:"review_id,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	URL       string    `json:"url,omitempty"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// ReviewMeta carries the fields of a submitted review that the body parsers
// attach to extracted findings. It exists so this package needs no GitHub
// wire types.
type ReviewMeta struct {
	ID          int64
	CommitID    string
	HTMLURL     string
	SubmittedAt time.Time
}

func IsActionableFinding(finding Finding) bool {
	title := CompactReviewBody(finding.Title)
	body := CompactReviewBody(finding.Body)
	if title == "" && body == "" {
		return false
	}
	text := strings.ToLower(title + "\n" + body)
	return !IsNonActionableText(text)
}
