package crq

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// originRepo builds a real repository on disk to clone from, so these tests
// exercise git itself rather than a mock of it — the whole point of this code is
// that the git invocations are right.
func originRepo(t *testing.T, dir string) (sha string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := gitDir(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	run("init", "--initial-branch=main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "crq test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "first")
	return run("rev-parse", "HEAD")
}

// crq's daemon has no checkout of any repository it reviews, so it needs its own
// place on disk. This covers the whole shape: clone once, fetch after, worktree
// at a commit, and clean up.
func TestWorkspaceChecksOutAHeadWithoutACheckout(t *testing.T) {
	// The remote base plus "owner/name" has to resolve to a real repository, so
	// lay the origin out the way GitHub does.
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("mirror is not a git directory: %v", err)
	}

	// Second call fetches into the same mirror rather than cloning again: a
	// mirror is reused across PRs and rounds, so the clone happens once.
	again, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if again != mirror {
		t.Errorf("second Mirror = %q, want the same path %q", again, mirror)
	}

	co, err := ws.Checkout(ctx, repo, 7, sha)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(co.Dir, "README.md")); err != nil || string(body) != "one\n" {
		t.Fatalf("worktree content = %q err=%v, want the committed file", body, err)
	}
	// Detached on purpose: this is a place to inspect, not a branch to commit to
	// by accident.
	if branch, err := co.Git(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || branch != "HEAD" {
		t.Errorf("HEAD = %q err=%v, want a detached checkout", branch, err)
	}
	if head, err := co.Git(ctx, "rev-parse", "HEAD"); err != nil || head != sha {
		t.Errorf("checked out %q, want %q", head, sha)
	}

	// A worktree left by a killed process must not wedge the next attempt.
	if _, err := ws.Checkout(ctx, repo, 7, sha); err != nil {
		t.Fatalf("re-checkout over an existing worktree: %v", err)
	}
	if err := co.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(co.Dir); !os.IsNotExist(err) {
		t.Errorf("worktree still present after Remove: %v", err)
	}
	if err := co.Remove(ctx); err != nil {
		t.Errorf("removing an already-removed worktree must be harmless, got %v", err)
	}
}

// A repo name is turned into path segments, so anything that could climb out of
// the workspace root has to be refused rather than joined.
func TestWorkspaceRefusesRepoNamesThatEscape(t *testing.T) {
	ws := Workspace{Root: t.TempDir()}
	for _, repo := range []string{"", "noslash", "../etc/passwd", "owner/../../etc", "owner/", "/name"} {
		if _, err := ws.mirrorPath(repo); err == nil {
			t.Errorf("mirrorPath(%q) was accepted; it must be refused", repo)
		}
	}
}

// The error has to say what git said. "exit status 128" alone has never told
// anybody what went wrong.
func TestGitErrorsCarryStderr(t *testing.T) {
	_, err := gitDir(context.Background(), t.TempDir(), "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("expected an error outside a repository")
	}
	if !strings.Contains(err.Error(), "rev-parse") || !strings.Contains(strings.ToLower(err.Error()), "git") {
		t.Errorf("error %q does not name the command that failed", err)
	}
}

// Four ways a workspace could destroy or expose something it should not.
func TestWorkspaceIsolatesCheckouts(t *testing.T) {
	base := t.TempDir()
	sha := originRepo(t, filepath.Join(base, "owner/thing"))
	t.Setenv("CRQ_REMOTE_BASE", base)
	root := t.TempDir()
	ws := Workspace{Root: root}
	ctx := context.Background()

	// A stale handle must not delete the checkout that replaced it.
	first, err := ws.Checkout(ctx, "owner/thing", 9, sha)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ws.Checkout(ctx, "owner/thing", 9, sha)
	if err != nil {
		t.Fatal(err)
	}
	if first.Dir == second.Dir {
		t.Fatal("two checkouts of one PR share a directory, so either can delete the other")
	}
	if err := first.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.Dir); err != nil {
		t.Errorf("the live checkout was removed by a stale handle: %v", err)
	}

	// Repo names that differ only in where the slash falls must not collide:
	// "a-b/c" and "a/b-c" joined with a dash are the same string.
	sha2 := originRepo(t, filepath.Join(base, "a-b/c"))
	sha3 := originRepo(t, filepath.Join(base, "a/b-c"))
	one, err := ws.Checkout(ctx, "a-b/c", 1, sha2)
	if err != nil {
		t.Fatal(err)
	}
	two, err := ws.Checkout(ctx, "a/b-c", 1, sha3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(one.Dir); err != nil {
		t.Errorf("checking out a/b-c destroyed a-b/c's worktree: %v", err)
	}
	if strings.HasPrefix(two.Dir, one.Dir) || strings.HasPrefix(one.Dir, two.Dir) {
		t.Errorf("checkout paths overlap: %q and %q", one.Dir, two.Dir)
	}

	// A mirror of a private repository is private source: on a shared host the
	// umask default would let any local user read it.
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("workspace root mode = %o, want 0700", perm)
	}
}

