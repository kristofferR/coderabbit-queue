package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// stubLoader hands back one state, counting how many times it was asked.
type stubLoader struct {
	st    state.State
	err   error
	reads int
}

func (l *stubLoader) Load(context.Context) (state.State, state.Revision, error) {
	l.reads++
	return l.st, state.Revision{}, l.err
}

// Before the first load returns there is no snapshot — and no error either. The
// zero Snapshot encodes its collections as null, and the client takes a 200 for
// live state and iterates them straight away, so the dashboard crashed during
// ordinary startup against a slow state read.
func TestHandlersRefuseUntilTheFirstLoadSucceeds(t *testing.T) {
	srv := New(&stubLoader{}, Options{Now: func() time.Time { return time.Unix(0, 0).UTC() }})

	for _, path := range []string{"/api/snapshot", "/api/overview"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		switch path {
		case "/api/snapshot":
			srv.handleSnapshot(rec, req)
		default:
			srv.handleOverview(rec, req)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d before any load, want 503 rather than a null-filled snapshot", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pr/o/r/1", nil)
	req.SetPathValue("owner", "o")
	req.SetPathValue("name", "r")
	req.SetPathValue("pr", "1")
	srv.handlePR(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/api/pr = %d before any load, want 503 rather than a false empty PR", rec.Code)
	}

	// Health must not read as ok either: a check that passes here passes against
	// a server that has never reached the state ref.
	rec = httptest.NewRecorder()
	srv.handleHealth(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", nil))
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != false {
		t.Errorf("health = %v before any load, want ok false", health)
	}

	// And the SSE stream sends nothing until there is something real to send.
	rec = httptest.NewRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
	srv.handleEvents(rec, req)
	if body := rec.Body.String(); body != "" {
		t.Errorf("the stream sent %q before any load; a browser would take it for live state", body)
	}

	// Once a load lands, everything answers.
	srv.refresh(context.Background())
	rec = httptest.NewRecorder()
	srv.handleSnapshot(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("snapshot = %d after a successful load, want 200", rec.Code)
	}
}

// A first read that FAILS is a different thing from one still running, and the
// stream is the only thing the page consumes. Sending nothing left a broken
// credential or state ref looking exactly like a slow one — the dashboard sat on
// "Reading the state ref…" indefinitely while every other handler could say why.
func TestTheStreamSaysWhyThereIsNoStateYet(t *testing.T) {
	loader := &stubLoader{err: errors.New("state ref unreadable: bad credentials")}
	srv := New(loader, Options{Now: func() time.Time { return time.Unix(0, 0).UTC() }})
	srv.refresh(context.Background())

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
	srv.handleEvents(rec, req)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: unavailable\n") {
		t.Fatalf("stream sent %q, want a named event a snapshot reader ignores", body)
	}
	if !strings.Contains(body, "bad credentials") {
		t.Errorf("stream sent %q, want the error that explains the empty page", body)
	}
}

func TestBroadcastReplacesAQueuedStaleFrame(t *testing.T) {
	ch := make(chan []byte, 1)
	ch <- []byte("old")
	broadcastFrame([]byte("new"), []chan []byte{ch})
	if got := string(<-ch); got != "new" {
		t.Fatalf("queued frame = %q, want the latest frame", got)
	}
}

// The dashboard header stops an ordinary cross-site POST but not a DNS rebind:
// a name the attacker controls, re-pointed at 127.0.0.1, is same-origin as far
// as the browser is concerned and may set any header it likes. The name the
// request was addressed to is what still tells the two apart.
func TestActionsAreRefusedOnANameThatOnlyResolvesHere(t *testing.T) {
	srv := New(&stubLoader{}, Options{Addr: "127.0.0.1:7777", Host: "atlas", AllowedHosts: []string{"crq.example.test"}})

	for _, host := range []string{
		"127.0.0.1:7777", "[::1]:7777", "192.168.1.4:7777", // an IP literal cannot be rebound
		"localhost:7777", "crq.localhost:7777",
		"atlas", "atlas.local:7777", "atlas.tail1234.ts.net", // the same machine, however it is reached
		"crq.example.test:7777", // named with --allow-host
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/action/hold", nil)
		req.Host = host
		if err := srv.addressedHere(req); err != nil {
			t.Errorf("Host %q was refused: %v", host, err)
		}
	}
	for _, host := range []string{
		"", "evil.test:7777", "crq.example.test.evil.test:7777",
		// The machine is called `atlas`, and this name is not it: a zone its
		// owner controls, pointed here, is the rebinding the check is for.
		"atlas.attacker.example:7777", "atlas.evil.test",
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/action/hold", nil)
		req.Host = host
		if err := srv.addressedHere(req); err == nil {
			t.Errorf("Host %q was accepted; a page on that name could act on this fleet", host)
		}
	}

	// And an Origin that contradicts the Host is not a same-origin request
	// whatever it claims, even when the Host itself is one we answer to.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/action/hold", nil)
	req.Host = "localhost:7777"
	req.Header.Set("Origin", "http://evil.test")
	if err := srv.addressedHere(req); err == nil {
		t.Error("a cross-origin POST to localhost was accepted")
	}
}

// Rev alone cannot decide whether to push. BuildOverview derives categorical
// state from `now` — a quota block expiring, a lease lapsing, a claim going
// dead — and none of that moves Rev, so a quiet fleet stayed visibly blocked
// until an unrelated write. The render clock itself must NOT count, or every
// poll would broadcast and the change detection would mean nothing.
func TestSnapshotDigestIgnoresTheClockButNotWhatItDecides(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	blocked := now.Add(10 * time.Minute)
	st := state.New()
	st.Account.BlockedUntil = &blocked

	build := func(at time.Time) Snapshot {
		ov := BuildOverview(st, at, 0, time.Minute, func(string) []BotName { return nil }, 0, nil)
		return BuildFleet(st, FleetConfig{}, ov, nil, "testhost", at,
			func(string) []BotName { return nil }, nil, nil, nil, nil)
	}

	if a, b := snapshotDigest(build(now)), snapshotDigest(build(now.Add(time.Second))); a != b {
		t.Error("the render clock moved the digest; every poll would broadcast and nothing would be gained")
	}
	if a, b := snapshotDigest(build(now)), snapshotDigest(build(blocked.Add(time.Minute))); a == b {
		t.Error("the quota block expired without changing the digest, so nothing would be pushed")
	}
}

// The rolling-upgrade table is only as right as its idea of "newest". Compared
// as text, 2.9.0 outranks 2.10.0 the first time the fleet crosses a digit
// boundary — and then every upgraded host is warned about while the hosts still
// running the old binary read as current, which is the warning exactly
// backwards.
func TestNewestHostVersionIsPickedNumerically(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := state.State{HostReports: map[string]state.HostReport{
		"atlas": {Host: "atlas", Version: "2.9.0", At: now},
		"borg":  {Host: "borg", Version: "2.10.0", At: now},
	}}

	behind := map[string]bool{}
	for _, row := range hostTools(st, now) {
		behind[row.Host] = row.Behind
	}
	if !behind["atlas"] {
		t.Error("2.9.0 is behind 2.10.0 and must be marked so")
	}
	if behind["borg"] {
		t.Error("2.10.0 is the newest crq reporting and must not be marked behind")
	}
}

// A round's answer belongs to whichever primary produced it. CRQ_BOT is a
// setting, so a fleet that changes it leaves rounds the retired bot answered
// behind — and attributing those to whatever this process calls its primary
// showed the newly configured bot as working and the one that actually reviewed
// as silent, which is both claims backwards.
func TestAPrimarysAnswerStaysWithThePrimaryThatGaveIt(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	answered := now.Add(-time.Hour)
	st := state.State{Rounds: map[string]state.Round{
		"o/repo#1": {
			Repo: "o/repo", PR: 1, Head: "abcdef123", Phase: state.PhaseCompleted,
			PrimaryAnsweredAt: &answered, PrimaryAnsweredBy: "coderabbitai[bot]",
		},
	}}
	// The dashboard has since been configured with a different primary.
	cfg := FleetConfig{GateRepo: "o/gate", Reviewers: []ReviewerCfg{
		{Login: "macroscope[bot]", Name: "macroscope", Primary: true, Metered: true},
	}}
	running := []BotName{{Login: "macroscope[bot]", Name: "macroscope", Primary: true}}

	macroscope := func(st state.State) BotCard {
		for _, card := range botCards(st, cfg, running, now) {
			if card.Login == "macroscope[bot]" {
				return card
			}
		}
		t.Fatalf("no card for the configured primary")
		return BotCard{}
	}
	if got := macroscope(st); got.LastSeen != nil || got.Status == "working" {
		t.Errorf("the new primary was credited with a review it never did: %+v", got)
	}

	// A round recorded before the login was stored has nobody to attribute it
	// to but the running primary, which is what crq assumed for every round
	// until now — so the fallback stays, and only rounds that name a bot move.
	legacy := st.Rounds["o/repo#1"]
	legacy.PrimaryAnsweredBy = ""
	st.Rounds["o/repo#1"] = legacy
	if got := macroscope(st); got.LastSeen == nil || !got.LastSeen.Equal(answered) {
		t.Errorf("an unattributed answer must still count for the running primary: %+v", got)
	}
}

func TestBotCardsUseTheEffectiveFleetPrimary(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cfg := FleetConfig{Reviewers: []ReviewerCfg{
		{Login: "coderabbitai[bot]", Name: "coderabbit", Primary: true, Metered: true},
		{Login: "cursor[bot]", Name: "bugbot"},
	}}
	running := []BotName{
		{Login: "cursor[bot]", Name: "bugbot", Primary: true, Required: true},
	}
	cards := botCards(state.State{}, cfg, running, now)
	primary := ""
	for _, card := range cards {
		if card.Primary {
			if primary != "" {
				t.Fatalf("multiple primary cards: %q and %q", primary, card.Login)
			}
			primary = card.Login
		}
		if card.Login == "coderabbitai[bot]" && card.Primary {
			t.Fatal("the retired startup primary is still marked primary")
		}
	}
	if primary != "cursor[bot]" {
		t.Fatalf("primary card = %q, want the effective fleet primary", primary)
	}
}

func TestBotCardsUseEffectiveReviewerMetadata(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cfg := FleetConfig{Reviewers: []ReviewerCfg{{
		Login: "cursor[bot]", Name: "bugbot", Command: "old command",
		Trigger: "never", Grace: Dur(time.Minute),
	}}}
	running := []BotName{{
		Login: "cursor[bot]", Name: "bugbot", Command: "new command",
		Trigger: "always", Grace: Dur(7 * time.Minute),
	}}
	cards := botCards(state.State{}, cfg, running, now)
	for _, card := range cards {
		if card.Login != "cursor[bot]" {
			continue
		}
		if card.Command != "new command" || card.Trigger != "always" || card.Grace != Dur(7*time.Minute) {
			t.Fatalf("card = %+v, want effective state-resolved metadata", card)
		}
		return
	}
	t.Fatal("no card for the effective reviewer")
}

func TestCoOnlyRoundLeavesPrimaryPending(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	marks := botMarks(state.Round{FiredAt: &at, CommandID: 42, CoOnly: true}, []BotName{{
		Login: "coderabbitai[bot]", Name: "coderabbit", Primary: true,
	}})
	if len(marks) != 1 || marks[0].Mark != "pending" {
		t.Fatalf("marks = %+v, want the uncommanded primary pending", marks)
	}
}

func TestObservationKeyIncludesEffectiveReviewers(t *testing.T) {
	base := []BotName{{Login: "coderabbitai[bot]", Primary: true, Required: true}}
	changed := []BotName{
		{Login: "coderabbitai[bot]", Primary: true, Required: true},
		{Login: "cursor[bot]", Required: true, Trigger: "always", Command: "bugbot run"},
	}
	if observationKey("o/r", 1, "abcdef123", base) ==
		observationKey("o/r", 1, "abcdef123", changed) {
		t.Fatal("reviewer change reused the old observation cache key")
	}
}
