package crq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendRetriesTransientThenSucceeds(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token") // refreshToken resolves from env, no gh exec
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable) // transient 5xx -> retry
		case 2:
			w.WriteHeader(http.StatusUnauthorized) // 401 -> refresh token + retry
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	g := &GitHub{
		token:          "test-token",
		httpClient:     srv.Client(),
		apiBase:        srv.URL,
		maxRetries:     5,
		maxWait:        time.Second,
		backoffBase:    time.Millisecond,
		networkMaxWait: time.Second,
	}
	resp, err := g.send(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("send returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retries, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts (503, 401, 200), got %d", got)
	}
}

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "opaque transport failure" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestSendRetriesHTMLEdgeErrorButNotJSON4xx(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	newGH := func(srv *httptest.Server) *GitHub {
		return &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 4, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}
	}

	// An HTML 400 (edge error) is retried, then succeeds.
	var calls int32
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<html><head><title>Bad request</title></head><body>Bad request</body></html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer htmlSrv.Close()
	resp, err := newGH(htmlSrv).send(context.Background(), http.MethodPost, htmlSrv.URL+"/git/blobs", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("send returned error on retryable HTML edge error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected retry then 200 (2 calls), got status %d in %d calls", resp.StatusCode, calls)
	}

	// A genuine JSON 400 is NOT retried — returned to the caller immediately.
	var jcalls int32
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&jcalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Problems parsing JSON"}`))
	}))
	defer jsonSrv.Close()
	resp, err = newGH(jsonSrv).send(context.Background(), http.MethodPost, jsonSrv.URL+"/git/blobs", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("send should return the response, not an error, for a JSON 4xx: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || atomic.LoadInt32(&jcalls) != 1 {
		t.Fatalf("expected a single un-retried JSON 400, got status %d in %d calls", resp.StatusCode, jcalls)
	}
}

func TestIsRetryableNetErr(t *testing.T) {
	if isRetryableNetErr(nil) {
		t.Fatal("nil is not retryable")
	}
	// net.Error with Timeout() retries even when the message matches nothing.
	if !isRetryableNetErr(fakeTimeoutErr{}) {
		t.Fatal("a net.Error timeout should be retryable")
	}
	for _, msg := range []string{
		"context deadline exceeded (Client.Timeout exceeded while awaiting headers)",
		"dial tcp: connect: connection refused",
		"read: connection reset by peer",
		"dial tcp: lookup api.github.com: no such host",
		"net/http: TLS handshake timeout",
		"unexpected EOF",
	} {
		if !isRetryableNetErr(errors.New(msg)) {
			t.Fatalf("expected retryable transport error: %q", msg)
		}
	}
	if isRetryableNetErr(errors.New("invalid json payload")) {
		t.Fatal("a non-transport error must not be retried")
	}
}

func TestNetworkRetryWaitPlateaus(t *testing.T) {
	base := 2 * time.Second
	want := map[int]time.Duration{
		0:  2 * time.Second,
		1:  4 * time.Second,
		2:  8 * time.Second,
		3:  16 * time.Second,
		4:  30 * time.Second, // 32s capped to 30s
		5:  30 * time.Second,
		20: 30 * time.Second, // plateau holds for a long outage
	}
	for attempt, exp := range want {
		if got := networkRetryWait(base, attempt); got != exp {
			t.Fatalf("networkRetryWait(attempt=%d) = %s, want %s", attempt, got, exp)
		}
	}
}

func TestBackoffWaitRidesOutKnownReset(t *testing.T) {
	g := &GitHub{maxRetries: 6, maxWait: 120 * time.Second, backoffBase: 2 * time.Second}
	// Fresh reset within the cap: wait it out (~5m), reported as a known reset so
	// the caller doesn't spend its retry budget, not clamped to maxWait.
	if w, known, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(5 * time.Minute)}, 0); !ok || !known || w < 4*time.Minute || w > 6*time.Minute {
		t.Fatalf("expected ~5m known-reset ride-out, got %s known=%v ok=%v", w, known, ok)
	}
	// Implausibly far reset: give up rather than wedge.
	if _, _, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(3 * time.Hour)}, 0); ok {
		t.Fatal("should not wait out a reset beyond the cap")
	}
	// Expired reset hint (stale header / clock skew): treat as hint-less so it
	// consumes the budget instead of hot-looping with attempt frozen at ~0 wait.
	if w, known, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(-time.Minute)}, 0); !ok || known || w != 2*time.Second {
		t.Fatalf("expected expired reset to fall through to 2s budget-consuming backoff, got %s known=%v ok=%v", w, known, ok)
	}
	if _, _, ok := g.backoffWait(&RateLimitError{Until: time.Now().Add(-time.Minute)}, 6); ok {
		t.Fatal("should give up after maxRetries on an expired reset hint")
	}
	// Hint-less secondary limit: bounded exponential backoff, capped by maxRetries.
	if w, known, ok := g.backoffWait(&RateLimitError{}, 0); !ok || known || w != 2*time.Second {
		t.Fatalf("expected 2s exponential backoff, got %s known=%v ok=%v", w, known, ok)
	}
	if _, _, ok := g.backoffWait(&RateLimitError{}, 6); ok {
		t.Fatal("should give up after maxRetries on a hint-less limit")
	}
}

