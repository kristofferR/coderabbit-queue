package engine

import (
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

const (
	bugbotLogin = dialect.BugbotLogin
	macroLogin  = dialect.MacroscopeLogin
)

// TestDecideCoPostTriggerMatrix is the trigger-mode decision matrix:
// never/selfheal/always crossed with the bot's observed activity, plus the
// mode-independent guards (already commanded, live command, head evidence,
// check run present including in-progress).
func TestDecideCoPostTriggerMatrix(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	grace := 10 * time.Minute
	freshAnchor := now.Add(-time.Minute)
	staleAnchor := now.Add(-time.Hour)

	policy := func(mode TriggerMode) CoReviewerPolicy {
		return CoReviewerPolicy{Login: bugbotLogin, Command: "bugbot run", Trigger: mode, SelfHealGrace: grace}
	}
	obsWith := func(co CoSeen, checks ...CheckSeen) Observation {
		return Observation{Head: head, Open: true, Checks: checks,
			Co: map[string]CoSeen{dialect.NormalizeBotName(bugbotLogin): co}}
	}
	commanded := state.Round{Head: head}
	commanded.SetCoCommand(bugbotLogin, 42, now.Add(-time.Hour))

	cases := []struct {
		name           string
		cp             CoReviewerPolicy
		round          state.Round
		obs            Observation
		commandPresent bool
		anchor         time.Time
		want           bool
	}{
		{name: "never posts nothing even when silent", cp: policy(TriggerNever), obs: obsWith(CoSeen{}), anchor: staleAnchor, want: false},
		{name: "always posts for a silent bot", cp: policy(TriggerAlways), obs: obsWith(CoSeen{}), want: true},
		{name: "always defers to auto-review", cp: policy(TriggerAlways), obs: obsWith(CoSeen{AutoActive: true}), want: false},
		{name: "always respects a live command", cp: policy(TriggerAlways), obs: obsWith(CoSeen{}), commandPresent: true, want: false},
		{name: "always respects the recorded round command", cp: policy(TriggerAlways), round: commanded, obs: obsWith(CoSeen{}), want: false},
		{
			name: "always never double-reviews a completed check",
			cp:   policy(TriggerAlways),
			obs:  obsWith(CoSeen{}, CheckSeen{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean, CompletedAt: now}),
			want: false,
		},
		{
			name: "always waits out an in-progress check",
			cp:   policy(TriggerAlways),
			obs:  obsWith(CoSeen{}, CheckSeen{Bot: bugbotLogin, Verdict: dialect.CheckInProgress}),
			want: false,
		},
		{
			name: "always ignores another bot's check",
			cp:   policy(TriggerAlways),
			obs:  obsWith(CoSeen{}, CheckSeen{Bot: macroLogin, Verdict: dialect.CheckDone, CompletedAt: now}),
			want: true,
		},
		{name: "selfheal stays quiet for an inactive bot", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{}), anchor: staleAnchor, want: false},
		{name: "selfheal posts for an active bot that missed the head past grace", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{AutoActive: true}), anchor: staleAnchor, want: true},
		{name: "selfheal counts round activity as active", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{ActiveThisRound: true}), anchor: staleAnchor, want: true},
		{name: "selfheal waits out the grace period", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{AutoActive: true}), anchor: freshAnchor, want: false},
		{name: "selfheal needs an anchor", cp: policy(TriggerSelfHeal), obs: obsWith(CoSeen{AutoActive: true}), want: false},
		{
			name: "selfheal never re-triggers a bot with head evidence",
			cp:   policy(TriggerSelfHeal),
			obs: Observation{Head: head, Open: true,
				Reviews: []ReviewSeen{{Bot: bugbotLogin, Commit: "abcdef1234567890", SubmittedAt: now.Add(-time.Minute)}},
				Co:      map[string]CoSeen{dialect.NormalizeBotName(bugbotLogin): {AutoActive: true}}},
			anchor: staleAnchor, want: false,
		},
		{
			name:   "selfheal suppressed by an in-progress check",
			cp:     policy(TriggerSelfHeal),
			obs:    obsWith(CoSeen{AutoActive: true}, CheckSeen{Bot: bugbotLogin, Verdict: dialect.CheckInProgress}),
			anchor: staleAnchor, want: false,
		},
		{name: "no command configured never posts", cp: CoReviewerPolicy{Login: bugbotLogin, Trigger: TriggerAlways}, obs: obsWith(CoSeen{}), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.round.Head == "" {
				tc.round = state.Round{Head: head}
			}
			if got := DecideCoPost(tc.round, tc.obs, tc.cp, tc.commandPresent, tc.anchor, now); got != tc.want {
				t.Fatalf("DecideCoPost = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCompletionCheckEvidence pins step 2b and the generic dynamic gate: a
// required check-bearing bot converges on its completed check run alone (the
// silent-clean Bugbot round), an in-progress check does not, and a
// wanted-only bot gates exactly when observed active.
func TestCompletionCheckEvidence(t *testing.T) {
	fired := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	round := state.Round{Head: head, FiredAt: &fired}
	crReview := ReviewSeen{Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: fired.Add(time.Minute)}
	bugbotKey := dialect.NormalizeBotName(bugbotLogin)

	required := Policy{Bot: "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]", bugbotLogin},
		CoReviewers:  []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerSelfHeal}}}

	// Required Bugbot, no evidence: not done.
	silent := Observation{Head: head, Open: true, Reviews: []ReviewSeen{crReview}}
	if got := Completion(round, silent, required); got.Done {
		t.Fatalf("required bugbot with no evidence must gate: %+v", got)
	}
	// Its clean check run alone converges the round.
	clean := silent
	clean.Checks = []CheckSeen{{Bot: bugbotLogin, Name: "Cursor Bugbot", Verdict: dialect.CheckDoneClean, CompletedAt: fired.Add(2 * time.Minute)}}
	if got := Completion(round, clean, required); !got.Done {
		t.Fatalf("a completed clean check must converge the silent-clean round: %+v", got)
	}
	// A completed check with findings converges too — the findings gate via
	// threads in the feedback layer, not via Completion.
	issues := silent
	issues.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDone, CompletedAt: fired.Add(2 * time.Minute)}}
	if got := Completion(round, issues, required); !got.Done {
		t.Fatalf("a completed check with findings must still mark the bot reviewed: %+v", got)
	}
	// An in-progress check is not evidence.
	running := silent
	running.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckInProgress}}
	if got := Completion(round, running, required); got.Done {
		t.Fatalf("an in-progress check must not converge the round: %+v", got)
	}

	// Wanted-only (not required): an inactive bot never gates.
	wanted := Policy{Bot: "coderabbitai[bot]",
		RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers:  []CoReviewerPolicy{{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerSelfHeal}}}
	if got := Completion(round, silent, wanted); !got.Done {
		t.Fatalf("a wanted-only inactive bot must not gate: %+v", got)
	}
	// Observed active this round (in-progress check), it gates dynamically…
	activeRunning := running
	activeRunning.Co = map[string]CoSeen{bugbotKey: {ActiveThisRound: true}}
	if got := Completion(round, activeRunning, wanted); got.Done {
		t.Fatalf("an active wanted bot must gate until its evidence lands: %+v", got)
	}
	// …and its completed check satisfies the dynamic gate.
	activeDone := activeRunning
	activeDone.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean, CompletedAt: fired.Add(2 * time.Minute)}}
	if got := Completion(round, activeDone, wanted); !got.Done {
		t.Fatalf("the active bot's completed check must satisfy the gate: %+v", got)
	}
}

