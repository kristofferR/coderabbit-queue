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
// Credentials stay as close to the host's own as they can be. The remote is the
// ordinary https URL, so whatever git credential helper the host already uses —
// `gh auth setup-git` writes one — supplies the token. Only a daemon that has no
// such helper makes crq inject one of its own (see credentialHelper), and even
// then the secret lives in the environment rather than in argv or a config file.
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
	return gitEnv(ctx, dir, []string{"CRQ_GIT_TOKEN=" + w.Token}, full...)
}

// credentialHelper answers for https://github.com and nothing else.
//
// A git command can be led to a URL that repository CONTENT chose — a submodule,
// an LFS endpoint — and a helper that answers every request would hand the
// account's token to whatever host a pull request pointed it at. git writes the
// request to stdin as key=value lines and appends the operation as an argument,
// so read the protocol and host and stay quiet unless they are ours. The lines
// are drained whatever the operation is, so a store or an erase does not meet a
// closed pipe, and the snippet always exits 0.
//
// github.com literally: the token is a GitHub token, and CRQ_REMOTE_BASE (the
// only other remote crq ever uses) points at a local path in tests, which needs
// no credentials at all.
const credentialHelper = `!f() { p=; h=; while IFS= read -r l; do case "$l" in protocol=*) p=${l#protocol=} ;; host=*) h=${l#host=} ;; esac; done; if test "$1" = get && test "$p" = https && test "$h" = github.com; then printf 'username=x-access-token\npassword=%s\n' "$CRQ_GIT_TOKEN"; fi; }; f`

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
		if cerr := w.setConfig(ctx, path, "remote.origin.fetch", originRefspec); cerr != nil {
			return "", fmt.Errorf("configuring %s: %w", repo, cerr)
		}
		// A mirror cloned with --mirror also carries remote.origin.mirror=true,
		// which the refspec does not clear: a plain `git push` from a session's
		// worktree would then mirror the whole local ref namespace, publishing
		// internal refs and deleting remote branches this repository has never
		// heard of. Read before unsetting: not being set is the normal case, and
		// an unconditional --unset-all takes config.lock on every single call.
		if _, gerr := w.git(ctx, path, "config", "--get", "remote.origin.mirror"); gerr == nil {
			_, _ = w.git(ctx, path, "config", "--unset-all", "remote.origin.mirror")
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
			// Only contention is somebody else's success. Expired credentials, an
			// unreachable remote or a corrupt mirror are this caller's problem, and
			// swallowing them hands back stale refs — which surfaces later as an
			// unreadable commit rather than the fetch error that explains it.
			if !isRefLockContention(ferr) {
				return "", fmt.Errorf("fetching %s: %w", repo, ferr)
			}
			if err := sleepCtx(ctx, time.Duration(attempt+1)*200*time.Millisecond); err != nil {
				return "", err
			}
		}
		// Still contended. Another worker is updating the mirror right now, so
		// returning an error here fails a dispatch over somebody else's success —
		// the case concurrent dispatch is supposed to survive.
		return path, nil
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
	// Init and fetch, not `clone --bare`: a bare clone copies the remote's branch
	// heads straight into refs/heads, and the refspec set afterwards only governs
	// later fetches. refs/heads belongs to the sessions — a branch already sitting
	// there is one a session cannot create, and a stale commit it could land on.
	// Fetching into an empty repository leaves that namespace to them, and never
	// sets the remote.origin.mirror that `clone --mirror` did.
	if _, err := w.git(ctx, "", "init", "--bare", "--quiet", pending); err != nil {
		return "", fmt.Errorf("creating mirror of %s: %w", repo, err)
	}
	if _, err := w.git(ctx, pending, "remote", "add", "origin", w.remoteURL(repo)); err != nil {
		return "", fmt.Errorf("configuring %s: %w", repo, err)
	}
	if _, err := w.git(ctx, pending, "config", "remote.origin.fetch", originRefspec); err != nil {
		return "", fmt.Errorf("configuring %s: %w", repo, err)
	}
	if _, err := w.git(ctx, pending, "fetch", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("cloning %s: %w", repo, err)
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

// setConfig writes one config key of the mirror at path, unless it already holds
// value.
//
// git serializes configuration writes through config.lock, so two dispatches of
// the same repository arriving together make one of them fail with "could not
// lock config file" — before the fetch, which is the part written to tolerate
// concurrency. Reading first takes that write out of every call but the one that
// actually migrates something, and that one retries.
func (w Workspace) setConfig(ctx context.Context, path, key, value string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if cur, gerr := w.git(ctx, path, "config", "--get", key); gerr == nil && cur == value {
			return nil
		}
		if _, err = w.git(ctx, path, "config", key, value); err == nil {
			return nil
		}
		if !isConfigLockContention(err) {
			return err
		}
		if serr := sleepCtx(ctx, time.Duration(attempt+1)*200*time.Millisecond); serr != nil {
			return serr
		}
	}
	// Still contended: the other worker is writing this same migration, so its
	// result is the one we wanted. Only its absence is worth an error.
	if cur, gerr := w.git(ctx, path, "config", "--get", key); gerr == nil && cur == value {
		return nil
	}
	return err
}