func TestSearchOwnerQualifierDistinguishesOrgAndUser(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var userHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			atomic.AddInt32(&userHits, 1)
			_, _ = w.Write([]byte(`{"type":"Organization"}`))
		case "/users/alice":
			_, _ = w.Write([]byte(`{"type":"User"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	if q, err := g.searchOwnerQualifier(context.Background(), "acme"); err != nil || q != "org:" {
		t.Fatalf("an organization scope must use org:, got %q", q)
	}
	if q, err := g.searchOwnerQualifier(context.Background(), "alice"); err != nil || q != "user:" {
		t.Fatalf("a user scope must use user:, got %q", q)
	}
	// Second lookup of the same login is served from cache, not a new request.
	if q, err := g.searchOwnerQualifier(context.Background(), "acme"); err != nil || q != "org:" {
		t.Fatalf("cached org lookup mismatch: %q", q)
	}
	if got := atomic.LoadInt32(&userHits); got != 1 {
		t.Fatalf("expected the org type to be cached after one lookup, made %d", got)
	}
}

func TestEachOpenPRPropagatesOwnerQualifierFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var searchHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			http.Error(w, "temporary scope lookup failure", http.StatusForbidden)
		case "/search/issues":
			atomic.AddInt32(&searchHits, 1)
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	err := g.EachOpenPR(context.Background(), "acme", false, func(SearchPR) (bool, error) {
		t.Fatal("callback should not run when owner qualifier lookup fails")
		return true, nil
	})
	if err == nil {
		t.Fatal("expected owner qualifier lookup failure to propagate")
	}
	if got := atomic.LoadInt32(&searchHits); got != 0 {
		t.Fatalf("must not run the open-PR search with a fallback qualifier after lookup failure, search hits=%d", got)
	}
}

func TestEachOpenPRStopsAtCallerBudgetWithoutOverFetching(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "t")
	var searchHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/alice" {
			_, _ = w.Write([]byte(`{"type":"User"}`))
			return
		}
		if r.URL.Path == "/search/issues" {
			atomic.AddInt32(&searchHits, 1)
			// A full page, so pagination would continue if the caller didn't stop.
			parts := make([]string, 0, 100)
			for i := 0; i < 100; i++ {
				parts = append(parts, `{"number":`+strconv.Itoa(i+1)+`,"repository_url":"https://api.github.com/repos/alice/repo"}`)
			}
			_, _ = w.Write([]byte(`{"items":[` + strings.Join(parts, ",") + `]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	g := &GitHub{token: "t", httpClient: srv.Client(), apiBase: srv.URL, maxRetries: 2, maxWait: time.Second, backoffBase: time.Millisecond, networkMaxWait: time.Second}

	seen := 0
	err := g.EachOpenPR(context.Background(), "alice", false, func(SearchPR) (bool, error) {
		seen++
		return seen >= 5, nil // caller's post-filter budget
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 5 {
		t.Fatalf("EachOpenPR should stop at the caller's budget (5), saw %d", seen)
	}
	if got := atomic.LoadInt32(&searchHits); got != 1 {
		t.Fatalf("should stop within the first page, made %d search requests", got)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, c := range []int{500, 502, 503, 504} {
		if !isRetryableStatus(c) {
			t.Fatalf("expected status %d to be retryable", c)
		}
	}
	for _, c := range []int{200, 400, 401, 403, 404, 422, 429, 501} {
		if isRetryableStatus(c) {
			t.Fatalf("status %d must not be retryable", c)
		}
	}
}

func TestRateLimitFrom(t *testing.T) {
	mk := func(status int, hdr map[string]string) *http.Response {
		h := http.Header{}
		for k, v := range hdr {
			h.Set(k, v)
		}
		return &http.Response{StatusCode: status, Header: h}
	}

	if rateLimitFrom(mk(http.StatusOK, nil), "") != nil {
		t.Fatal("200 must not be a rate limit")
	}
	if rateLimitFrom(mk(http.StatusNotFound, nil), "") != nil {
		t.Fatal("404 must not be a rate limit")
	}

	// Plain 403 (no quota exhaustion / secondary markers) is a permission error, not a rate limit.
	if rl := rateLimitFrom(mk(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4999"}), "Resource not accessible by integration"); rl != nil {
		t.Fatalf("plain 403 must not be a rate limit: %#v", rl)
	}

	// Primary: remaining 0 + reset header.
	reset := time.Now().Add(15 * time.Minute).Unix()
	rl := rateLimitFrom(mk(http.StatusForbidden, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset, 10),
	}), "API rate limit exceeded for user ID 1.")
	if rl == nil || rl.Kind != "primary" || rl.Until.Unix() != reset {
		t.Fatalf("primary rate limit mismatch: %#v", rl)
	}

	// Secondary with Retry-After.
	rl = rateLimitFrom(mk(http.StatusForbidden, map[string]string{"Retry-After": "30"}), "You have exceeded a secondary rate limit")
	if rl == nil || rl.Kind != "secondary" || rl.Until.IsZero() {
		t.Fatalf("secondary (retry-after) mismatch: %#v", rl)
	}

	// Secondary by body, no Retry-After: Until left zero so the caller applies exponential backoff.
	rl = rateLimitFrom(mk(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4321"}), "You have exceeded a secondary rate limit. Please wait a few minutes.")
	if rl == nil || rl.Kind != "secondary" || !rl.Until.IsZero() {
		t.Fatalf("secondary (body) mismatch: %#v", rl)
	}

	// 429 with reset is a rate limit too.
	rl = rateLimitFrom(mk(http.StatusTooManyRequests, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset, 10),
	}), "")
	if rl == nil || rl.Kind != "primary" {
		t.Fatalf("429 primary mismatch: %#v", rl)
	}
}
