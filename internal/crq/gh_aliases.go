package crq

import (
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// Aliases for the GitHub transport that moved to internal/gh. They keep
// existing call sites and tests stable while the refactor lands package by
// package; new code should import internal/gh directly. Deleted once the
// engine extraction rewrites the callers. (The import is named ghapi because
// crq code conventionally uses gh as a variable name for the client.)

type (
	GitHub         = ghapi.GitHub
	APIError       = ghapi.APIError
	RateLimitError = ghapi.RateLimitError
	Issue          = ghapi.Issue
	Pull           = ghapi.Pull
	RepoInfo       = ghapi.RepoInfo
	IssueComment   = ghapi.IssueComment
	Review         = ghapi.Review
	ReviewComment  = ghapi.ReviewComment
	Reaction       = ghapi.Reaction
	SearchPR       = ghapi.SearchPR
	gitCommit      = ghapi.Commit
)

var (
	ErrNotFound   = ghapi.ErrNotFound
	NewGitHub     = ghapi.NewGitHub
	isThrottled   = ghapi.IsThrottled
	rateLimitWait = ghapi.ThrottleWait
	sleepCtx      = ghapi.SleepCtx
)

func init() {
	ghapi.UserAgent = "crq/" + Version
}
