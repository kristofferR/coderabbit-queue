package crq

import (
	"context"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// The race crq hold exists to close is between selecting a round and writing the
// reservation. Checking the hold only at selection leaves exactly that window
// open, so the command could return successfully while a daemon fired anyway.
func TestHoldIsRecheckedWhenTheRoundIsReserved(t *testing.T) {
	ctx := context.Background()
	cfg := firingConfig()
	gh := newFakeGitHub()
	gh.graphQL = noForcePush
	repo, pr, head := "o/r", 3, "aaaaaaaa1"

	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = head
	gh.pulls[fakeKey(repo, pr)] = pull

	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	now := time.Now().UTC()
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now, 0)

	// The hold lands after the round was chosen, which is the whole point.
	round := func() Round {
		st, _, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return *st.Round(repo, pr)
	}()
	if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err != nil {
		t.Fatal(err)
	}

	obs := engine.Observation{Open: true, Head: head}
	res, err := svc.fireRound(ctx, round, obs, true, 0, time.Time{}, "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, posted := range gh.posted {
		if posted == cfg.ReviewCommand {
			t.Fatalf("a held PR was fired anyway (%s)", res.Action)
		}
	}
	st, _, _ := store.Load(ctx)
	if st.FireSlot != nil {
		t.Errorf("a held round took the fire slot: %#v", st.FireSlot)
	}
}
