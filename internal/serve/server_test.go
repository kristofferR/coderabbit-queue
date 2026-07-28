package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		req := httptest.NewRequest(http.MethodGet, path, nil)
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

	// Health must not read as ok either: a check that passes here passes against
	// a server that has never reached the state ref.
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != false {
		t.Errorf("health = %v before any load, want ok false", health)
	}

	// And the SSE stream sends nothing until there is something real to send.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	srv.handleEvents(rec, req.WithContext(ctx))
	if body := rec.Body.String(); body != "" {
		t.Errorf("the stream sent %q before any load; a browser would take it for live state", body)
	}

	// Once a load lands, everything answers.
	srv.refresh(context.Background())
	rec = httptest.NewRecorder()
	srv.handleSnapshot(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("snapshot = %d after a successful load, want 200", rec.Code)
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
