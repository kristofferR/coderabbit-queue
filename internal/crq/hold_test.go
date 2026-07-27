package crq

import (
	"context"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/engine"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

func setHoldCapableLeader(t *testing.T, ctx context.Context, store StateStore, now time.Time) {
	t.Helper()
	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{
			Owner:        "current-daemon",
			Token:        "leader",
			ExpiresAt:    now.Add(time.Hour),
			UpdatedAt:    now,
			Capabilities: []string{leaderCapabilityHolds},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

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
	setHoldCapableLeader(t, ctx, store, now)

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

// Quota-free co-review paths bypass NextEligible, so each one must repeat the
// hold check in the CAS that claims its trigger post.
func TestHoldIsRecheckedByQuotaFreeFirePaths(t *testing.T) {
	tests := map[string]func(*Service, context.Context, Round, time.Time) error{
		"co-only": func(s *Service, ctx context.Context, round Round, now time.Time) error {
			_, err := s.fireCoOnly(ctx, round, []string{dialect.CodexBotLogin}, "primary already reviewed", now)
			return err
		},
		"co-deferred": func(s *Service, ctx context.Context, round Round, now time.Time) error {
			_, err := s.fireCoDeferred(ctx, round, engine.FireDecision{
				Verdict: engine.FireCoDeferred,
				PostCo:  []string{dialect.CodexBotLogin},
				Reason:  "primary account blocked",
			}, now)
			return err
		},
		"co-review-wait": func(s *Service, ctx context.Context, round Round, now time.Time) error {
			_, err := s.fireCoReviewWait(ctx, round, engine.Observation{
				Open:   true,
				Head:   round.Head,
				HeadAt: now.Add(-time.Minute),
			}, "waiting for automatic co-review", now)
			return err
		},
	}

	for name, fire := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
			cfg := firingConfig()
			cfg.RequiredBots = append(cfg.RequiredBots, dialect.CodexBotLogin)
			cfg.CoBots = codexCoBots(cfg.RequiredBots)
			gh := newFakeGitHub()
			store := NewMemoryStore(cfg)
			svc := NewService(cfg, gh, store, nil)
			svc.now = func() time.Time { return now }
			repo, pr, head := "o/r", 4, "bbbbbbbb1"
			seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
			setHoldCapableLeader(t, ctx, store, now)

			st, _, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			round := *st.Round(repo, pr)
			if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err != nil {
				t.Fatal(err)
			}
			if err := fire(svc, ctx, round, now); err != nil {
				t.Fatal(err)
			}
			if len(gh.posted) != 0 {
				t.Fatalf("a held PR received a co-review trigger: %v", gh.posted)
			}
			st, _, err = store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := st.Round(repo, pr)
			if got == nil || got.Phase != PhaseQueued || got.Co(dialect.CodexBotLogin).ClaimedAt != nil {
				t.Fatalf("a held round was mutated by %s: %+v", name, got)
			}
		})
	}
}

