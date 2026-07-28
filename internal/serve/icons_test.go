package serve

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestIconAuthStaysOnTrustedGitHubHosts(t *testing.T) {
	icons := NewIcons("secret", nil)
	for _, rawURL := range []string{
		"https://api.github.com/users/bot",
		"https://raw.githubusercontent.com/o/r/HEAD/icon.png",
		"https://avatars.githubusercontent.com/u/1?v=4",
		"https://example.test/icon.png",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		icons.auth(req)
		want := strings.Contains(rawURL, "api.github.com") || strings.Contains(rawURL, "raw.githubusercontent.com")
		if got := req.Header.Get("Authorization") != ""; got != want {
			t.Errorf("Authorization on %s = %t, want %t", rawURL, got, want)
		}
	}
}

func TestIconFetchRetriesWithARefreshedToken(t *testing.T) {
	calls := 0
	icons := NewIcons("old", func(context.Context) string { return "new" })
	icons.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		status := http.StatusUnauthorized
		body := ""
		contentType := ""
		if req.Header.Get("Authorization") == "Bearer new" {
			status = http.StatusOK
			body = "\x89PNG\r\n\x1a\n"
			contentType = "image/png"
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	if _, _, ok := icons.fetch(t.Context(), "https://raw.githubusercontent.com/o/r/HEAD/icon.png"); !ok {
		t.Fatal("fetch did not retry successfully with the refreshed token")
	}
	if calls != 2 {
		t.Fatalf("requests = %d, want one initial request and one retry", calls)
	}
}

func TestMalformedIconNamesAreRejectedBeforeFetching(t *testing.T) {
	icons := NewIcons("", nil)
	icons.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("malformed input reached the HTTP client")
		return nil, nil
	})
	if got := icons.Repo(t.Context(), "../private"); got != nil {
		t.Fatalf("Repo returned %#v for traversal input", got)
	}
	if got := icons.Bot(t.Context(), "bot/../../token"); got != nil {
		t.Fatalf("Bot returned %#v for delimiter input", got)
	}
}
