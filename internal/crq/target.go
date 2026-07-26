package crq

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// InferTarget works out which PR the caller means from the checkout they are
// standing in.
//
// Every crq command took <repo> <pr> explicitly, which is two arguments an agent
// has to carry through a loop it is already driving from inside the repository —
// and getting either wrong is silent, because another repo's PR number is usually
// a valid PR. The checkout already knows: its remote names the repository, and
// its branch names the pull request.
//
// It is only ever a convenience. Anything it cannot establish is an error naming
// what was missing, never a guess.
func (s *Service) InferTarget(ctx context.Context) (string, int, error) {
	branch, err := gitIn(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", 0, fmt.Errorf("not inside a git checkout, so <repo> <pr> cannot be inferred: %w", err)
	}
	if branch == "HEAD" {
		return "", 0, fmt.Errorf("this checkout is detached, so there is no branch to find a pull request for")
	}
	remotes, err := gitIn(ctx, "remote", "-v")
	if err != nil {
		return "", 0, fmt.Errorf("could not read this checkout's remotes: %w", err)
	}
	repo := ""
	for _, line := range strings.Split(remotes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if slug := repoSlugFromRemote(fields[1]); slug != "" {
			repo = slug
			break
		}
	}
	if repo == "" {
		return "", 0, fmt.Errorf("no github remote found in this checkout")
	}

	// Ask for this branch's PR specifically rather than listing them all: the
	// head filter is one request whatever the repository's size.
	query := url.Values{}
	query.Set("state", "open")
	query.Set("head", ownerOf(repo)+":"+branch)
	pulls, err := s.gh.ListPulls(ctx, repo, query)
	if err != nil {
		return "", 0, err
	}
	if len(pulls) == 0 {
		return "", 0, fmt.Errorf("no open pull request for %s on branch %s", repo, branch)
	}
	if len(pulls) > 1 {
		return "", 0, fmt.Errorf("%d open pull requests for %s on branch %s; name the one you mean", len(pulls), repo, branch)
	}
	return repo, pulls[0].Number, nil
}

func gitIn(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