// Two workers starting on the same repository at once must not clone into one
// directory — the second used to fail with "destination path already exists".
func TestMirrorSurvivesAConcurrentFirstClone(t *testing.T) {
	base := t.TempDir()
	originRepo(t, filepath.Join(base, "owner/thing"))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}

	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := ws.Mirror(context.Background(), "owner/thing")
			errs <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent first clone: %v", err)
		}
	}
}

// The token must never reach argv: a process listing, a log line and this
// package's own error strings would all carry it.
func TestGitTokenTravelsInTheEnvironment(t *testing.T) {
	ws := Workspace{Root: t.TempDir(), Token: "ghp_secret_value"}
	_, err := ws.git(context.Background(), t.TempDir(), "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("expected an error outside a repository")
	}
	if strings.Contains(err.Error(), "ghp_secret_value") {
		t.Errorf("the token leaked into an error message: %v", err)
	}
}

func TestConfigureOriginPersistsTokenCredentialHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.git")
	if _, err := gitDir(context.Background(), "", "init", "--bare", path); err != nil {
		t.Fatal(err)
	}
	ws := Workspace{Token: "ghp_secret_value"}
	if err := ws.configureOrigin(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	got, err := gitDir(context.Background(), path, "config", "--local", "--get", "credential.helper")
	if err != nil {
		t.Fatal(err)
	}
	if got != credentialHelper {
		t.Fatalf("persisted credential.helper = %q, want dispatch helper", got)
	}
}

