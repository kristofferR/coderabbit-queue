package engine

import (
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

var (
	t0     = time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	policy = Policy{
		Bot:               "coderabbitai[bot]",
		RequiredBots:      []string{"coderabbitai[bot]"},
		MinInterval:       90 * time.Second,
		InflightTimeout:   15 * time.Minute,
		RateLimitFallback: 15 * time.Minute,
		RetryBackoff:      5 * time.Minute,
	}
)

func firedRound(t *testing.T, head string) state.Round {
	t.Helper()
	s := state.New()
	r, err := s.NewRound("owner/repo", 448, head, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("tok", "host", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.Fire(1001, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return *r
}

func rateLimitEvent(id int64, at time.Time, window *time.Time) dialect.BotEvent {
	return dialect.BotEvent{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: id, CreatedAt: at, UpdatedAt: at, AutoReply: true, Window: window,
	}
}

// TestRateLimitedRoundParksAndHoldsWindow is the #448 scenario at engine
// level: a fired head that comes back rate limited must park with a real
// window, and re-observing the SAME edited comment must not extend it.
func TestRateLimitedRoundParksAndHoldsWindow(t *testing.T) {
	r := firedRound(t, "a21da4aeb")
	window := t0.Add(40 * time.Minute)
	obs := Observation{Head: "a21da4aeb", Open: true,
		Events: []dialect.BotEvent{rateLimitEvent(555, t0.Add(10*time.Second), &window)}}

	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(window) {
		t.Fatalf("want retry at the parsed window, got %+v", tr)
	}
	if tr.Blocked == nil || tr.Blocked.CommentID != 555 {
		t.Fatalf("must record the rate-limit comment identity, got %+v", tr.Blocked)
	}

	// Apply: the round parks. It is not fire-eligible before the window.
	if err := r.AwaitRetry(tr.RetryAt, tr.Reason, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r.FireEligible(window.Add(-time.Second)) {
		t.Fatal("round must stay parked inside the block window")
	}

	// The daemon re-observes the SAME comment (edited in place, later
	// UpdatedAt, later parse base → later window). The standing block wins.
	quota := state.AccountQuota{RLCommentID: 555, BlockedUntil: &window}
	later := t0.Add(5 * time.Minute)
	laterWindow := later.Add(40 * time.Minute)
	obs2 := Observation{Head: "a21da4aeb", Open: true,
		Events: []dialect.BotEvent{rateLimitEvent(555, later, &laterWindow)}}
	r2 := firedRound(t, "a21da4aeb")
	tr2 := Progress(r2, quota, obs2, later, policy)
	if tr2.Outcome != OutRetry || !tr2.RetryAt.Equal(window) {
		t.Fatalf("re-observation must reuse the standing window %v, got %+v", window, tr2)
	}
}

func TestUnparseableRateLimitFallsBackConservatively(t *testing.T) {
	r := firedRound(t, "a21da4aeb")
	now := t0.Add(time.Minute)
	obs := Observation{Head: "a21da4aeb", Open: true,
		Events: []dialect.BotEvent{rateLimitEvent(555, t0.Add(10*time.Second), nil)}}
	tr := Progress(r, state.AccountQuota{}, obs, now, policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("want the 15m fallback window, got %+v", tr)
	}
}

// TestInstantCompletionReplyDoesNotConverge encodes the 865ef40 fix: a
// "Review finished" ack on the FIRST-ever command (no prior submitted
// review) must not complete the round.
func TestInstantCompletionReplyDoesNotConverge(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{
		{Kind: dialect.EvCommand, Bot: "kristofferR", CommentID: 1001, CreatedAt: t0.Add(2 * time.Second), UpdatedAt: t0.Add(2 * time.Second)},
		{Kind: dialect.EvCompletion, Bot: "coderabbitai[bot]", CommentID: 1002, AutoReply: true, CreatedAt: t0.Add(7 * time.Second), UpdatedAt: t0.Add(7 * time.Second)},
	}}
	if got := Completion(r, obs, policy); got.Done {
		t.Fatalf("instant ack with no prior review must not converge: %+v", got)
	}
	// With a prior review on an older commit, the same ack DOES stand in for a
	// no-findings re-review.
	obs.Reviews = []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "000011122", SubmittedAt: t0.Add(-time.Hour)}}
	if got := Completion(r, obs, policy); !got.Done {
		t.Fatalf("re-review completion reply must converge: %+v", got)
	}
}

// TestProcessingSummaryBlocksCompletion encodes the c22eb4b fix, now applied
// on the daemon path too: while the in-place-edited top summary says the
// review is processing, a completion reply must not converge or complete.
func TestProcessingSummaryBlocksCompletion(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "000011122", SubmittedAt: t0.Add(-time.Hour)}},
		Events: []dialect.BotEvent{
			{Kind: dialect.EvCommand, Bot: "kristofferR", CommentID: 1001, CreatedAt: t0.Add(2 * time.Second), UpdatedAt: t0.Add(2 * time.Second)},
			{Kind: dialect.EvCompletion, Bot: "coderabbitai[bot]", CommentID: 1002, AutoReply: true, CreatedAt: t0.Add(7 * time.Second), UpdatedAt: t0.Add(7 * time.Second)},
			{Kind: dialect.EvInProgress, Bot: "coderabbitai[bot]", CommentID: 900, CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(8 * time.Second)},
		}}
	if got := Completion(r, obs, policy); got.Done {
		t.Fatalf("processing summary must block convergence: %+v", got)
	}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutReviewing {
		t.Fatalf("daemon path should release the slot but keep the round open, got %+v", tr)
	}
}