func TestHoldStopsInflightCoReviewerSelfHeal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	cfg.RequiredBots = append(cfg.RequiredBots, dialect.CodexBotLogin)
	cfg.CoBots = codexCoBots(cfg.RequiredBots)
	gh := newFakeGitHub()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, gh, store, nil)
	svc.now = func() time.Time { return now }
	repo, pr, head := "o/r", 6, "cccccccc1"
	seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
	setHoldCapableLeader(t, ctx, store, now)
	if _, err := store.Update(ctx, func(st *State) error {
		r := st.Round(repo, pr)
		if err := r.Reserve("token", cfg.Host, now.Add(-time.Minute)); err != nil {
			return err
		}
		if err := r.Fire(123, now.Add(-time.Minute)); err != nil {
			return err
		}
		st.PutRound(*r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round := *st.Round(repo, pr)
	if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err != nil {
		t.Fatal(err)
	}

	svc.selfHealCoReviewers(ctx, round, engine.Observation{Open: true, Head: head}, now)
	if len(gh.posted) != 0 {
		t.Fatalf("held in-flight round received a co-review trigger: %v", gh.posted)
	}
	st, _, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim := st.Round(repo, pr).Co(dialect.CodexBotLogin).ClaimedAt; claim != nil {
		t.Fatalf("held in-flight round acquired a co-review claim: %s", claim)
	}
}

func TestHoldRejectsTriggerClaimsAlreadyBeingPosted(t *testing.T) {
	tests := map[string]func(*State, *Round, Config, time.Time) error{
		"primary reservation": func(st *State, r *Round, cfg Config, now time.Time) error {
			if err := r.Reserve("token", cfg.Host, now); err != nil {
				return err
			}
			st.FireSlot = &FireSlot{Key: QueueKey(r.Repo, r.PR), Token: "token", Since: now}
			return nil
		},
		"co-reviewer claim": func(_ *State, r *Round, _ Config, now time.Time) error {
			r.ClaimCo(dialect.CodexBotLogin, now)
			return nil
		},
	}

	for name, claim := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
			cfg := firingConfig()
			store := NewMemoryStore(cfg)
			svc := NewService(cfg, newFakeGitHub(), store, nil)
			svc.now = func() time.Time { return now }
			repo, pr, head := "o/r", 7, "dddddddd1"
			seedRound(t, store, cfg, repo, pr, head, PhaseQueued, now.Add(-time.Minute), 0)
			setHoldCapableLeader(t, ctx, store, now)
			if _, err := store.Update(ctx, func(st *State) error {
				r := st.Round(repo, pr)
				if err := claim(st, r, cfg, now); err != nil {
					return err
				}
				st.PutRound(*r)
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := svc.Hold(ctx, repo, pr, "waiting on a decision"); err == nil {
				t.Fatal("hold succeeded while a trigger post was already claimed")
			}
			st, _, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, held := st.HeldPR(repo, pr); held {
				t.Fatal("a rejected hold was persisted")
			}
		})
	}
}

func TestHoldAndUnholdDryRunDoNotMutateState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	cfg.DryRun = true
	store := NewMemoryStore(cfg)
	if _, err := store.Update(ctx, func(st *State) error {
		st.Hold("o/held", 1, "existing hold", "operator", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, revision, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	held, err := svc.Hold(ctx, "o/new", 2, "simulated hold")
	if err != nil {
		t.Fatal(err)
	}
	if !held.Held || held.Reason != "simulated hold" {
		t.Fatalf("unexpected simulated hold result: %+v", held)
	}
	released, err := svc.Unhold(ctx, "o/held", 1)
	if err != nil {
		t.Fatal(err)
	}
	if released.Held {
		t.Fatalf("unexpected simulated unhold result: %+v", released)
	}

	after, afterRevision, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterRevision != revision {
		t.Fatalf("dry-run changed state revision: before=%+v after=%+v", revision, afterRevision)
	}
	if len(after.Holds) != len(before.Holds) {
		t.Fatalf("dry-run changed holds: before=%+v after=%+v", before.Holds, after.Holds)
	}
	if _, held := after.HeldPR("o/held", 1); !held {
		t.Fatal("dry-run unhold removed the existing hold")
	}
	if _, held := after.HeldPR("o/new", 2); held {
		t.Fatal("dry-run hold persisted the simulated hold")
	}
}

// A rolling deployment can leave an older daemon holding the leader lease.
// Such a daemon preserves Holds in JSON but does not enforce them, so the
// command must not claim success until a capable leader owns the fleet.
func TestHoldRequiresACapableLiveLeader(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err == nil {
		t.Fatal("hold succeeded without a live capable daemon")
	}
	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{
			Owner:     "old-daemon",
			Token:     "old",
			ExpiresAt: now.Add(time.Minute),
			UpdatedAt: now,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err == nil {
		t.Fatal("hold succeeded while an incompatible daemon owned the fleet")
	}
	st, _, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := st.HeldPR("o/r", 5); held {
		t.Fatal("a rejected hold was persisted")
	}

	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader.Capabilities = []string{leaderCapabilityHolds}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err != nil {
		t.Fatalf("hold with capable leader: %v", err)
	}
}

func TestHoldAcceptsTopLevelCapabilityPreservedByOldWriter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	svc.now = func() time.Time { return now }

	if _, err := store.Update(ctx, func(st *State) error {
		st.Leader = &LeaderLease{
			Owner:     "new-daemon",
			Token:     "current-token",
			ExpiresAt: now.Add(time.Minute),
			UpdatedAt: now,
		}
		st.LeaderCapabilities = &LeaderCapabilityLease{
			Token:        "current-token",
			Capabilities: []string{leaderCapabilityHolds},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err != nil {
		t.Fatalf("hold with preserved top-level capability: %v", err)
	}

	if _, err := store.Update(ctx, func(st *State) error {
		st.Unhold("o/r", 5)
		st.Leader.Token = "old-daemon-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hold(ctx, "o/r", 5, "waiting on a decision"); err == nil {
		t.Fatal("stale capabilities from a different lease token were accepted")
	}
}