// A relative root would make `git worktree add` — which runs inside the mirror —
// create the worktree under the mirror, while the path handed back points at a
// directory that does not exist.
func TestWorkspaceResolvesARelativeRoot(t *testing.T) {
	base := t.TempDir()
	sha := originRepo(t, filepath.Join(base, "owner/thing"))
	t.Setenv("CRQ_REMOTE_BASE", base)

	// Run from a scratch directory so a relative root has somewhere to land.
	scratch := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(scratch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	co, err := Workspace{Root: "relative-cache"}.Checkout(context.Background(), "owner/thing", 5, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(co.Dir) {
		t.Errorf("checkout dir %q is relative", co.Dir)
	}
	if _, err := os.Stat(filepath.Join(co.Dir, "README.md")); err != nil {
		t.Errorf("the checkout is not where it says it is: %v", err)
	}
}

// Another worker may be building in an earlier generation right now. Clearing
// the directory eagerly pulled the ground out from under a live session.
func TestCheckoutLeavesALiveSiblingAlone(t *testing.T) {
	base := t.TempDir()
	sha := originRepo(t, filepath.Join(base, "owner/thing"))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	first, err := ws.Checkout(ctx, "owner/thing", 6, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Checkout(ctx, "owner/thing", 6, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(first.Dir, "README.md")); err != nil {
		t.Errorf("a live checkout was removed by the next one: %v", err)
	}
}

// A fix session's documented way to make changes is a branch in its checkout.
// A --mirror clone's refspec is +refs/*:refs/*, so the next `fetch --prune`
// reached into refs/heads and deleted that branch — destroying the session's work
// between one dispatch and the next.
func TestFetchDoesNotDeleteABranchAWorktreeCreated(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	co, err := ws.Checkout(ctx, repo, 3, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git(ctx, "checkout", "-b", "crq/fix-3"); err != nil {
		t.Fatal(err)
	}

	// Another dispatch fetches the shared mirror.
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if branch, err := co.Git(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || branch != "crq/fix-3" {
		t.Fatalf("branch = %q err=%v, want the session's branch to survive a fetch", branch, err)
	}
}

// Pruning read the checkout directory's own mtime, which editing files inside
// does not update — so a busy session read as abandoned and was deleted.
func TestPruningMeasuresTheNewestFileNotTheDirectory(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	nested := filepath.Join(dir, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "edited.go")
	if err := os.WriteFile(file, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The directories look ancient; the file inside was just written.
	for _, d := range []string{dir, nested} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if since := time.Since(newestModTime(dir)); since > time.Minute {
		t.Errorf("newest modification read as %s ago; a live session would be pruned", since)
	}
}

// A mirror created before the refspec rule still fetches +refs/*:refs/*. One
// branch created in a worktree then wedges every future fetch for the whole
// repository — "refusing to fetch into branch ... checked out at" — which is how
// a single fix session stopped every dispatch for hours.
func TestMirrorMigratesAnOldRefspec(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	// Put it back the way an older crq left it.
	if _, err := gitDir(ctx, mirror, "config", "remote.origin.fetch", "+refs/*:refs/*"); err != nil {
		t.Fatal(err)
	}
	co, err := ws.Checkout(ctx, repo, 4, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git(ctx, "checkout", "-b", "session-branch"); err != nil {
		t.Fatal(err)
	}

	// The next dispatch of ANY pr in this repository fetches the same mirror.
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatalf("a branch in one worktree wedged the whole repository: %v", err)
	}
	got, err := gitDir(ctx, mirror, "config", "--get", "remote.origin.fetch")
	if err != nil || got != originRefspec {
		t.Errorf("refspec = %q err=%v, want it migrated to %q", got, err, originRefspec)
	}
}

// The push a fix session is told to make — `git push origin HEAD:refs/heads/<branch>`
// from a detached checkout — is the whole point of the worktree. `clone --bare`
// leaves remote.origin.mirror set, and git then refuses that push outright
// ("--mirror can't be combined with refspecs"), so a session could commit its
// fixes and never land them.
func TestSessionCanPushItsDetachedHead(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	// The remote must not have the branch checked out, or it refuses the push for
	// its own reasons.
	if _, err := gitDir(context.Background(), filepath.Join(base, repo), "config", "receive.denyCurrentBranch", "ignore"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	co, err := ws.Checkout(ctx, repo, 5, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git(ctx, "-c", "user.email=t@example.invalid", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "fix the finding"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git(ctx, "push", "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("a session cannot push its work: %v", err)
	}
}

// A mirror cloned by an older crq carries remote.origin.mirror, and every session
// using it hits the same refusal — so the rule is enforced on each call.
func TestMirrorMigratesAwayFromMirrorMode(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(ctx, mirror, "config", "--bool", "remote.origin.mirror", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	// Unset or explicitly false: both answer "not a mirror" to git, and which one
	// migration leaves behind is not something a session can tell apart.
	if got, _ := gitDir(ctx, mirror, "config", "--get", "remote.origin.mirror"); got != "" && got != "false" {
		t.Errorf("remote.origin.mirror = %q, want it turned off so sessions can push", got)
	}
}

// With uncapped dispatch two PRs of one repository call Mirror at the same time.
// Every `git config` write takes config.lock, so rewriting values that were
// already correct made the loser fail the whole checkout with "could not lock
// config file" — a failure the fetch retry does not cover.
func TestMirrorDoesNotRewriteConfigItAgreesWith(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	// Somebody else holds the config lock, which is what the loser of that race
	// sees. Nothing here needs writing, so nothing may try.
	lock := filepath.Join(mirror, "config.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lock)
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Errorf("a checkout failed over configuration it did not need to change: %v", err)
	}
}

// The only age-based cleanup runs from a checkout, so pruning just the directory
// being checked out left a killed watcher's worktree for a PR that has since
// closed or converged on disk forever, rather than for staleWorkAge.
func TestCheckoutSweepsAnotherPullRequestsAbandonedWorktree(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	abandoned, err := ws.Checkout(ctx, repo, 5, sha)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleWorkAge)
	if err := filepath.WalkDir(abandoned.Dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}

	// A checkout for a DIFFERENT pull request of the same repository.
	if _, err := ws.Checkout(ctx, repo, 6, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned.Dir); !os.IsNotExist(err) {
		t.Errorf("pr 5's abandoned worktree is still there (%v); nothing else will ever visit it", err)
	}

	// A live one is left alone: another worker may be building in it right now.
	live, err := ws.Checkout(ctx, repo, 7, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Checkout(ctx, repo, 8, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live.Dir); err != nil {
		t.Errorf("a live worktree was swept: %v", err)
	}
}

func TestMirrorReturnsNonContentionFetchFailures(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing-origin")
	if _, err := gitDir(ctx, mirror, "config", "remote.origin.url", missing); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err == nil {
		t.Fatal("an unavailable remote was hidden by the existing mirror")
	} else if isRefLockContention(err) {
		t.Fatalf("an unavailable remote was misclassified as ref-lock contention: %v", err)
	}
}

func TestCredentialHelperAnswersOnlyForGitHub(t *testing.T) {
	ask := func(request string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", strings.TrimPrefix(credentialHelper, "!")+" get")
		cmd.Stdin = strings.NewReader(request)
		cmd.Env = append(os.Environ(), gitTokenEnv+"=ghp_secret_value")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("credential helper failed on %q: %v", request, err)
		}
		return string(out)
	}

	if got := ask("protocol=https\nhost=github.com\n\n"); !strings.Contains(got, "password=ghp_secret_value") {
		t.Errorf("helper answered github.com with %q, want the token", got)
	}
	for _, request := range []string{
		"protocol=https\nhost=evil.example\n\n",
		"protocol=https\nhost=github.com.evil.example\n\n",
		"protocol=http\nhost=github.com\n\n",
	} {
		if got := ask(request); strings.Contains(got, "ghp_secret_value") {
			t.Errorf("helper leaked the token to %q: %q", request, got)
		}
	}
}

func TestALiveCheckoutKeepsItselfFromBeingPruned(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * staleWorkAge)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if since := time.Since(newestModTime(dir)); since < staleWorkAge {
		t.Fatalf("checkout reads as %s old, want it stale before the heartbeat", since)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go keepAlive(ctx, dir, time.Millisecond)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if time.Since(newestModTime(dir)) < time.Minute {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("an idle checkout still reads as abandoned while its owner is alive")
}