// TestFailedSummaryParksTheRound encodes the e2aa2f0 fix on the daemon path:
// a failed review must not complete the round, and retries after a cooldown.
func TestFailedSummaryParksTheRound(t *testing.T) {
	r := firedRound(t, "abcdef123")
	now := t0.Add(time.Minute)
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "000011122", SubmittedAt: t0.Add(-time.Hour)}},
		Events: []dialect.BotEvent{
			{Kind: dialect.EvFailed, Bot: "coderabbitai[bot]", CommentID: 900, CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(9 * time.Second)},
		}}
	tr := Progress(r, state.AccountQuota{}, obs, now, policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("failed review must park with backoff, got %+v", tr)
	}
}

func TestReviewAtHeadCompletesRound(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "abcdef1234567890", SubmittedAt: t0.Add(3 * time.Minute)}}}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(4*time.Minute), policy)
	if tr.Outcome != OutComplete {
		t.Fatalf("review at head must complete, got %+v", tr)
	}
	// A review of a DIFFERENT commit must not.
	obs.Reviews[0].Commit = "999888777"
	tr = Progress(r, state.AccountQuota{}, obs, t0.Add(4*time.Minute), policy)
	if tr.Outcome == OutComplete {
		t.Fatalf("review of another head must not complete, got %+v", tr)
	}
}

func TestInflightTimeoutCarriesCooldown(t *testing.T) {
	r := firedRound(t, "abcdef123")
	now := t0.Add(16 * time.Minute)
	tr := Progress(r, state.AccountQuota{}, Observation{Head: "abcdef123", Open: true}, now, policy)
	if tr.Outcome != OutRetry || !tr.RetryAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("timeout must park with a cooldown (v2 had none — re-fire vector), got %+v", tr)
	}
}

func TestDecideFireGuards(t *testing.T) {
	free := Global{SlotFree: true}
	now := t0.Add(10 * time.Minute)

	queued := state.Round{Repo: "owner/repo", PR: 448, Head: "abcdef123", Phase: state.PhaseQueued, Seq: 1}
	open := Observation{Head: "abcdef123", Open: true}

	if d := DecideFire(free, queued, Observation{Head: "abcdef123", Open: false}, now, policy); d.Verdict != FireDrop {
		t.Fatalf("closed PR must drop, got %+v", d)
	}
	if d := DecideFire(free, queued, Observation{Head: "999888777", Open: true}, now, policy); d.Verdict != FireSupersede {
		t.Fatalf("moved head must supersede, got %+v", d)
	}
	fired := firedRound(t, "abcdef123")
	if d := DecideFire(free, fired, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("a fired round must never fire again, got %+v", d)
	}
	if d := DecideFire(Global{SlotFree: false}, queued, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("busy slot must block, got %+v", d)
	}
	blocked := now.Add(10 * time.Minute)
	if d := DecideFire(Global{SlotFree: true, BlockedUntil: &blocked}, queued, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("account block must block, got %+v", d)
	}
	last := now.Add(-time.Second)
	if d := DecideFire(Global{SlotFree: true, LastFired: &last}, queued, open, now, policy); d.Verdict != FireNo {
		t.Fatalf("min interval must block, got %+v", d)
	}
	reviewed := Observation{Head: "abcdef123", Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai", Commit: "abcdef1234567890", SubmittedAt: now}}}
	if d := DecideFire(free, queued, reviewed, now, policy); d.Verdict != FireDedupe {
		t.Fatalf("already-reviewed head must dedupe, got %+v", d)
	}
	withCommand := Observation{Head: "abcdef123", Open: true,
		Commands: []CommandSeen{{ID: 77, CreatedAt: now.Add(-time.Minute)}}}
	if d := DecideFire(free, queued, withCommand, now, policy); d.Verdict != FireAdopt || d.AdoptCommandID != 77 {
		t.Fatalf("existing command must be adopted, got %+v", d)
	}
	if d := DecideFire(free, queued, open, now, policy); d.Verdict != FirePost {
		t.Fatalf("clean queued round must post, got %+v", d)
	}

	// A parked round becomes fire-eligible only after RetryAt.
	parked := firedRound(t, "abcdef123")
	retryAt := now.Add(15 * time.Minute)
	if err := parked.AwaitRetry(retryAt, "rate limited", now); err != nil {
		t.Fatal(err)
	}
	if d := DecideFire(free, parked, open, retryAt.Add(-time.Second), policy); d.Verdict != FireNo {
		t.Fatalf("parked round must not fire before RetryAt, got %+v", d)
	}
	if d := DecideFire(free, parked, open, retryAt, policy); d.Verdict != FirePost {
		t.Fatalf("parked round must fire once RetryAt passes, got %+v", d)
	}
}

// TestBareReactionReleasesSlotButKeepsRoundOpen ports v2's doneBotReacted: a
// reaction on the fired command acknowledges it, releasing the slot while the
// review keeps running.
func TestBareReactionReleasesSlotButKeepsRoundOpen(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true, Reacted: true}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutReviewing {
		t.Fatalf("a bare reaction must release the slot and keep the round open, got %+v", tr)
	}
}

