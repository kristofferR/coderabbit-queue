package crq

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
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

	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{
		Dispatch: true, Command: []string{script}, MaxAttempts: 3,
	}, pool, report)
	if !ok {
		t.Fatalf("dispatch did not run: %s", why)
	}
	pool.wait()

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
	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{Dispatch: true, Command: []string{script}},
		pool, NextReport{Repo: "o/r", PR: 1, Head: "aaaaaaaa1", Action: "fix"})
	pool.wait()
	if ok {
		t.Error("a dry run dispatched a session")
	}
	if !strings.Contains(why, "dry run") {
		t.Errorf("reason = %q, want it to say why", why)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Error("the fix command ran under a dry run")
	}
	// Health is a shared CAS write like any other, so a dry run must not record
	// one — let alone raise a dispatcher alarm nothing caused.
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Drain != nil {
		t.Errorf("a dry run wrote dispatch health: %+v", st.Drain)
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
	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{Dispatch: true, Command: []string{script}, MaxAttempts: 3},
		pool, NextReport{Repo: repo, PR: 8, Head: sha, Action: "fix"})
	if !ok {
		t.Fatalf("dispatch failed: %s", why)
	}
	pool.wait()
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

// A session that commits its fixes but does not push them leaves a clean working
// tree, and deleting that worktree destroys the only copy of the fix.
func TestDispatchKeepsACommittedButUnpushedFix(t *testing.T) {
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
	gh.pulls[fakeKey(repo, 11)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 11, sha, PhaseQueued, time.Now().UTC(), 0)

	script := filepath.Join(t.TempDir(), "fix.sh")
	body := "#!/bin/sh\necho fixed >> README.md\n" +
		"git -c user.email=t@example.invalid -c user.name=t commit -qam 'fix the finding'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	pool := newDispatchPool(0)
	ok, why := svc.startDispatch(context.Background(), WatchOptions{Dispatch: true, Command: []string{script}, MaxAttempts: 3},
		pool, NextReport{Repo: repo, PR: 11, Head: sha, Action: "fix"})
	if !ok {
		t.Fatalf("dispatch failed: %s", why)
	}
	pool.wait()

	found := false
	_ = filepath.WalkDir(filepath.Join(cfg.WorkspaceRoot, "work"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "README.md" {
			if content, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(content), "fixed") {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Error("the session's committed but unpushed fix was deleted with the worktree")
	}
}

// A command that never reached a process did not use up the per-head budget: a
// mistyped fix agent would otherwise spend every attempt without a session ever
// running, and correcting it would come too late for that head.
func TestDispatchRefundsTheAttemptWhenTheCommandCannotStart(t *testing.T) {
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
	gh.pulls[fakeKey(repo, 6)] = pull
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	seedRound(t, store, cfg, repo, 6, sha, PhaseQueued, time.Now().UTC(), 0)

	pool := newDispatchPool(0)
	missing := filepath.Join(t.TempDir(), "no-such-agent")
	if ok, why := svc.startDispatch(context.Background(),
		WatchOptions{Dispatch: true, Command: []string{missing}, MaxAttempts: 3},
		pool, NextReport{Repo: repo, PR: 6, Head: sha, Action: "fix"}); !ok {
		t.Fatalf("the round was not claimed: %s", why)
	}
	pool.wait()

	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(repo, 6)
	if round == nil || round.Dispatch == nil {
		t.Fatalf("round = %#v, want the claim released", round)
	}
	if round.Dispatch.Attempts != 0 {
		t.Errorf("attempts = %d, want the budget intact for a session that never started", round.Dispatch.Attempts)
	}
}

// Findings on a head crq never queued — a review somebody triggered by hand, or
// feedback that predates the drain — used to be undispatchable forever, because
// `Next` returns fix before enqueueing and the claim had nowhere to live.
func TestClaimDispatchAdoptsAHeadTheQueueNeverSaw(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "aaaaaaaa1", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1"}},
	}

	if ok, why, _ := svc.claimDispatch(context.Background(), report, "tok", 3); !ok {
		t.Fatalf("claim refused: %s — these findings can never be drained", why)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(report.Repo, report.PR)
	if round == nil || round.Head != report.Head {
		t.Fatalf("round = %#v, want one tracking the observed head", round)
	}
	// Adopting the head must not buy a review: the findings are already in hand.
	if round.FireEligible(time.Now().UTC()) {
		t.Error("the adopted round is fire-eligible; dispatching would cost an account-metered review")
	}
	if !round.DispatchHeld(time.Now().UTC()) {
		t.Errorf("dispatch = %#v, want the claim held", round.Dispatch)
	}
}

// Feedback carried from an older commit is not evidence that anybody reviewed
// the current head. Marking it reviewed to hold the claim left a completed round
// for a head no reviewer had looked at: the review is deduped away and the
// caller waits for one that can no longer be requested.
func TestClaimDispatchDoesNotMarkACarriedHeadReviewed(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "bbbbbbbb2", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1", ThreadID: "PRRT_1"}},
	}

	if ok, why, _ := svc.claimDispatch(context.Background(), report, "tok", 3); !ok {
		t.Fatalf("claim refused: %s", why)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(report.Repo, report.PR)
	if round == nil || round.Phase == PhaseCompleted {
		t.Fatalf("round = %#v, want one that still needs a review of this head", round)
	}
	if !round.DispatchHeld(time.Now().UTC()) {
		t.Errorf("dispatch = %#v, want the claim held", round.Dispatch)
	}
}

// A head that moved on while its previous round still stood: `Next` reports fix
// without enqueueing, so nothing else supersedes the stale round. Refusing the
// claim over the mismatch left the new head's findings undispatchable on every
// pass — and counted each refusal as the dispatcher failing.
func TestClaimDispatchSupersedesAStaleRound(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	seedRound(t, store, cfg, "owner/thing", 12, "aaaaaaaa1", PhaseCompleted, time.Now().UTC(), 0)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "bbbbbbbb2", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "bbbbbbbb2"}},
	}

	ok, why, _ := svc.claimDispatch(context.Background(), report, "tok", 3)
	if !ok {
		t.Fatalf("claim refused: %s — the new head's findings can never be drained", why)
	}
	st, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	round := st.Round(report.Repo, report.PR)
	if round == nil || round.Head != report.Head {
		t.Fatalf("round = %#v, want the stale one superseded by the observed head", round)
	}

	// A session already running for an earlier head keeps the PR to itself: its
	// own push is what moved the head, and it is still landing that work.
	live := NextReport{Repo: report.Repo, PR: report.PR, Head: "cccccccc3", Action: "fix"}
	if ok, why, byDesign := svc.claimDispatch(context.Background(), live, "tok2", 3); ok {
		t.Error("a second session was started against a PR somebody is already fixing")
	} else if !byDesign {
		t.Errorf("reason %q counted as a dispatcher failure", why)
	}
}