// isConfigLockContention reports whether a config write lost the race for
// config.lock — git's own serialization, and so somebody else's success.
func isConfigLockContention(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not lock config file") ||
		strings.Contains(msg, "another git process")
}

// isRefLockContention reports whether a fetch failed because another process
// held the locks — the one failure that is somebody else's success, and so the
// only one worth retrying and then ignoring. Everything else (a bad token, an
// unreachable remote, a broken mirror) has to reach the caller.
func isRefLockContention(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "cannot lock ref"),
		strings.Contains(msg, "another git process"),
		strings.Contains(msg, ".lock") && strings.Contains(msg, "file exists"):
		return true
	}
	return false
}

// Checkout is a worktree at one commit, and the directory git commands for that
// PR run in.
type Checkout struct {
	Dir    string
	Repo   string
	PR     int
	mirror string
	// ws is the workspace that made this checkout, kept so the git commands run
	// inside it authenticate the same way the clone did. A session's push is a
	// network command like any other, and a daemon holding only GITHUB_TOKEN has
	// no credential helper to fall back on.
	ws Workspace
	// token makes this handle's directory its own. Two checkouts of one PR
	// otherwise share a path, and a deferred Remove on the older handle deletes
	// the newer one's worktree out from under it.
	token string
	// stop ends the heartbeat that keeps this checkout from being pruned while
	// the process holding it is alive.
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
	prDir, err := w.workPath(repo, pr)
	if err != nil {
		return Checkout{}, err
	}
	// Old generations are pruned by AGE, not by being there: another worker may
	// be building in one right now, and force-removing it would pull the ground
	// out from under a live session. Each handle removes its own on the way out;
	// this only collects what a killed process left behind.
	if err := pruneStaleWork(ctx, mirror, filepath.Dir(prDir)); err != nil {
		return Checkout{}, err
	}
	w.fetchPullRef(ctx, mirror, pr, sha)
	token := randomToken()
	dir := filepath.Join(prDir, token)
	if err := os.MkdirAll(prDir, 0o700); err != nil {
		return Checkout{}, err
	}
	// `--` before the positional arguments: a commit-ish beginning with a dash
	// would otherwise be read as an option, which is the shape behind a long line
	// of clone-and-checkout CVEs.
	if _, err := w.git(ctx, mirror, "worktree", "add", "--detach", "--", dir, sha); err != nil {
		return Checkout{}, fmt.Errorf("checking out %s@%s: %w", repo, shortSHA(sha), err)
	}
	// Held for as long as the caller's context lives, which is as long as this
	// handle is worth anything. A caller whose context ends at once gets what it
	// got before: a checkout that ages out of the workspace on its own.
	alive, stop := context.WithCancel(ctx)
	go keepAlive(alive, dir, heartbeatInterval)
	return Checkout{Dir: dir, Repo: NormalizeRepo(repo), PR: pr, mirror: mirror, ws: w, token: token, stop: stop}, nil
}

