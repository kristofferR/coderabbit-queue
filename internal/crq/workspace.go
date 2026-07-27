package crq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
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
	// Token authenticates clones and fetches. Empty falls back to whatever git
	// credential helper the host already has, which is the right answer on a
	// developer machine; a daemon configured with GITHUB_TOKEN alone has no
	// helper, and git does not read that variable by itself.
	Token string
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

// gitTokenEnv is the variable credentialHelper reads the token from. A fix
// session's own `git push` resolves credentials through the helper recorded in
// the mirror, so dispatch puts this in the session's environment.
const gitTokenEnv = "CRQ_GIT_TOKEN"

// credentialHelper answers git's credential query from the environment, but
// only for HTTPS requests to github.com.
//
// Repository content can select other endpoints through submodules or LFS. The
// helper therefore reads the protocol and host Git supplies on stdin and stays
// silent for every other destination, as well as when the token is unset.
const credentialHelper = `!f() { p=; h=; while IFS= read -r l; do case "$l" in protocol=*) p=${l#protocol=} ;; host=*) h=${l#host=} ;; esac; done; if test "$1" = get && test "$p" = https && test "$h" = github.com && test -n "$` + gitTokenEnv + `"; then printf 'username=x-access-token\npassword=%s\n' "$` + gitTokenEnv + `"; fi; }; f`

// git runs a git command for this workspace, injecting credentials when a token
// is configured.
//
// The token travels in the ENVIRONMENT, never in argv: the helper is a shell
// snippet that reads it, so a process listing, a log line and this package's own
// error strings all carry the snippet and never the secret.
func (w Workspace) git(ctx context.Context, dir string, args ...string) (string, error) {
	if strings.TrimSpace(w.Token) == "" {
		return gitDir(ctx, dir, args...)
	}
	full := append([]string{"-c", "credential.helper=", "-c", "credential.helper=" + credentialHelper}, args...)
	return gitEnv(ctx, dir, []string{gitTokenEnv + "=" + w.Token}, full...)
}

