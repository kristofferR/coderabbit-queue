package crq

import (
	"context"
	"testing"
	"time"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// Tidy deletes, so what it must NOT touch matters more than what it does. A PR
// with a spent command from a superseded round, the live round's own command,
// and a person's request to review.
func TestTidyRemovesOnlySpentCommandsCrqPosted(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	cfg.Tidy = true
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 12
	head := "bbbbbbbb2"

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits[head] = commitAt(now.Add(-10 * time.Minute))

	add := func(id int64, login, body string, at time.Time) {
		c := ghapi.IssueComment{ID: id, Body: body, CreatedAt: at, UpdatedAt: at}
		c.User.Login = login
		gh.comments[fakeKey(repo, pr)] = append(gh.comments[fakeKey(repo, pr)], c)
	}
	add(100, "kristofferR", cfg.ReviewCommand, now.Add(-2*time.Hour)) // spent: old round
	add(200, "kristofferR", cfg.ReviewCommand, now.Add(-time.Minute)) // the live round's
	// A person's request that a round ADOPTED as its own fire. The round records
	// it in exactly the same CommandID as one crq posted, so only "did crq write
	// it" keeps this from being deleted out from under whoever asked.
	add(300, "someone-else", cfg.ReviewCommand, now.Add(-4*time.Hour))
	// The bot's own answer. It must survive: an auto-reply can be a rate-limit or
	// skip notice, which crq reads as evidence and surfaces as a finding.
	add(400, cfg.Bot, "<!-- auto-generated reply by CodeRabbit -->\nReview in progress", now.Add(-2*time.Hour))

	// The bot reviewed after the old command, which is the evidence that it read
	// it — and reviewed at the CURRENT head, so the round is answered.
	review := ghapi.Review{ID: 900, CommitID: head, State: "COMMENTED", SubmittedAt: now.Add(-90 * time.Minute), Body: "looks fine"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)

	// Three rounds: one that adopted the person's command, one holding the spent
	// command crq posted, and the live round holding 200.
	if _, err := store.Update(ctx, func(st *State) error {
		adopting, err := st.NewRound(repo, pr, "aaaaaaaa0", now.Add(-5*time.Hour))
		if err != nil {
			return err
		}
		if err := adopting.Fire(300, now.Add(-4*time.Hour)); err != nil {
			return err
		}
		st.PutRound(*adopting)
		if _, err := st.Supersede(repo, pr, "aaaaaaaa1", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		old := st.Round(repo, pr)
		if err := old.Reserve("t", "h", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		if err := old.Fire(100, now.Add(-2*time.Hour)); err != nil {
			return err
		}
		old.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
		st.PutRound(*old)
		if _, err := st.Supersede(repo, pr, head, now.Add(-time.Hour)); err != nil {
			return err
		}
		live := st.Round(repo, pr)
		if err := live.Reserve("t2", "h", now.Add(-2*time.Minute)); err != nil {
			return err
		}
		if err := live.Fire(200, now.Add(-time.Minute)); err != nil {
			return err
		}
		live.RecordPosted(cfg.Bot, 200, now.Add(-time.Minute))
		st.PutRound(*live)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != 100 {
		t.Fatalf("deleted = %v, want only the spent command from the superseded round", result.Deleted)
	}

	left := map[int64]bool{}
	for _, c := range gh.comments[fakeKey(repo, pr)] {
		left[c.ID] = true
	}
	if !left[200] {
		t.Error("the live round's command was deleted; the next pump will post a duplicate")
	}
	if !left[300] {
		t.Error("a person's review request was deleted; adopting one is not writing it")
	}
	if !left[400] {
		t.Error("a bot comment was deleted; those carry rate-limit and skip notices crq reads as findings")
	}
}

// Dry run must report the same decision and change nothing.
func TestTidyDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 13

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))

	c := ghapi.IssueComment{ID: 100, Body: cfg.ReviewCommand, CreatedAt: now.Add(-2 * time.Hour)}
	c.User.Login = "kristofferR"
	gh.comments[fakeKey(repo, pr)] = []ghapi.IssueComment{c}
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", SubmittedAt: now.Add(-90 * time.Minute), State: "COMMENTED"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	if _, err := store.Update(ctx, func(st *State) error {
		r, err := st.NewRound(repo, pr, "aaaaaaaa1", now.Add(-3*time.Hour))
		if err != nil {
			return err
		}
		if err := r.Reserve("t", "h", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
			return err
		}
		r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
		if err := r.Complete(); err != nil {
			return err
		}
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || len(result.Deleted) != 1 {
		t.Fatalf("result = %+v, want the same decision, reported not applied", result)
	}
	if len(gh.comments[fakeKey(repo, pr)]) != 1 {
		t.Error("a dry run deleted a comment")
	}
}

// A command crq deleted stays recorded on its round for ever, so a pass that
// did not check what is still on the PR would DELETE it again every time and
// read the 404 back as a fresh removal.
func TestTidySkipsCommandsAlreadyGone(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	now := time.Now().UTC()
	repo, pr := "o/r", 14

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = "bbbbbbbb2"
	gh.pulls[fakeKey(repo, pr)] = pull
	gh.commits["bbbbbbbb2"] = commitAt(now.Add(-10 * time.Minute))
	review := ghapi.Review{ID: 900, CommitID: "bbbbbbbb2", SubmittedAt: now.Add(-90 * time.Minute), State: "COMMENTED"}
	review.User.Login = cfg.Bot
	gh.reviews[fakeKey(repo, pr)] = []ghapi.Review{review}

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	// The round remembers posting command 100; the PR no longer has it.
	if _, err := store.Update(ctx, func(st *State) error {
		r, err := st.NewRound(repo, pr, "aaaaaaaa1", now.Add(-3*time.Hour))
		if err != nil {
			return err
		}
		if err := r.Reserve("t", "h", now.Add(-3*time.Hour)); err != nil {
			return err
		}
		if err := r.Fire(100, now.Add(-2*time.Hour)); err != nil {
			return err
		}
		r.RecordPosted(cfg.Bot, 100, now.Add(-2*time.Hour))
		if err := r.Complete(); err != nil {
			return err
		}
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Tidy(ctx, repo, pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 {
		t.Errorf("deleted = %v, want nothing: the comment is already gone", result.Deleted)
	}
	if len(gh.deleted) != 0 {
		t.Errorf("issued %d delete(s) for a comment that is not on the pr", len(gh.deleted))
	}
}

// commitAt is a commit object with a committer date, which is the cutoff
// adoption uses and therefore the cutoff tidying respects.
func commitAt(at time.Time) ghapi.Commit {
	var c ghapi.Commit
	c.Committer.Date = at
	return c
}
