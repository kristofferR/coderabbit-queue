package crq

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// OpenThread is one unresolved review thread, enough to decide about it and to
// pass its ID to `crq resolve`.
type OpenThread struct {
	ID       string `json:"thread_id"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Bot      string `json:"bot,omitempty"`
	Outdated bool   `json:"outdated"`
	// Title is the thread's first line, so a list is readable without opening
	// each one.
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

// OpenThreads lists a PR's unresolved review threads, INCLUDING the ones GitHub
// has marked outdated.
//
// Findings deliberately exclude outdated threads: the code they point at is
// gone, so re-reporting them would be noise, and anything with a thread ID
// blocks a round until it is resolved. But an outdated thread is still open on
// the PR, and after a push that is every thread from the previous head — so an
// agent that fixed and pushed had no way left to close them through crq at all.
// The documented answer was to stop using crq and query GitHub's GraphQL API by
// hand, which is the one thing the skill tells agents never to do.
//
// This is the read that closes that loop: list, decide, `crq resolve`.
func (s *Service) OpenThreads(ctx context.Context, repo string, pr int) ([]OpenThread, error) {
	threads, err := s.reviewThreads(ctx, repo, pr)
	if err != nil {
		return nil, err
	}
	out := make([]OpenThread, 0, len(threads))
	for _, thread := range threads {
		if thread.IsResolved {
			continue
		}
		open := OpenThread{ID: thread.ID, Path: thread.Path, Line: thread.Line, Outdated: thread.IsOutdated}
		if nodes := thread.Comments.Nodes; len(nodes) > 0 {
			first := nodes[0]
			open.Bot = dialect.NormalizeBotName(first.Author.Login)
			open.URL = first.URL
			open.Title = threadTitle(first.Body)
			if open.Path == "" {
				open.Path = first.Path
			}
			if open.Line == 0 {
				open.Line = first.Line
			}
		}
		out = append(out, open)
	}
	// Current threads first, then by location, so the ones still pointing at
	// live code are what a reader sees first.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Outdated != out[j].Outdated {
			return !out[i].Outdated
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

// markup matches the badge images, HTML wrappers and emphasis a bot puts around
// its title. Left in, a listing reads as `![P2 Badge](https://img.shields.io/...)`
// instead of what the finding says.
var markup = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)|<[^>]+>|[*_` + "`" + `#]+`)

// threadTitle reduces a comment body to one readable line. It prefers the bot's
// own bold title, which is where both CodeRabbit and Codex put the summary.
func threadTitle(body string) string {
	title := dialect.TitleFromDetailedBlock(body)
	if strings.TrimSpace(markup.ReplaceAllString(title, "")) == "" {
		title = dialect.TitleOf(body)
	}
	title = strings.Join(strings.Fields(markup.ReplaceAllString(title, " ")), " ")
	// Drop a leading severity/category rubric ("Functional Correctness | Minor |
	// Quick win"): it is the same on every finding, so it displaces the part that
	// differs.
	if _, rest, ok := strings.Cut(title, "|"); ok {
		if last := strings.LastIndex(rest, "|"); last >= 0 {
			rest = rest[last+1:]
		}
		if trimmed := strings.TrimSpace(rest); trimmed != "" {
			title = trimmed
		}
	}
	if len(title) > 120 {
		title = title[:117] + "..."
	}
	return title
}
