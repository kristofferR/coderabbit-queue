package crq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Workspace is crq's own place on disk for a repository.
//
// Everything crq does with git today assumes it was RUN inside the checkout it
// cares about: localWork shells out with no directory, target inference reads
// the current branch. That is fine for a command an agent types from its own
// working copy, and useless for the daemon, which has no checkout of any of the
// repositories it reviews.
//
// A Workspace separates the two: a bare mirror per repository, fetched rather
// than re-cloned, and a throwaway worktree per head. Nothing here reads or
// writes the process's working directory.
//
// Credentials are deliberately not crq's business. The remote is the ordinary
// https URL, so whatever git credential helper the host already uses — `gh auth
// setup-git` writes one — supplies the token. crq never holds a secret it would
// then have to avoid logging.
type Workspace struct {
	// Root holds the mirrors and worktrees. Empty means the default cache
	// location (see DefaultWorkspaceRoot).
	Root string
}

// DefaultWorkspaceRoot is $XDG_CACHE_HOME/crq (or ~/.cache/crq).
func DefaultWorkspaceRoot() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CRQ_WORKSPACE")); dir != "" {
		return dir, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "crq"), nil
}

func (w Workspace) root() (string, error) {
	if strings.TrimSpace(w.Root) != "" {
		return w.Root, nil
	}
	return DefaultWorkspaceRoot()
}

// mirrorPath is where repo's bare mirror lives. The owner and name are path
// segments, so a repo string that is not exactly "owner/name" is refused rather
// than allowed to escape the root.
func (w Workspace) mirrorPath(repo string) (string, error) {
	root, err := w.root()
	if err != nil {
		return "", err
	}
	owner, name, ok := splitRepo(repo)
	if !ok {
		return "", fmt.Errorf("repo must be owner/name, got %q", repo)
	}
	return filepath.Join(root, "mirrors", owner, name+".git"), nil
}

// splitRepo splits owner/name, rejecting anything that could traverse a path.
func splitRepo(repo string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(NormalizeRepo(repo), "/")
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	for _, part := range []string{owner, name} {
		if part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			return "", "", false
		}
	}
	return owner, name, true
}

// Mirror makes sure repo's bare mirror exists and is up to date, and returns its
// path. It clones on first use and fetches afterwards — a mirror is reused
// across PRs and across rounds, so the expensive clone happens once per
// repository rather than once per dispatch.
func (w Workspace) Mirror(ctx context.Context, repo string) (string, error) {
	path, err := w.mirrorPath(repo)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		if _, err := gitDir(ctx, path, "fetch", "--prune", "origin"); err != nil {
			return "", fmt.Errorf("fetching %s: %w", repo, err)
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := gitDir(ctx, "", "clone", "--mirror", w.remoteURL(repo), path); err != nil {
		return "", fmt.Errorf("cloning %s: %w", repo, err)
	}
	return path, nil
}

// remoteURL is the URL crq clones from. CRQ_REMOTE_BASE exists so a test can
// point at a local path; in production it is github.com over https, which is
// what the host's credential helper is configured for.
func (w Workspace) remoteURL(repo string) string {
	base := strings.TrimSpace(os.Getenv("CRQ_REMOTE_BASE"))
	if base == "" {
		return "https://github.com/" + NormalizeRepo(repo) + ".git"
	}
	return strings.TrimRight(base, "/") + "/" + NormalizeRepo(repo)
}

// Checkout is a worktree at one commit, and the directory git commands for that
// PR run in.
type Checkout struct {
	Dir    string
	Repo   string
	PR     int
	mirror string
}

// Checkout creates a worktree of repo at sha, on a detached HEAD.
//
// Detached on purpose: this is a place to inspect and build, not a branch to
// commit to by accident. A caller that means to write creates its own branch.
func (w Workspace) Checkout(ctx context.Context, repo string, pr int, sha string) (Checkout, error) {
	if strings.TrimSpace(sha) == "" {
		return Checkout{}, errors.New("checkout needs a commit")
	}
	mirror, err := w.Mirror(ctx, repo)
	if err != nil {
		return Checkout{}, err
	}
	root, err := w.root()
	if err != nil {
		return Checkout{}, err
	}
	owner, name, ok := splitRepo(repo)
	if !ok {
		return Checkout{}, fmt.Errorf("repo must be owner/name, got %q", repo)
	}
	dir := filepath.Join(root, "work", fmt.Sprintf("%s-%s-%d", owner, name, pr))
	// A worktree left behind by a killed process would make this fail forever,
	// so replace rather than reuse: the head it holds is probably stale anyway.
	if err := w.removeWorktree(ctx, mirror, dir); err != nil {
		return Checkout{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Checkout{}, err
	}
	if _, err := gitDir(ctx, mirror, "worktree", "add", "--detach", dir, sha); err != nil {
		return Checkout{}, fmt.Errorf("checking out %s@%s: %w", repo, shortSHA(sha), err)
	}
	return Checkout{Dir: dir, Repo: NormalizeRepo(repo), PR: pr, mirror: mirror}, nil
}

// Remove deletes the worktree. Safe to call on an already-removed one.
func (c Checkout) Remove(ctx context.Context) error {
	if c.mirror == "" || c.Dir == "" {
		return nil
	}
	return Workspace{}.removeWorktree(ctx, c.mirror, c.Dir)
}

func (w Workspace) removeWorktree(ctx context.Context, mirror, dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		// Still prune: git may hold a record of a directory somebody deleted.
		_, _ = gitDir(ctx, mirror, "worktree", "prune")
		return nil
	}
	if _, err := gitDir(ctx, mirror, "worktree", "remove", "--force", dir); err != nil {
		// Fall back to deleting it ourselves; a stale registration is pruned.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return rmErr
		}
	}
	_, _ = gitDir(ctx, mirror, "worktree", "prune")
	return nil
}

// Git runs a git command inside this checkout.
func (c Checkout) Git(ctx context.Context, args ...string) (string, error) {
	return gitDir(ctx, c.Dir, args...)
}

// gitDir runs git in dir ("" means the process's own directory) and returns its
// trimmed stdout. Stderr is folded into the error, because "exit status 128" on
// its own has never told anybody what went wrong.
func gitDir(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