func (w Workspace) root() (string, error) {
	root := strings.TrimSpace(w.Root)
	if root == "" {
		var err error
		if root, err = DefaultWorkspaceRoot(); err != nil {
			return "", err
		}
	}
	// Absolute, always: `git worktree add` runs inside the mirror, so a relative
	// path would create the worktree under the mirror while the path handed back
	// points at a directory that does not exist.
	return filepath.Abs(root)
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
	// 0700, not the umask's 0755: a mirror of a private repository is private
	// source, and on a shared host the default would let any local user read it.
	if root, rerr := w.root(); rerr == nil {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(root, 0o700); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		// Enforce the refspec on EVERY call, not only at clone time. A mirror
		// created before this rule still fetches +refs/*:refs/*, and one branch
		// created in a worktree then wedges every future fetch for the whole
		// repository with "refusing to fetch into branch ... checked out at".
		if cerr := w.configureOrigin(ctx, path); cerr != nil {
			return "", fmt.Errorf("configuring %s: %w", repo, cerr)
		}
		// Two workers fetching one mirror race on git's ref locks, and the loser
		// reports "cannot lock ref" even though the winner has just made the
		// mirror current. Retry briefly, then accept the mirror as it stands
		// rather than failing a dispatch over a fetch somebody else completed.
		var ferr error
		for attempt := 0; attempt < 3; attempt++ {
			if _, ferr = w.git(ctx, path, "fetch", "--prune", "origin"); ferr == nil {
				return path, nil
			}
			// Only contention is somebody else's success. Expired
			// credentials, an unreachable remote, or a corrupt mirror must
			// reach the caller instead of handing it stale refs.
			if !isRefLockContention(ferr) {
				return "", fmt.Errorf("fetching %s: %w", repo, ferr)
			}
			if attempt < 2 {
				if err := sleepCtx(ctx, time.Duration(attempt+1)*200*time.Millisecond); err != nil {
					return "", err
				}
			}
		}
		// Repeated lock contention is an error: it can be a stale lock left by a
		// killed Git process, and without a successful fetch the mirror is not
		// known current.
		return "", fmt.Errorf("fetching %s after lock retries: %w", repo, ferr)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	// Clone somewhere else and move it into place, so two workers starting on the
	// same repository at once cannot clone into one directory — the second would
	// fail with "destination path already exists" and take the caller with it.
	staging, err := os.MkdirTemp(filepath.Dir(path), ".clone-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	pending := filepath.Join(staging, "mirror.git")
	// --bare, not --mirror. A mirror's refspec is +refs/*:refs/*, so `fetch
	// --prune` reaches into refs/heads and deletes any branch a fix session
	// created in a worktree — the documented way to make changes. Remote refs
	// live under refs/remotes/origin/*, leaving refs/heads to the sessions.
	if _, err := w.git(ctx, "", "clone", "--bare", w.remoteURL(repo), pending); err != nil {
		return "", fmt.Errorf("cloning %s: %w", repo, err)
	}
	if err := w.configureOrigin(ctx, pending); err != nil {
		return "", fmt.Errorf("configuring %s: %w", repo, err)
	}
	// A bare clone puts the remote's branches in local refs/heads. The refspec
	// above only governs later fetches, so populate refs/remotes/origin on the
	// first use too. Sessions and repository checks can then rely on origin/main
	// from their very first checkout, not only after a second dispatch.
	if _, err := w.git(ctx, pending, "fetch", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("fetching freshly cloned %s: %w", repo, err)
	}
	if err := os.Rename(pending, path); err != nil {
		if _, statErr := os.Stat(filepath.Join(path, "HEAD")); statErr == nil {
			return path, nil // somebody else won the race; their mirror is as good
		}
		return "", err
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

// originRefspec keeps fetched refs out of refs/heads, which belongs to the
// worktrees: a session's branch there must survive a fetch, and a branch checked
// out in a worktree makes any fetch that would update it fail outright.
const originRefspec = "+refs/heads/*:refs/remotes/origin/*"

// isRefLockContention reports whether a fetch failed because another process
// held Git's ref locks. It is the only fetch error another worker can resolve
// for us, so it is the only one Mirror retries and eventually tolerates.
func isRefLockContention(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "cannot lock ref") ||
		(strings.Contains(msg, ".lock") &&
			(strings.Contains(msg, "file exists") || strings.Contains(msg, "another git process")))
}

// configureOrigin makes the mirror behave like an ordinary remote, on every call
// rather than only at clone time: a mirror created before any of these rules
// still carries the old configuration, and all of them break silently.
//
// `clone --bare` sets remote.origin.mirror, which is not what the fetch refspec
// above says and not what a fix session needs: with it set, git refuses the
// documented `git push origin HEAD:refs/heads/<branch>` outright ("--mirror
// can't be combined with refspecs"). A session could then commit its fixes and
// never land them.
//
// The credential helper is there for the same session. crq's own commands pass
// it with -c, but the session runs plain `git push`, and git does not read
// GITHUB_TOKEN by itself — so a daemon authenticated by a token alone would fix
// every finding and fail at the push. The config records the SNIPPET, never the
// secret; dispatch supplies the token through the environment.
//
// Every value is read before it is written. A `git config` write takes
// config.lock, so with concurrent dispatch two checkouts of one repository
// rewriting identical values made the loser fail outright with "could not lock
// config file" — a failure the fetch retry below never covers, over a change
// that was not needed at all.
func (w Workspace) configureOrigin(ctx context.Context, path string) error {
	want := [][2]string{
		{"remote.origin.fetch", originRefspec},
		{"remote.origin.mirror", "false"},
	}
	if strings.TrimSpace(w.Token) != "" {
		want = append(want, [2]string{"credential.helper", credentialHelper})
	}
	for _, kv := range want {
		if err := configureOriginValue(ctx, path, kv[0], kv[1], kv[0] == "remote.origin.fetch"); err != nil {
			return err
		}
	}
	return nil
}

// configureOriginValue migrates one persisted setting without making another
// concurrent checkout lose a config.lock race over the same migration.
func configureOriginValue(ctx context.Context, path, key, value string, replaceAll bool) error {
	get := []string{"config", "--local", "--get", key}
	set := []string{"config", "--local", key, value}
	if replaceAll {
		get[2] = "--get-all"
		set = []string{"config", "--local", "--replace-all", key, value}
	}
	matches := func() bool {
		// Read the persisted mirror config directly. Workspace.git injects a
		// command-line credential helper, which would hide a missing local one.
		got, err := gitDir(ctx, path, get...)
		return err == nil && got == value
	}

	var writeErr error
	for attempt := 0; attempt < 3; attempt++ {
		if matches() {
			return nil
		}
		if _, writeErr = gitDir(ctx, path, set...); writeErr == nil {
			return nil
		}
		if !isConfigLockContention(writeErr) {
			return writeErr
		}
		if err := sleepCtx(ctx, time.Duration(attempt+1)*100*time.Millisecond); err != nil {
			return err
		}
	}
	// The competing writer may have completed during the final backoff.
	if matches() {
		return nil
	}
	return writeErr
}

func isConfigLockContention(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not lock config file") &&
		(strings.Contains(msg, "file exists") || strings.Contains(msg, "another git process"))
}

// Checkout is a worktree at one commit, and the directory git commands for that
// PR run in.
type Checkout struct {
	Dir    string
	Repo   string
	PR     int
	mirror string
	// token makes this handle's directory its own. Two checkouts of one PR
	// otherwise share a path, and a deferred Remove on the older handle deletes
	// the newer one's worktree out from under it.
	token string
	// auth is the workspace's credential, so a git command run IN this checkout
	// reaches the remote the same way the fetch that created it did.
	auth string
	// stop ends the filesystem heartbeat that keeps a live checkout from being
	// mistaken for abandoned.
	stop context.CancelFunc
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
	// A fork PR's head lives in the contributor's repository, and the mirror
	// fetches only this one's branches — so the commit is simply absent and the
	// worktree cannot be created. GitHub publishes it on the BASE repository as
	// refs/pull/<n>/head, which is the one ref that reaches it from here.
	if _, err := w.git(ctx, mirror, "cat-file", "-e", sha+"^{commit}"); err != nil {
		spec := fmt.Sprintf("+refs/pull/%d/head:refs/remotes/origin/pr/%d", pr, pr)
		if _, ferr := w.git(ctx, mirror, "fetch", "origin", spec); ferr != nil {
			return Checkout{}, fmt.Errorf("fetching pull %d of %s: %w", pr, repo, ferr)
		}
	}
	prDir, err := w.workPath(repo, pr)
	if err != nil {
		return Checkout{}, err
	}
	// Old generations are pruned by AGE, not by being there: another worker may
	// be building in one right now, and force-removing it would pull the ground
	// out from under a live session. Each handle removes its own on the way out;
	// this only collects what a killed process left behind.
	if err := w.pruneStaleWork(ctx, mirror, prDir); err != nil {
		return Checkout{}, err
	}
	// And the rest of this repository's checkouts, whose PRs may never be
	// dispatched again: a killed watcher's worktree for a PR that has since
	// closed or converged is visited by nothing else, so pruning only the
	// directory being checked out left it on disk forever rather than for the
	// documented staleWorkAge.
	w.sweepStaleWork(ctx, mirror, prDir)
	token := randomToken()
	dir := filepath.Join(prDir, token)
	if err := os.MkdirAll(prDir, 0o700); err != nil {
		return Checkout{}, err
	}
	if _, err := w.git(ctx, mirror, "worktree", "add", "--detach", dir, sha); err != nil {
		return Checkout{}, fmt.Errorf("checking out %s@%s: %w", repo, shortSHA(sha), err)
	}
	alive, stop := context.WithCancel(ctx)
	go keepAlive(alive, dir, heartbeatInterval)
	return Checkout{
		Dir: dir, Repo: NormalizeRepo(repo), PR: pr, mirror: mirror,
		token: token, auth: w.Token, stop: stop,
	}, nil
}

// workPath is the directory holding this PR's checkouts. Owner and name stay
// separate path components: joined with a dash, "a-b/c" and "a/b-c" would
// collide, and one repository's cleanup would delete the other's live worktree.
func (w Workspace) workPath(repo string, pr int) (string, error) {
	root, err := w.root()
	if err != nil {
		return "", err
	}
	owner, name, ok := splitRepo(repo)
	if !ok {
		return "", fmt.Errorf("repo must be owner/name, got %q", repo)
	}
	return filepath.Join(root, "work", owner, name, strconv.Itoa(pr)), nil
}

// staleWorkAge is how long an abandoned checkout survives. Longer than any fix
// session should take, short enough that a killed watcher does not leave the
// disk filling up.
const staleWorkAge = 12 * time.Hour

// heartbeatInterval is far inside staleWorkAge, so only a checkout whose owner
// has stopped refreshing it can age out.
const heartbeatInterval = 15 * time.Minute

// keepAlive refreshes a checkout's own directory timestamp until its owner is
// done. A marker file inside the checkout would appear in git status and could
// be committed by the fix session, while the directory timestamp is already
// included by newestModTime.
func keepAlive(ctx context.Context, dir string, every time.Duration) {
	for {
		if err := sleepCtx(ctx, every); err != nil {
			return
		}
		now := time.Now()
		if err := os.Chtimes(dir, now, now); err != nil {
			return
		}
	}
}

// pruneStaleWork removes checkouts under prDir that nothing has touched for
// staleWorkAge, leaving live ones alone.
func (w Workspace) pruneStaleWork(ctx context.Context, mirror, prDir string) error {
	entries, err := os.ReadDir(prDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		dir := filepath.Join(prDir, entry.Name())
		// The NEWEST file in the checkout, including the directory heartbeat:
		// editing a file or running a build inside leaves the root's mtime
		// untouched, while a live idle session refreshes it via keepAlive.
		if touched := newestModTime(dir); time.Since(touched) < staleWorkAge {
			continue
		}
		if err := w.removeWorktree(ctx, mirror, dir); err != nil {
			return err
		}
	}
	return nil
}

// sweepStaleWork prunes the stale checkouts of every OTHER PR of this
// repository, and removes a PR directory it empties.
//
// Best-effort: this is housekeeping for pull requests nobody asked about, so an
// undeletable leftover under one of them must not fail the checkout being made
// for another. The one being checked out is pruned by the caller, which does
// treat a failure as fatal — there it is in the way.
func (w Workspace) sweepStaleWork(ctx context.Context, mirror, prDir string) {
	repoDir := filepath.Dir(prDir)
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		dir := filepath.Join(repoDir, entry.Name())
		if !entry.IsDir() || dir == prDir {
			continue
		}
		if err := w.pruneStaleWork(ctx, mirror, dir); err != nil {
			continue
		}
		// Only when nothing is left: a non-empty directory still holds a live or
		// not-yet-stale checkout, and Remove refuses it for us.
		_ = os.Remove(dir)
	}
}

// newestModTime is the most recent modification anywhere under dir.
func newestModTime(dir string) time.Time {
	newest := time.Time{}
	now := time.Now()
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			modified := info.ModTime()
			if !modified.After(now) && modified.After(newest) {
				newest = modified
			}
		}
		return nil
	})
	return newest
}