// TestCompletionVerdictIsParticipationOnly: a Macroscope approvability verdict
// engages round participation (CoActiveThisRound) but never counts as review
// evidence.
func TestCompletionVerdictIsParticipationOnly(t *testing.T) {
	fired := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	round := state.Round{Head: head, FiredAt: &fired}
	approved := true
	verdict := dialect.BotEvent{Kind: dialect.EvCoVerdict, Bot: macroLogin, For: macroLogin,
		Approved: &approved, CommentID: 9, CreatedAt: fired.Add(time.Minute), UpdatedAt: fired.Add(time.Minute)}
	obs := Observation{Head: head, Open: true, Events: []dialect.BotEvent{verdict}}

	if !CoActiveThisRound(round, obs, macroLogin) {
		t.Fatal("a round-window verdict must count as participation")
	}
	if reviewed := coReviewedHead(obs, macroLogin); reviewed {
		t.Fatal("a verdict must never count as head review evidence")
	}
}

// TestDecideFireMultiBotPostCo: multiple always-mode co-reviewers post in one
// fire step, ride the same dedupe resolution, and the deferred degrade carries
// all of them without touching the fire slot.
func TestDecideFireMultiBotPostCo(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	head := "abcdef123"
	queued := state.Round{Repo: "o/r", PR: 1, Head: head, Phase: state.PhaseQueued}
	p := Policy{Bot: "coderabbitai[bot]",
		RequiredBots:          []string{"coderabbitai[bot]", dialect.CodexBotLogin, bugbotLogin},
		RateLimitCodexDegrade: true,
		CoReviewers: []CoReviewerPolicy{
			{Login: dialect.CodexBotLogin, Command: "@codex review", Trigger: TriggerAlways},
			{Login: bugbotLogin, Command: "bugbot run", Trigger: TriggerAlways},
		}}
	free := Global{SlotFree: true}

	wantBoth := func(t *testing.T, d FireDecision) {
		t.Helper()
		if len(d.PostCo) != 2 || !hasCodexLogin(d.PostCo) {
			t.Fatalf("PostCo = %v, want codex+bugbot", d.PostCo)
		}
		if !d.PostCodex {
			t.Fatal("PostCodex mirror must be set while codex is in PostCo")
		}
	}

	d := DecideFire(free, queued, Observation{Head: head, Open: true}, now, p)
	if d.Verdict != FirePost {
		t.Fatalf("verdict = %v, want FirePost", d.Verdict)
	}
	wantBoth(t, d)

	// CodeRabbit already reviewed the head → co-only trigger for both.
	crReviewed := Observation{Head: head, Open: true,
		Reviews: []ReviewSeen{{Bot: "coderabbitai[bot]", Commit: "abcdef1234567890", SubmittedAt: now}}}
	d = DecideFire(free, queued, crReviewed, now, p)
	if d.Verdict != FireCoOnly {
		t.Fatalf("verdict = %v, want FireCoOnly", d.Verdict)
	}
	wantBoth(t, d)

	// One bot already has head evidence → only the other posts.
	partial := crReviewed
	partial.Checks = []CheckSeen{{Bot: bugbotLogin, Verdict: dialect.CheckDoneClean, CompletedAt: now}}
	d = DecideFire(free, queued, partial, now, p)
	if d.Verdict != FireCoOnly || len(d.PostCo) != 1 || !dialect.IsCodexBot(d.PostCo[0]) {
		t.Fatalf("expected codex-only PostCo, got %+v", d)
	}

	// Account blocked → deferred degrade posts both, slot untouched.
	blocked := now.Add(time.Hour)
	d = DecideFire(Global{SlotFree: true, BlockedUntil: &blocked}, queued, Observation{Head: head, Open: true}, now, p)
	if d.Verdict != FireCoDeferred {
		t.Fatalf("verdict = %v, want FireCoDeferred", d.Verdict)
	}
	wantBoth(t, d)

	// A wanted-only silent bot never forces a wait: with every co-reviewer
	// satisfied or inert, the reviewed head dedupes.
	inert := Policy{Bot: "coderabbitai[bot]", RequiredBots: []string{"coderabbitai[bot]"},
		CoReviewers: []CoReviewerPolicy{{Login: macroLogin, Command: "", Trigger: TriggerSelfHeal}}}
	d = DecideFire(free, queued, crReviewed, now, inert)
	if d.Verdict != FireDedupe {
		t.Fatalf("verdict = %v, want FireDedupe for a wanted-only silent bot", d.Verdict)
	}
}