// fetchPullRef fetches refs/pull/<pr>/head when sha is not in the mirror yet.
//
// A PR opened from a fork has its head on no branch of the base repository, so
// the mirror's refspec never brings it down and the worktree would fail at an
// unknown commit. GitHub publishes it as refs/pull/<pr>/head. Best effort on
// purpose: the checkout that follows is the real check, and its error names the
// commit that could not be found.
func (w Workspace) fetchPullRef(ctx context.Context, mirror string, pr int, sha string) {
	if pr <= 0 {
		return
	}
	if _, err := gitDir(ctx, mirror, "cat-file", "-e", sha+"^{commit}"); err == nil {
		return
	}
	ref := fmt.Sprintf("+refs/pull/%d/head:refs/remotes/origin/pull/%d", pr, pr)
	_, _ = w.git(ctx, mirror, "fetch", "origin", ref)
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

// heartbeatInterval is how often a live checkout refreshes itself. Far inside
// staleWorkAge, so a checkout ages out only once nothing has refreshed it for
// several dozen missed beats.
const heartbeatInterval = 15 * time.Minute

// keepAlive refreshes dir's own timestamp until ctx ends.
//
// Age alone does not mean abandoned: a session can legitimately touch no file
// for hours — holding uncommitted fixes while it waits on a reviewer or on the
// queue — and pruning would then delete a checkout somebody is still using. Age
// SINCE THE LAST BEAT does mean it, because the only thing that stops the beat
// is the process that owns the checkout going away.
//
// The directory's own timestamp, not a marker file: it is what newestModTime
// already reads, and a file inside would turn up in the session's own git
// status and in the diff it is about to commit.
func keepAlive(ctx context.Context, dir string, every time.Duration) {
	for {
		if err := sleepCtx(ctx, every); err != nil {
			return
		}
		now := time.Now()
		if err := os.Chtimes(dir, now, now); err != nil {
			return // the checkout is gone; there is nothing left to keep alive
		}
	}
}

// pruneStaleWork removes checkouts anywhere under repoDir that nothing has
// touched for staleWorkAge, leaving live ones alone.
//
// Every PR of the repository, not only the one being checked out: a process
// killed mid-dispatch leaves a generation behind, and if that PR is then merged,
// closed or simply never dispatched again, nothing would ever come back to
// collect it. Sweeping the repository means any dispatch of any PR does.
func pruneStaleWork(ctx context.Context, mirror, repoDir string) error {
	prDirs, err := os.ReadDir(repoDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, prDir := range prDirs {
		entries, err := os.ReadDir(filepath.Join(repoDir, prDir.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			dir := filepath.Join(repoDir, prDir.Name(), entry.Name())
			// The NEWEST file in the checkout, not the directory's own timestamp:
			// editing a file or running a build inside leaves the root's mtime
			// untouched, so a busy session would read as abandoned. An idle one
			// keeps that root fresh itself (keepAlive).
			if touched := newestModTime(dir); time.Since(touched) < staleWorkAge {
				continue
			}
			if err := removeWorktree(ctx, mirror, dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// newestModTime is the most recent modification anywhere under dir.
func newestModTime(dir string) time.Time {
	newest := time.Time{}
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

// Remove deletes the worktree. Safe to call on an already-removed one.
func (c Checkout) Remove(ctx context.Context) error {
	if c.stop != nil {
		c.stop() // this handle is done either way, so stop claiming it is live
	}
	if c.mirror == "" || c.Dir == "" || filepath.Base(c.Dir) != c.token {
		// Not this handle's directory: a later checkout replaced it, and removing
		// it would delete a worktree somebody else is using.
		return nil
	}
	return removeWorktree(ctx, c.mirror, c.Dir)
}

func removeWorktree(ctx context.Context, mirror, dir string) error {
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

// Git runs a git command inside this checkout, with the same credentials the
// mirror was cloned with — a session's push is a network command too.
func (c Checkout) Git(ctx context.Context, args ...string) (string, error) {
	return c.ws.git(ctx, c.Dir, args...)
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
