package serve

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Icons resolves the small images the dashboard shows: a repository's own
// favicon, and a review bot's GitHub avatar.
//
// Both are fetched lazily and cached, including misses. A repository without a
// favicon is the common case, and re-discovering that on every render would
// spend the shared REST budget on nothing. The browser never talks to GitHub
// itself — that would leak the token or fail on private repos.
type Icons struct {
	token  string
	client *http.Client

	mu    sync.Mutex
	cache map[string]*icon
}

type icon struct {
	body        []byte
	contentType string
	expires     time.Time
	// missing distinguishes "we looked and there is nothing" from "not looked
	// at yet", so a miss is cached rather than retried on every paint.
	missing bool
}

const (
	iconTTL = 6 * time.Hour
	missTTL = 1 * time.Hour
)

// candidatePaths are where projects actually keep a favicon, most likely first.
// Each miss costs one request, so the list is ordered by how often it hits
// across a real fleet rather than by completeness.
var candidatePaths = []string{
	"favicon.png",
	"favicon.svg",
	"public/favicon.png",
	"public/favicon.svg",
	"app/public/icon.svg",
	"app/public/favicon.png",
	"static/favicon.png",
	"assets/favicon.png",
	"web/public/favicon.png",
	"docs/favicon.png",
	"icon.png",
	"Logo/128.png",
	"src-tauri/icons/128x128.png",
	"assets/icons/favicon.png",
	".github/logo.png",
}

func NewIcons(token string) *Icons {
	return &Icons{
		token:  token,
		client: &http.Client{Timeout: 10 * time.Second},
		cache:  map[string]*icon{},
	}
}

func (ic *Icons) get(key string) (*icon, bool) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	got, ok := ic.cache[key]
	if !ok || time.Now().After(got.expires) {
		return nil, false
	}
	return got, true
}

func (ic *Icons) put(key string, got *icon) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	ic.cache[key] = got
}

// Repo returns a repository's favicon, or nil when it has none.
func (ic *Icons) Repo(ctx context.Context, repo string) *icon {
	key := "repo:" + strings.ToLower(repo)
	if got, ok := ic.get(key); ok {
		return got
	}
	for _, path := range candidatePaths {
		// raw.githubusercontent honours the token, so private repos work and we
		// avoid the contents API's base64 envelope.
		raw := "https://raw.githubusercontent.com/" + repo + "/HEAD/" + path
		if body, ctype, ok := ic.fetch(ctx, raw); ok {
			got := &icon{body: body, contentType: ctype, expires: time.Now().Add(iconTTL)}
			ic.put(key, got)
			return got
		}
	}
	ic.put(key, &icon{missing: true, expires: time.Now().Add(missTTL)})
	return nil
}

// Bot returns a reviewer's avatar. Bot logins are ordinary GitHub accounts for
// OAuth apps, but a GitHub App's avatar lives under /in/<id> and is only
// reachable through the users API, so both routes are tried.
func (ic *Icons) Bot(ctx context.Context, login string) *icon {
	key := "bot:" + strings.ToLower(login)
	if got, ok := ic.get(key); ok {
		return got
	}
	for _, candidate := range []string{login, strings.TrimSuffix(login, "[bot]")} {
		avatar := ic.avatarURL(ctx, candidate)
		if avatar == "" {
			continue
		}
		if body, ctype, ok := ic.fetch(ctx, avatar+"&s=96"); ok {
			got := &icon{body: body, contentType: ctype, expires: time.Now().Add(iconTTL)}
			ic.put(key, got)
			return got
		}
	}
	ic.put(key, &icon{missing: true, expires: time.Now().Add(missTTL)})
	return nil
}

func (ic *Icons) avatarURL(ctx context.Context, login string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/users/"+url.PathEscape(login), nil)
	if err != nil {
		return ""
	}
	ic.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := ic.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	// A tiny scan beats decoding the whole user object for one field.
	const marker = `"avatar_url":"`
	i := strings.Index(string(body), marker)
	if i < 0 {
		return ""
	}
	rest := string(body[i+len(marker):])
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (ic *Icons) fetch(ctx context.Context, rawURL string) ([]byte, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false
	}
	ic.auth(req)
	resp, err := ic.client.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}
	// A favicon is small; anything large is not one, and streaming it would
	// only tie up memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil || len(body) == 0 {
		return nil, "", false
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" || strings.HasPrefix(ctype, "text/plain") {
		ctype = sniff(rawURL, body)
	}
	if !strings.HasPrefix(ctype, "image/") {
		return nil, "", false
	}
	return body, ctype, true
}

func (ic *Icons) auth(req *http.Request) {
	if ic.token != "" {
		req.Header.Set("Authorization", "Bearer "+ic.token)
	}
	req.Header.Set("User-Agent", "crq-dashboard")
}

// sniff decides a content type when the origin serves everything as text,
// which raw.githubusercontent does.
func sniff(path string, body []byte) string {
	switch {
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".ico"):
		return "image/x-icon"
	}
	return http.DetectContentType(body)
}

// handleIcon serves both kinds. A miss is a 404 so the browser falls back to
// the letter tile without another round trip.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	if s.icons == nil {
		http.NotFound(w, r)
		return
	}
	kind := r.PathValue("kind")
	name := strings.Trim(r.PathValue("name"), "/")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	var got *icon
	switch kind {
	case "repo":
		got = s.icons.Repo(r.Context(), name)
	case "bot":
		got = s.icons.Bot(r.Context(), name)
	default:
		http.NotFound(w, r)
		return
	}
	if got == nil || got.missing {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", got.contentType)
	// These bytes come from somebody else's repository, and an SVG is not a
	// picture — navigated to directly, /api/icon/repo/<owner>/<name> is a
	// DOCUMENT on the dashboard's own origin, so a scripted favicon.svg could
	// call the mutation endpoints and set the header handleAction checks for.
	// The sandbox directive drops it into an opaque origin with scripting off,
	// which the <img> the dashboard actually renders never needed anyway.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(got.body)
}
