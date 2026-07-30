package crq

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailSessionLogIsBoundedToTheRepoWorkspace(t *testing.T) {
	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	dir, err := svc.workspace(context.Background()).LogDir("o/r")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "1-head-now.log")
	if err := os.WriteFile(path, []byte("old\nnew\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, err := svc.TailSessionLog(context.Background(), "o/r", path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if tail.Text != "new\n" || !tail.Truncated || tail.Size != 8 {
		t.Fatalf("tail = %+v", tail)
	}
	outside := filepath.Join(cfg.WorkspaceRoot, "secret")
	if err := os.WriteFile(outside, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TailSessionLog(context.Background(), "o/r", outside, 100); err == nil ||
		!strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside path error = %v", err)
	}
	link := filepath.Join(dir, "linked.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TailSessionLog(context.Background(), "o/r", link, 100); err == nil {
		t.Fatal("a session log symlink escaping the repository workspace was opened")
	}
}