// Remove deletes the worktree. Safe to call on an already-removed one.
func (c Checkout) Remove(ctx context.Context) error {
	if c.stop != nil {
		c.stop()
	}
	if c.mirror == "" || c.Dir == "" || filepath.Base(c.Dir) != c.token {
		// Not this handle's directory: a later checkout replaced it, and removing
		// it would delete a worktree somebody else is using.
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

// Git runs a git command inside this checkout, with the workspace's credentials:
// confirming a push means fetching, and on a private repository an
// unauthenticated fetch fails and reads as work that never landed.
func (c Checkout) Git(ctx context.Context, args ...string) (string, error) {
	return Workspace{Token: c.auth}.git(ctx, c.Dir, args...)
}

// gitDir runs git in dir ("" means the process's own directory) and returns its
// trimmed stdout. Stderr is folded into the error, because "exit status 128" on
// its own has never told anybody what went wrong.
func gitDir(ctx context.Context, dir string, args ...string) (string, error) {
	return gitEnv(ctx, dir, nil, args...)
}

// gitEnv is gitDir with extra environment entries.
func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
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

// workspace is crq's workspace as this configuration describes it: the root from
// the config file (not just the process environment) and the token crq already
// resolved for the API, so a daemon with GITHUB_TOKEN alone can still clone.
func (s *Service) workspace(ctx context.Context) Workspace {
	return Workspace{Root: s.cfg.WorkspaceRoot, Token: gitToken(ctx)}
}

// gitToken is the token to authenticate git with, or "" to leave git to the
// host's own credential helper.
//
// It uses the SAME resolution the API client does, including `gh auth token`.
// Reading only the environment meant the documented `gh auth login` setup — API
// calls working fine — still produced unauthenticated clones, so every private
// checkout failed at dispatch's first real act.
func gitToken(ctx context.Context) string { return ghapi.LookupToken(ctx) }

// sleepCtx waits, or returns early when the context ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
