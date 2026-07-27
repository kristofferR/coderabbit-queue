package crq

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// originRepo builds a real repository on disk for the dispatch tests to clone
// from, so they exercise git itself rather than a mock of it. The workspace
// package has its own copy for its own tests; a test helper is not worth
// exporting from the package under test.
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
