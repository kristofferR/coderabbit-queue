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
	repos, owners := remoteSlugs(remotes)
	if len(repos) == 0 {
		return "", 0, fmt.Errorf("no github remote found in this checkout")
	}

	// Every remote repository, with every remote owner as a possible head. In a
	// fork checkout the branch lives in origin (me/app) while the pull request
	// is filed against upstream (owner/app), so taking the first remote and its
	// own owner finds nothing and reports that no PR exists.
	//
	// Asking for this branch specifically rather than listing every PR keeps
	// each lookup one request whatever the repository's size, and the usual
	// single-remote checkout still costs exactly one.
	type match struct {
		repo string
		pr   int
	}
	var found []match
	seen := map[string]bool{}
	for _, repo := range repos {
		for _, owner := range owners {
			query := url.Values{}
			query.Set("state", "open")
			query.Set("head", owner+":"+branch)
			pulls, err := s.gh.ListPulls(ctx, repo, query)
			if err != nil {
				return "", 0, err
			}
			for _, pull := range pulls {
				key := fmt.Sprintf("%s#%d", strings.ToLower(repo), pull.Number)
				if seen[key] {
					continue
				}
				seen[key] = true
				found = append(found, match{repo, pull.Number})
			}
		}
	}
	if len(found) == 0 {
		return "", 0, fmt.Errorf("no open pull request for %s on branch %s", strings.Join(repos, " or "), branch)
	}
	if len(found) > 1 {
		names := make([]string, 0, len(found))
		for _, m := range found {
			names = append(names, fmt.Sprintf("%s#%d", m.repo, m.pr))
		}
		return "", 0, fmt.Errorf("branch %s has %d open pull requests (%s); name the one you mean",
			branch, len(found), strings.Join(names, ", "))
	}
	return found[0].repo, found[0].pr, nil
}

// remoteSlugs reduces `git remote -v` to the GitHub repositories it names and
// the owners of those repositories, both in first-seen order (so origin leads)
// and deduplicated.
func remoteSlugs(remotes string) (repos, owners []string) {
	seenRepo, seenOwner := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(remotes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// repoSlugFromRemote is deliberately loose — it exists to MATCH a remote
		// against a repo crq already knows, where a wrong guess simply fails to
		// match. Here the slug becomes an API lookup, so a local path like
		// /home/me/code/app must not turn into a request for code/app.
		if !strings.Contains(strings.ToLower(fields[1]), "github.com") {
			continue
		}
		slug := repoSlugFromRemote(fields[1])
		if slug == "" {
			continue
		}
		if key := strings.ToLower(slug); !seenRepo[key] {
			seenRepo[key] = true
			repos = append(repos, slug)
		}
		owner := ownerOf(slug)
		if key := strings.ToLower(owner); owner != "" && !seenOwner[key] {
			seenOwner[key] = true
			owners = append(owners, owner)
		}
	}
	return repos, owners
}

func gitIn(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
