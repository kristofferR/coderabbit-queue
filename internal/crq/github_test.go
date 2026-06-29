package crq

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

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
