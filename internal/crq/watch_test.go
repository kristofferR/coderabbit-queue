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
		t.Fatal("the session was given no findings file")
	}
	// OUTSIDE the worktree: at the repository root it is an untracked file, and
	// a session following the documented `git add -A` push would commit crq's
	// review payload into the PR.
	if strings.HasPrefix(got.Findings, got.Cwd) {
		t.Errorf("findings at %q are inside the worktree %q", got.Findings, got.Cwd)
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

// DryRun means crq writes nothing and posts nothing. Claiming shared state and
// running a code-writing command is the largest possible violation of that.
func TestDispatchHonoursDryRun(t *testing.T) {
	cfg := firingConfig()
	cfg.DryRun = true
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)

	ran := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, why := svc.dispatch(context.Background(), WatchOptions{Dispatch: true, Command: []string{script}},
		NextReport{Repo: "o/r", PR: 1, Head: "aaaaaaaa1", Action: "fix"})
	if ok {
		t.Error("a dry run dispatched a session")
	}
	if !strings.Contains(why, "dry run") {
		t.Errorf("reason = %q, want it to say why", why)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Error("the fix command ran under a dry run")
	}
}

// A session that fixes files without pushing must not have that work deleted:
// removing the worktree discards fixes that were made but not landed.
func TestDispatchKeepsAWorktreeWithUnpushedWork(t *testing.T) {
	base := t.TempDir()
	repo := "owner/thing"
	sha := originRepo(t, filepath.Join(base, repo))
	t.Setenv("CRQ_REMOTE_BASE", base)

	cfg := firingConfig()
	cfg.WorkspaceRoot = t.TempDir()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	gh.pulls[fakeKey(repo, 8)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 8, sha, PhaseQueued, time.Now().UTC(), 0)

	// A session that edits a file and stops there, which is the ordinary shape.
	script := filepath.Join(t.TempDir(), "fix.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho fixed >> README.md\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, why := svc.dispatch(context.Background(), WatchOptions{Dispatch: true, Command: []string{script}, MaxAttempts: 3},
		NextReport{Repo: repo, PR: 8, Head: sha, Action: "fix"})
	if !ok {
		t.Fatalf("dispatch failed: %s", why)
	}
	found := false
	_ = filepath.WalkDir(filepath.Join(cfg.WorkspaceRoot, "work"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "README.md" {
			if body, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(body), "fixed") {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Error("the session's uncommitted fix was deleted with the worktree")
	}
}

// With three dispatch slots and four PRs needing fixes, a fixed pass order gives
// the same three the slots every time and tells the fourth "at dispatch
// capacity" forever. One PR sat five hours that way while its findings grew from
// 15 to 25, so every PR has to reach the front eventually.
func TestWatchPassRotatesSoNoPRIsStarved(t *testing.T) {
	cfg := firingConfig()
	cfg.AllowRepos = map[string]bool{"o/r": true}
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	for _, pr := range []int{1, 2, 3, 4} {
		var p ghapi.Pull
		p.State = "open"
		p.Number = pr
		p.Head.SHA = "aaaaaaaa1"
		gh.pulls[fakeKey("o/r", pr)] = p
	}
	svc := NewService(cfg, gh, NewMemoryStore(cfg), nil)

	// Record the order each pass visits PRs in. Only the ORDER matters here, so
	// the pool takes every candidate.
	seen := map[int]int{}
	firstOf := []int{}
	for pass := 0; pass < 4; pass++ {
		var order []int
		err := svc.watchPass(context.Background(), WatchOptions{}, newDispatchPool(4), func(e WatchEvent) error {
			order = append(order, e.PR)
			seen[e.PR]++
			return nil
		})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if len(order) == 0 {
			t.Fatalf("pass %d visited nothing", pass)
		}
		firstOf = append(firstOf, order[0])
	}

	// Every PR was looked at every pass...
	for pr, n := range seen {
		if n != 4 {
			t.Errorf("pr %d seen %d times, want every pass", pr, n)
		}
	}
	// ...and the front of the queue moved, so the tail is not starved of slots.
	distinct := map[int]bool{}
	for _, pr := range firstOf {
		distinct[pr] = true
	}
	if len(distinct) < 2 {
		t.Errorf("the same PR led every pass (%v); the tail can never get a dispatch slot", firstOf)
	}
}
