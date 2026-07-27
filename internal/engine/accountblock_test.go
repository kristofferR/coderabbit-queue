package engine

import (
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// The account allowance is not a property of a round. Progress only derived the
// window for a round that still existed and fired before the notice, and neither
// survives a fix session's push: the head moves, the round is superseded, and
// the rate-limit reply is archived unread. crq then believed the account was
// free and posted the command again minutes after being told to wait.
func TestObservedAccountBlockSurvivesASupersededRound(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	window := now.Add(38 * time.Minute)

	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 900, UpdatedAt: now.Add(-4 * time.Minute), Window: &window,
	}}}

	// No round is involved at all — the one that asked has been superseded away.
	blk := ObservedAccountBlock(obs, p, state.AccountQuota{}, now)
	if blk == nil {
		t.Fatal("a rate-limit notice must count even with no round to attach it to")
	}
	if !blk.Until.Equal(window) {
		t.Errorf("until = %s, want the window the bot stated (%s)", blk.Until, window)
	}
}

// CodeRabbit rewrites its notice in place as the window counts down. Treating
// each edit as new evidence renews the block forever from a message crq has
// already accounted for — and the standing window would never be allowed to end.
func TestObservedAccountBlockIgnoresAnEditedNotice(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	recorded := now.Add(-30 * time.Minute)

	q := state.AccountQuota{RLCommentID: 900, RLCommentUpdated: &recorded}
	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 900, UpdatedAt: now, // edited just now
	}}}

	if blk := ObservedAccountBlock(obs, p, q, now); blk != nil {
		t.Errorf("an edit of the recorded notice created a new block until %s", blk.Until)
	}

	// A DIFFERENT notice is new evidence and does block.
	obs.Events[0].CommentID = 901
	if blk := ObservedAccountBlock(obs, p, q, now); blk == nil {
		t.Error("a new rate-limit notice must block the account")
	}
}

// Only the configured primary meters the account; a co-reviewer saying it is
// busy is not an account block.
func TestObservedAccountBlockIgnoresOtherBots(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: dialect.CodexBotLogin, CommentID: 7, UpdatedAt: now,
	}}}
	if blk := ObservedAccountBlock(obs, p, state.AccountQuota{}, now); blk != nil {
		t.Errorf("a co-reviewer's limit blocked the account until %s", blk.Until)
	}
}