// The attempt bound is crq obeying its own configuration, not fix sessions
// failing to start. Counted as drain health, a correctly bounded head raised the
// "fix sessions are not starting" alert after three passes — and every pass
// after that, forever.
func TestExhaustedAttemptsAreNotADispatcherFailure(t *testing.T) {
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	report := NextReport{
		Repo: "owner/thing", PR: 12, Head: "aaaaaaaa1", Action: "fix",
		Findings: []dialect.Finding{{ID: "f1", Commit: "aaaaaaaa1"}},
	}

	for attempt := 1; attempt <= 2; attempt++ {
		ok, why, _ := svc.claimDispatch(context.Background(), report, fmt.Sprintf("tok%d", attempt), 2)
		if !ok {
			t.Fatalf("attempt %d refused: %s", attempt, why)
		}
		svc.releaseDispatch(context.Background(), report, fmt.Sprintf("tok%d", attempt), true)
	}
	ok, why, byDesign := svc.claimDispatch(context.Background(), report, "tok3", 2)
	if ok {
		t.Fatal("the attempt bound let a third dispatch through")
	}
	if !byDesign {
		t.Errorf("reason %q counted as a dispatcher failure; the bound is the point", why)
	}
}

// The prune runs before the new log is created, so it has to leave room for it —
// otherwise the steady state is one file per PR above the bound.
func TestSessionLogPruneLeavesRoomForTheNewLog(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("7-abcdef123-2026010%dT000000.log", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneSessionLogs(dir, 7)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Errorf("kept %d logs, want 4 so the one about to be written makes 5", len(entries))
	}
	// The ones kept are the newest.
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "7-abcdef123-20260103") {
			t.Errorf("%s was kept over a newer log", e.Name())
		}
	}
}
