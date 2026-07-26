package crq

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// Dispatch is the one place crq starts something that writes code, so what it
// hands the session has to be right: the PR's own worktree at the head the
// findings are about, and the findings themselves.
func TestWatchDispatchesAFixSessionWithItsContext(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	cfg.AllowRepos = map[string]bool{repo: true}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	var pull ghapi.Pull
	pull.State = "open"
	pull.Number = 4
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 4)] = pull

	// A record of what the session was given, written by the "session" itself.
	record := filepath.Join(t.TempDir(), "record.json")
	script := filepath.Join(t.TempDir(), "session.sh")
	body := "#!/bin/sh\n" +
		"printf '{\"repo\":\"%s\",\"pr\":\"%s\",\"head\":\"%s\",\"cwd\":\"%s\",\"findings\":\"%s\"}' " +
		"\"$CRQ_DISPATCH_REPO\" \"$CRQ_DISPATCH_PR\" \"$CRQ_DISPATCH_HEAD\" \"$(pwd)\" \"$CRQ_DISPATCH_FINDINGS\" > " + record + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	report := NextReport{Repo: repo, PR: 4, Head: sha, Action: "fix"}
	seedRound(t, store, cfg, repo, 4, sha, PhaseQueued, time.Now().UTC(), 0)

	ok, why := svc.dispatch(context.Background(), WatchOptions{
		Dispatch: true, Command: []string{script}, MaxAttempts: 3,
	}, report)
	if !ok {
		t.Fatalf("dispatch did not run: %s", why)
	}

	var got struct{ Repo, PR, Head, Cwd, Findings string }
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the session did not run: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Repo != repo || got.PR != "4" || got.Head != sha {
		t.Errorf("session context = %+v, want %s#4@%s", got, repo, sha)
	}
	// It ran in a checkout of the PR, not in whatever directory crq was started
	// from — the entire reason the workspace exists.
	if !strings.HasPrefix(got.Cwd, cfg.WorkspaceRoot) {
		t.Errorf("session ran in %q, want a worktree under %q", got.Cwd, cfg.WorkspaceRoot)
	}
	if got.Findings == "" {
		t.Error("the session was given no findings file")
	}

	// The claim is released afterwards, so the next round is not blocked by a
	// session that already finished.
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, 4)
	if round == nil || round.DispatchHeld(time.Now().UTC()) {
		t.Errorf("claim still held after the session finished: %#v", round.Dispatch)
	}
	if round.Dispatch == nil || round.Dispatch.Attempts != 1 {
		t.Errorf("attempts = %#v, want 1 recorded", round.Dispatch)
	}
	// And the worktree is cleaned up rather than left to accumulate. Empty
	// parent directories are fine; a checkout still holding a repository is not.
	_ = filepath.WalkDir(filepath.Join(cfg.WorkspaceRoot, "work"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == ".git" {
			t.Errorf("a worktree was left behind at %s", filepath.Dir(path))
		}
		return nil
	})
}

// Watching is an observation; dispatching writes code. Asking for the second
// without saying what to run must fail loudly rather than silently watch.
func TestWatchRefusesDispatchWithNoCommand(t *testing.T) {
	cfg := firingConfig()
	cfg.DispatchCommand = nil
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	err := svc.Watch(context.Background(), WatchOptions{Dispatch: true, Once: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Errorf("err = %v, want a refusal naming the missing command", err)
	}
}
