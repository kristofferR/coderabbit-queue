package crq

import (
	"context"
	"sort"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// OpenThread is one unresolved review thread, enough to decide about it and to
// pass its ID to `crq resolve`.
type OpenThread struct {
	ID   string `json:"thread_id"`
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
	Bot  string `json:"bot,omitempty"`
	// Author names a human reviewer, when the thread is not a bot's.
	Author   string `json:"author,omitempty"`
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
			// Only when the author is actually a reviewer bot. The command
			// promises every unresolved thread, humans included, and labelling
			// "alice" as a bot makes the output plainly wrong.
			if login := first.Author.Login; isReviewerBot(s.cfg, login) {
				open.Bot = dialect.NormalizeBotName(login)
			} else {
				open.Author = login
			}
			open.URL = first.URL
			open.Title = dialect.ThreadTitle(first.Author.Login, first.Body)
			if open.Path == "" {
				open.Path = first.Path
			}
			if open.Line == 0 {
				// GitHub clears both current lines on an outdated thread while
				// keeping originalLine — and outdated threads are the whole reason
				// this command exists.
				open.Line = firstPositive(first.Line, first.OriginalLine)
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

// isReviewerBot reports whether login is a reviewer crq knows: the configured
// primary, a configured co-reviewer, or a registry entry.
func isReviewerBot(cfg Config, login string) bool {
	key := dialect.NormalizeBotName(login)
	if key == dialect.NormalizeBotName(cfg.Bot) {
		return true
	}
	for _, cb := range cfg.CoBots {
		if dialect.NormalizeBotName(cb.Login) == key {
			return true
		}
	}
	_, known := dialect.CoReviewerByName(login)
	return known
}
