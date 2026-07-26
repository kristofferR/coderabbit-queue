package crq

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
