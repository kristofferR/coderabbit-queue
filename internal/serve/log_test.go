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

func TestAutofixLogRequiresDashboardRequestAndResolvesTheLiveSession(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	called := false
	srv := New(&stubLoader{}, Options{
		Addr: "127.0.0.1:7777",
		Host: "atlas",
		Now:  func() time.Time { return now },
		TailLog: func(_ context.Context, repo, path string, max int64) (LogTail, error) {
			called = true
			if repo != "o/r" || path != "/safe/session.log" || max != 128<<10 {
				t.Fatalf("tail args = %q %q %d", repo, path, max)
			}
			return LogTail{Text: "working\n", Size: 8}, nil
		},
	})
	srv.lastState.Dispatches = map[string]state.DispatchClaim{
		"o/r#7": {Host: "host=atlas pid=1", At: now.Add(-time.Minute), Heartbeat: now, Log: "/safe/session.log"},
	}

	request := func(header bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:7777/api/autofix-log/o/r/7", nil)
		req.SetPathValue("owner", "o")
		req.SetPathValue("name", "r")
		req.SetPathValue("pr", "7")
		if header {
			req.Header.Set("X-CRQ-Dashboard", "1")
		}
		srv.handleAutofixLog(rec, req)
		return rec
	}
	if rec := request(false); rec.Code != http.StatusForbidden {
		t.Fatalf("untrusted log read = %d, want 403", rec.Code)
	}
	rec := request(true)
	var tail LogTail
	if err := json.Unmarshal(rec.Body.Bytes(), &tail); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || tail.Text != "working\n" || tail.Size != 8 || tail.Host != "atlas" {
		t.Fatalf("trusted log read = %d %+v", rec.Code, tail)
	}
	if !called {
		t.Fatal("trusted request never reached the bounded tail reader")
	}
}
