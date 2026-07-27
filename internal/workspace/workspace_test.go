package workspace

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

// One file stamped in the future — an extracted artifact, a corrected clock —
// made time.Since negative and so always under staleWorkAge, and the abandoned
// checkout holding it would never be collected.
func TestPruningIgnoresATimestampInTheFuture(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * staleWorkAge)
	future := filepath.Join(dir, "artifact.tar")
	if err := os.WriteFile(future, []byte("packed elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	ahead := time.Now().Add(90 * 24 * time.Hour)
	if err := os.Chtimes(future, ahead, ahead); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if since := time.Since(newestModTime(dir)); since < staleWorkAge {
		t.Errorf("the checkout reads as %s old, want the future stamp ignored and the checkout collectable", since)
	}
}

// `clone --bare` copies the remote's branch heads straight into refs/heads, and
// a refspec set afterwards only governs later fetches. A session could then not
// create its branch — the name is taken — or would land on the commit the clone
// froze there.
func TestANewMirrorLeavesRefsHeadsToTheSessions(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	if _, err := gitDir(context.Background(), filepath.Join(base, repo), "branch", "feature"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if heads, err := gitDir(ctx, mirror, "for-each-ref", "refs/heads"); err != nil || heads != "" {
		t.Errorf("refs/heads = %q err=%v, want it empty and left to the sessions", heads, err)
	}
	if remotes, err := gitDir(ctx, mirror, "for-each-ref", "--format=%(refname)", "refs/remotes/origin"); err != nil || !strings.Contains(remotes, "refs/remotes/origin/feature") {
		t.Errorf("remote refs = %q err=%v, want the branches fetched under origin", remotes, err)
	}

	// The name a remote branch occupies is therefore free for a session.
	co, err := ws.Checkout(ctx, repo, 8, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git(ctx, "checkout", "-b", "feature"); err != nil {
		t.Errorf("a session could not create its branch: %v", err)
	}
}

// A mirror cloned by an older crq also has remote.origin.mirror=true, which the
// refspec does not clear: a plain push from a session's worktree would mirror
// every local ref, publishing internal refs and deleting remote branches.
func TestMirrorMigrationClearsThePushMirrorFlag(t *testing.T) {
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
	if _, err := gitDir(ctx, mirror, "config", "remote.origin.mirror", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if got, err := gitDir(ctx, mirror, "config", "--get", "remote.origin.mirror"); err == nil {
		t.Errorf("remote.origin.mirror = %q, want it unset", got)
	}
}

// The mirror's HEAD exists because the mirror exists, so testing for it after a
// failed fetch declared every failure contention: expired credentials and an
// unreachable remote came back as success with stale refs, and the caller met
// them later as an unreadable commit instead.
func TestMirrorReportsAFetchFailureThatIsNotContention(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	// The remote goes away, the way an expired token or a network outage does.
	if err := os.RemoveAll(filepath.Join(base, repo)); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err == nil {
		t.Error("a failed fetch was reported as a current mirror")
	}
}

// A PR opened from a fork has its head on no branch of the base repository, so
// the mirror's refspec never brings it down; GitHub publishes it as
// refs/pull/<pr>/head.
func TestCheckoutFetchesAForkHeadFromThePullRef(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	origin := filepath.Join(base, repo)
	originRepo(t, origin)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, err := gitDir(ctx, origin, args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	// A commit reachable from the pull ref alone, exactly like a fork's head.
	run("checkout", "-q", "-b", "from-a-fork")
	if err := os.WriteFile(filepath.Join(origin, "fork.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "fork.txt")
	run("commit", "-m", "fork work")
	forkSHA := run("rev-parse", "HEAD")
	run("update-ref", "refs/pull/11/head", forkSHA)
	run("checkout", "-q", "main")
	run("branch", "-qD", "from-a-fork")

	t.Setenv("CRQ_REMOTE_BASE", base)
	co, err := Workspace{Root: t.TempDir()}.Checkout(ctx, repo, 11, forkSHA)
	if err != nil {
		t.Fatalf("checking out a fork head: %v", err)
	}
	if head, err := co.Git(ctx, "rev-parse", "HEAD"); err != nil || head != forkSHA {
		t.Errorf("checked out %q err=%v, want the fork head %q", head, err, forkSHA)
	}
}

// Pruning ran only under the PR being checked out, so a generation left by a
// killed process was collected only if that same PR was dispatched again — never,
// once it was merged or closed.
func TestPruningCollectsAnAbandonedCheckoutOfAnotherPR(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	abandoned, err := ws.workPath(repo, 21)
	if err != nil {
		t.Fatal(err)
	}
	abandoned = filepath.Join(abandoned, "leftover")
	if err := os.MkdirAll(abandoned, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(abandoned, "stale.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleWorkAge)
	for _, p := range []string{file, abandoned} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// A dispatch of a different PR is the only visitor this repository gets.
	if _, err := ws.Checkout(ctx, repo, 22, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("an abandoned checkout of another PR survived: %v", err)
	}
}

// A daemon holding only GITHUB_TOKEN has no credential helper, so a push from a
// session's checkout could not authenticate even though the clone did.
func TestCheckoutGitCarriesTheWorkspaceCredentials(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ctx := context.Background()

	co, err := Workspace{Root: t.TempDir(), Token: "ghp_secret_value"}.Checkout(ctx, repo, 12, sha)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := co.Git(ctx, "config", "--get-all", "credential.helper")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(helper, "CRQ_GIT_TOKEN") {
		t.Errorf("credential.helper = %q, want the workspace's helper", helper)
	}
	if strings.Contains(helper, "ghp_secret_value") {
		t.Errorf("the token itself reached git's configuration: %q", helper)
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

// A mirror an older crq made with `git clone --mirror` had already copied the
// remote's branches into refs/heads, and changing the fetch refspec does not
// move them: the name a session needs stays taken by a ref frozen at the commit
// that clone saw.
func TestMirrorDropsHeadsAnOldCloneLeftInPlace(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	origin := filepath.Join(base, repo)
	sha := originRepo(t, origin)
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()
	if _, err := gitDir(ctx, origin, "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	// Lay the mirror out the way the older crq left it.
	mirror, err := ws.mirrorPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(ctx, "", "clone", "--mirror", "--quiet", ws.remoteURL(repo), mirror); err != nil {
		t.Fatal(err)
	}
	// A branch of a session's own, which origin has never heard of.
	if _, err := gitDir(ctx, mirror, "update-ref", "refs/heads/crq/fix-3", sha); err != nil {
		t.Fatal(err)
	}

	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	heads, err := gitDir(ctx, mirror, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(heads, "refs/heads/feature") || strings.Contains(heads, "refs/heads/main") {
		t.Errorf("refs/heads = %q, want the clone's copies of origin's branches gone", heads)
	}
	if !strings.Contains(heads, "refs/heads/crq/fix-3") {
		t.Errorf("refs/heads = %q, want a session's own branch left alone", heads)
	}
	if remotes, err := gitDir(ctx, mirror, "for-each-ref", "--format=%(refname)", "refs/remotes/origin"); err != nil || !strings.Contains(remotes, "refs/remotes/origin/feature") {
		t.Errorf("remote refs = %q err=%v, want origin's branches under origin", remotes, err)
	}

	// The name is free for a session again — and stays the session's, even though
	// origin has a branch of that name: `git update-ref -d` would delete a
	// checked-out branch without complaint, work and all.
	co, err := ws.Checkout(ctx, repo, 3, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git(ctx, "checkout", "-b", "feature"); err != nil {
		t.Fatalf("a session could not create its branch: %v", err)
	}
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if branch, err := co.Git(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || branch != "feature" {
		t.Errorf("branch = %q err=%v, want the session's checked-out branch to survive", branch, err)
	}
}

// Checked-out status alone is not what tells a clone's leftover from a session's
// branch: a session that detaches HEAD to look at another commit still owns the
// branch it committed to, and deleting that ref is the only thing keeping its
// commits reachable.
func TestMirrorKeepsASessionBranchThatIsNotCheckedOut(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	origin := filepath.Join(base, repo)
	sha := originRepo(t, origin)
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()
	// Origin has the name too, which is what made the mirror read the session's
	// branch as a copy of it.
	if _, err := gitDir(ctx, origin, "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	co, err := ws.Checkout(ctx, repo, 7, sha)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "crq test"},
		{"checkout", "-b", "feature"},
		{"commit", "--allow-empty", "-m", "unpushed work"},
		{"checkout", "--detach"},
	} {
		if _, err := co.Git(ctx, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	work, err := co.Git(ctx, "rev-parse", "refs/heads/feature")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	mirror, err := ws.mirrorPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := gitDir(ctx, mirror, "rev-parse", "refs/heads/feature"); err != nil || got != work {
		t.Errorf("refs/heads/feature = %q err=%v, want the session's unpushed commit %s", got, err, work)
	}
}

// Once an old mirror's fetched heads have been removed, refs/heads belongs to
// sessions. An equal-tip branch is still session state: deleting it on every
// refresh also deletes its tracking configuration and reflog.
func TestMirrorKeepsAnEqualTipSessionBranchAfterMigration(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	origin := filepath.Join(base, repo)
	sha := originRepo(t, origin)
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()
	if _, err := gitDir(ctx, origin, "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	// Exercise the legacy-clone migration before the session takes ownership of
	// the name. Later Mirror calls must not run that cleanup against its branch.
	mirror, err := ws.mirrorPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(ctx, "", "clone", "--mirror", "--quiet", ws.remoteURL(repo), mirror); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}

	co, err := ws.Checkout(ctx, repo, 7, sha)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"checkout", "-b", "feature"},
		{"config", "branch.feature.remote", "origin"},
		{"config", "branch.feature.merge", "refs/heads/feature"},
		{"checkout", "--detach"},
	} {
		if _, err := co.Git(ctx, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	if _, err := co.Git(ctx, "reflog", "exists", "refs/heads/feature"); err != nil {
		t.Fatalf("session branch had no reflog before refresh: %v", err)
	}

	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if got, err := gitDir(ctx, mirror, "rev-parse", "refs/heads/feature"); err != nil || got != sha {
		t.Errorf("refs/heads/feature = %q err=%v, want equal-tip session branch %s", got, err, sha)
	}
	if got, err := gitDir(ctx, mirror, "config", "--get", "branch.feature.remote"); err != nil || got != "origin" {
		t.Errorf("branch.feature.remote = %q err=%v, want tracking configuration preserved", got, err)
	}
	if got, err := gitDir(ctx, mirror, "config", "--get", "branch.feature.merge"); err != nil || got != "refs/heads/feature" {
		t.Errorf("branch.feature.merge = %q err=%v, want tracking configuration preserved", got, err)
	}
	if _, err := gitDir(ctx, mirror, "reflog", "exists", "refs/heads/feature"); err != nil {
		t.Errorf("session branch reflog was deleted: %v", err)
	}
}

// A mirror with a second remote.origin.fetch refused a single-value write —
// "cannot overwrite multiple values with a single value" — which would have
// failed every later Mirror call for that repository for good.
func TestMirrorMigratesAMultiValuedRefspec(t *testing.T) {
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
	// The refspec that reaches into refs/heads, added rather than replacing.
	if _, err := gitDir(ctx, mirror, "config", "--add", "remote.origin.fetch", "+refs/*:refs/*"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatalf("a mirror with two refspecs could not be migrated: %v", err)
	}
	if got, err := gitDir(ctx, mirror, "config", "--get-all", "remote.origin.fetch"); err != nil || got != originRefspec {
		t.Errorf("refspec = %q err=%v, want only %q", got, err, originRefspec)
	}
}

// A ref lock left behind by a killed git never clears, and reads exactly like a
// live race for as long as it sits there. Retrying past it and handing back the
// mirror anyway presented refs known to be stale as current ones.
func TestMirrorReportsAFetchItCouldNotComplete(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	origin := filepath.Join(base, repo)
	originRepo(t, origin)
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	// Origin moves on, so the next fetch has a ref it must update...
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "second"}} {
		if _, err := gitDir(ctx, origin, args...); err != nil {
			t.Fatal(err)
		}
	}
	// ...and the lock makes that impossible for as long as it is there.
	lock := filepath.Join(mirror, "refs", "remotes", "origin", "main.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err == nil {
		t.Error("a mirror that could not be fetched was reported as current")
	}
}

// Discarding the failure to clear remote.origin.mirror handed back a mirror
// whose later plain push still had mirror semantics — publishing internal refs
// and deleting remote branches, the exact hazard the migration exists for.
func TestMirrorReportsAPushMirrorFlagItCouldNotClear(t *testing.T) {
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
	if _, err := gitDir(ctx, mirror, "config", "remote.origin.mirror", "true"); err != nil {
		t.Fatal(err)
	}
	// Somebody else holds the lock for good, the way a killed git does.
	if err := os.WriteFile(filepath.Join(mirror, "config.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(mirror, "config.lock"))
	if _, err := ws.Mirror(ctx, repo); err == nil {
		t.Error("a mirror still configured to push as a mirror was handed back as usable")
	}
}

// git serializes config writes through config.lock, so an unconditional write on
// every Mirror call made two dispatches of one repository collide with "could
// not lock config file" — before the fetch, which is the part written to survive
// concurrency. A mirror already holding the right value must not write at all.
func TestMirrorDoesNotWriteConfigThatIsAlreadyCurrent(t *testing.T) {
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
	// Somebody else is holding the lock, exactly as a concurrent dispatch would.
	if err := os.WriteFile(filepath.Join(mirror, "config.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(mirror, "config.lock"))
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Errorf("a held config.lock failed a dispatch that had nothing to write: %v", err)
	}
}

// A git command can be led to a URL that repository content chose — a submodule,
// an LFS endpoint — so a helper that answered every request would hand a pull
// request the account's token.
func TestCredentialHelperAnswersOnlyForGitHub(t *testing.T) {
	ask := func(t *testing.T, request string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", strings.TrimPrefix(credentialHelper, "!")+" get")
		cmd.Stdin = strings.NewReader(request)
		cmd.Env = append(os.Environ(), "CRQ_GIT_TOKEN=ghp_secret_value")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("credential helper failed on %q: %v", request, err)
		}
		return string(out)
	}
	if got := ask(t, "protocol=https\nhost=github.com\n\n"); !strings.Contains(got, "password=ghp_secret_value") {
		t.Errorf("helper answered github.com with %q, want the token", got)
	}
	for _, request := range []string{
		"protocol=https\nhost=evil.example\n\n",
		"protocol=https\nhost=github.com.evil.example\n\n",
		"protocol=http\nhost=github.com\n\n",
	} {
		if got := ask(t, request); strings.Contains(got, "ghp_secret_value") {
			t.Errorf("helper leaked the token to %q: %q", request, got)
		}
	}
}

// A session sitting idle — holding uncommitted fixes while it waits on a
// reviewer — touches no file for hours, and pruning by age alone would delete
// the checkout out from under it. A live handle keeps its own timestamp fresh.
func TestALiveCheckoutKeepsItselfFromBeingPruned(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * staleWorkAge)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if since := time.Since(newestModTime(dir)); since < staleWorkAge {
		t.Fatalf("the checkout reads as %s old, wanted it stale to begin with", since)
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
	t.Error("an idle checkout still read as abandoned while its process was alive")
}

func TestCheckoutGitRefreshesRotatedCredentials(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	token := "ghp_first"
	ws := Workspace{
		Root: t.TempDir(),
		TokenSource: func(context.Context) string {
			return token
		},
	}
	co, err := ws.Checkout(context.Background(), repo, 13, sha)
	if err != nil {
		t.Fatal(err)
	}

	token = "ghp_rotated"
	out, err := co.Git(
		context.Background(),
		"-c", `alias.current-token=!f() { printf '%s' "$CRQ_GIT_TOKEN"; }; f`,
		"current-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if out != token {
		t.Errorf("checkout used token %q after rotation, want %q", out, token)
	}
}

func TestMirrorPrunesDeletedTagsAndRefreshesMovedTags(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	origin := filepath.Join(base, repo)
	first := originRepo(t, origin)
	ctx := context.Background()
	if _, err := gitDir(ctx, origin, "tag", "deleted"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(ctx, origin, "tag", "release"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := gitDir(ctx, origin, "tag", "-d", "deleted"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "second"}} {
		if _, err := gitDir(ctx, origin, args...); err != nil {
			t.Fatal(err)
		}
	}
	second, err := gitDir(ctx, origin, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(ctx, origin, "tag", "-f", "release", second); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Mirror(ctx, repo); err != nil {
		t.Fatal(err)
	}

	if _, err := gitDir(ctx, mirror, "show-ref", "--verify", "--quiet", "refs/tags/deleted"); err == nil {
		t.Error("deleted remote tag survived in the reused mirror")
	}
	if got, err := gitDir(ctx, mirror, "rev-parse", "refs/tags/release"); err != nil || got != second {
		t.Errorf("release tag = %q err=%v, want moved tag %q (was %q)", got, err, second, first)
	}
}

func TestFetchedHeadDeletionRechecksWorktreeOccupancy(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	origin := filepath.Join(base, repo)
	sha := originRepo(t, origin)
	if _, err := gitDir(context.Background(), origin, "branch", "feature"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir()}
	ctx := context.Background()
	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitDir(ctx, mirror, "update-ref", "refs/heads/feature", sha); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "work")
	if _, err := gitDir(ctx, mirror, "worktree", "add", "--", dir, "feature"); err != nil {
		t.Fatal(err)
	}

	if err := deleteFetchedHead(ctx, mirror, "feature"); err != nil {
		t.Fatalf("worktree-aware deletion rejected a branch a session attached: %v", err)
	}
	if got, err := gitDir(ctx, mirror, "rev-parse", "refs/heads/feature"); err != nil || got != sha {
		t.Errorf("checked-out branch = %q err=%v, want it preserved at %q", got, err, sha)
	}
}

func TestFreshHeartbeatAvoidsDeepPruningDecision(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * staleWorkAge)
	file := filepath.Join(dir, "generated.bin")
	if err := os.WriteFile(file, []byte("large tree stand-in"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		t.Fatal(err)
	}
	if !rootHeartbeatFresh(dir, now.Add(time.Second)) {
		t.Fatal("fresh checkout root did not take the heartbeat fast path")
	}
}

// A worktree is made for somebody else to work in, and that somebody pushes with
// a plain `git push`. crq's own commands pass the credential helper with -c,
// which lasts one command, so the mirror has to carry it for the ones this
// package does not run.
func TestMirrorPersistsTheCredentialHelperForOtherCallers(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)
	ws := Workspace{Root: t.TempDir(), Token: "ghp_secret_value"}
	ctx := context.Background()

	mirror, err := ws.Mirror(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gitDir(ctx, mirror, "config", "--local", "--get", "credential.helper")
	if err != nil {
		t.Fatalf("no credential helper persisted, so a session's own push has none: %v", err)
	}
	if got != credentialHelper {
		t.Errorf("persisted credential.helper = %q, want the workspace helper", got)
	}
	// The snippet, never the secret: a mirror somebody else finds on disk must
	// hand out nothing without TokenEnv set in the environment.
	if strings.Contains(got, "ghp_secret_value") {
		t.Error("the token itself was written into the mirror's config")
	}
}

// Without a token there is nothing to inject, and rewriting the host's own
// credential configuration would be crq overriding a choice that is not its own.
func TestMirrorLeavesTheHostsCredentialConfigurationAlone(t *testing.T) {
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
	if got, _ := gitDir(ctx, mirror, "config", "--local", "--get", "credential.helper"); got != "" {
		t.Errorf("credential.helper = %q, want none written when crq has no token", got)
	}
}