// TestReviewsPausedNoteIsNotAck ports v2: the auto-pause note is a bot comment
// but not an acknowledgement of the fired command, so the round keeps waiting.
func TestReviewsPausedNoteIsNotAck(t *testing.T) {
	r := firedRound(t, "abcdef123")
	paused := dialect.BotEvent{Kind: dialect.EvPaused, Bot: "coderabbitai[bot]", CommentID: 900,
		CreatedAt: t0.Add(10 * time.Second), UpdatedAt: t0.Add(10 * time.Second)}
	obs := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{paused}}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != KeepWaiting {
		t.Fatalf("a reviews-paused note must not acknowledge or complete the round, got %+v", tr)
	}
}

// TestRateLimitBeatsAlreadyReviewedAck encodes the carrier#82 incident: a
// rate-limit notice plus an "already reviewed" claim, with no review object,
// must park the round (retry later), never complete it.
func TestRateLimitBeatsAlreadyReviewedAck(t *testing.T) {
	r := firedRound(t, "a0646f010")
	window := t0.Add(40 * time.Minute)
	obs := Observation{Head: "a0646f010", Open: true, Events: []dialect.BotEvent{
		rateLimitEvent(501, t0.Add(10*time.Second), &window),
		{Kind: dialect.EvAlreadyReviewed, Bot: "coderabbitai[bot]", CommentID: 502, CreatedAt: t0.Add(10 * time.Second), UpdatedAt: t0.Add(10 * time.Second)},
	}}
	tr := Progress(r, state.AccountQuota{}, obs, t0.Add(time.Minute), policy)
	if tr.Outcome != OutRetry || tr.Blocked == nil {
		t.Fatalf("an unproven already-reviewed ack must yield to the rate limit, got %+v", tr)
	}
}

// TestPreFireReviewOfHeadCompletes ports botsReviewedHead: a required bot's
// review of the head counts even when it landed before the round was fired.
func TestPreFireReviewOfHeadCompletes(t *testing.T) {
	r := firedRound(t, "abcdef123")
	obs := Observation{Head: "abcdef123", Open: true, Reviews: []ReviewSeen{
		{Bot: "coderabbitai[bot]", ReviewID: 9, Commit: "abcdef1234567890", SubmittedAt: t0.Add(-10 * time.Minute)},
	}}
	if got := Completion(r, obs, policy); !got.Done {
		t.Fatalf("a required bot's pre-fire review of the head must complete the round: %+v", got)
	}
}

// TestCodexGatesCleanSummary ports the codexInactiveOrThumbed rules.
func TestCodexGatesCleanSummary(t *testing.T) {
	r := firedRound(t, "abcdef123")
	noAction := dialect.BotEvent{Kind: dialect.EvNoAction, Bot: "coderabbitai[bot]", CommentID: 2000,
		CreatedAt: t0.Add(30 * time.Second), UpdatedAt: t0.Add(30 * time.Second)}

	// Codex inactive: the clean summary converges alone.
	if got := Completion(r, Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{noAction}}, policy); !got.Done {
		t.Fatalf("codex-inactive clean summary must converge: %+v", got)
	}

	// Codex active in the round (a real Codex comment) without its review or
	// thumbs-up: the summary must NOT converge.
	codexComment := dialect.BotEvent{Kind: dialect.EvOther, Bot: "chatgpt-codex-connector[bot]", CommentID: 2001,
		CreatedAt: t0.Add(20 * time.Second), UpdatedAt: t0.Add(20 * time.Second)}
	obs := Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{noAction, codexComment}}
	if got := Completion(r, obs, policy); got.Done {
		t.Fatalf("active codex without review must block: %+v", got)
	}

	// A thumbs-up unblocks it.
	obs.CodexThumbsUp = true
	if got := Completion(r, obs, policy); !got.Done {
		t.Fatalf("codex thumbs-up must unblock: %+v", got)
	}

	// A Codex clean summary naming the head counts as Codex's review — and if
	// Codex gates the round, flips its ReviewedBy too.
	gated := policy
	gated.RequiredBots = []string{"coderabbitai[bot]", "chatgpt-codex-connector[bot]"}
	codexClean := dialect.BotEvent{Kind: dialect.EvCodexClean, Bot: "chatgpt-codex-connector[bot]", SHA: "abcdef1234",
		CommentID: 2002, CreatedAt: t0.Add(40 * time.Second), UpdatedAt: t0.Add(40 * time.Second)}
	got := Completion(r, Observation{Head: "abcdef123", Open: true, Events: []dialect.BotEvent{noAction, codexClean}}, gated)
	if !got.Done {
		t.Fatalf("codex clean summary at head must complete the gated round: %+v", got)
	}
}
