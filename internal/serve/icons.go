package serve

import (
	"context"
	"encoding/json"
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
	token       string
	lookupToken func(context.Context) string
	client      *http.Client

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
	iconTTL  = 6 * time.Hour
	missTTL  = 1 * time.Hour
	maxIcons = 512
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

func NewIcons(token string, lookupToken func(context.Context) string) *Icons {
	return &Icons{
		token:       token,
		lookupToken: lookupToken,
		client:      &http.Client{Timeout: 10 * time.Second},
		cache:       map[string]*icon{},
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
	now := time.Now()
	for k, cached := range ic.cache {
		if now.After(cached.expires) {
			delete(ic.cache, k)
		}
	}
	if len(ic.cache) >= maxIcons {
		var oldestKey string
		var oldest time.Time
		for k, cached := range ic.cache {
			if oldestKey == "" || cached.expires.Before(oldest) {
				oldestKey, oldest = k, cached.expires
			}
		}
		delete(ic.cache, oldestKey)
	}
	ic.cache[key] = got
}

// Repo returns a repository's favicon, or nil when it has none.
func (ic *Icons) Repo(ctx context.Context, repo string) *icon {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !plainName(owner) || !plainName(name) || strings.Contains(name, "/") {
		return nil
	}
	key := "repo:" + strings.ToLower(repo)
	if got, ok := ic.get(key); ok {
		return got
	}
	for _, path := range candidatePaths {
		// raw.githubusercontent honours the token, so private repos work and we
		// avoid the contents API's base64 envelope.
		segments := []string{url.PathEscape(owner), url.PathEscape(name), "HEAD"}
		for _, segment := range strings.Split(path, "/") {
			segments = append(segments, url.PathEscape(segment))
		}
		raw := "https://raw.githubusercontent.com/" + strings.Join(segments, "/")
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
	if !plainBotLogin(login) {
		return nil
	}
	key := "bot:" + strings.ToLower(login)
	if got, ok := ic.get(key); ok {
		return got
	}
	for _, candidate := range []string{login, strings.TrimSuffix(login, "[bot]")} {
		avatar := ic.avatarURL(ctx, candidate)
		if avatar == "" {
			continue
		}
		avatarURL, err := url.Parse(avatar)
		if err != nil {
			continue
		}
		query := avatarURL.Query()
		query.Set("s", "96")
		avatarURL.RawQuery = query.Encode()
		if body, ctype, ok := ic.fetch(ctx, avatarURL.String()); ok {
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
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := ic.do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var user struct {
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user); err != nil {
		return ""
	}
	return user.AvatarURL
}

func (ic *Icons) fetch(ctx context.Context, rawURL string) ([]byte, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false
	}
	resp, err := ic.do(req)
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
	if req.URL.Host == "api.github.com" || req.URL.Host == "raw.githubusercontent.com" {
		token := ic.currentToken(req.Context())
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	req.Header.Set("User-Agent", "crq-dashboard")
}

func (ic *Icons) currentToken(ctx context.Context) string {
	ic.mu.Lock()
	token, lookup := ic.token, ic.lookupToken
	ic.mu.Unlock()
	if token != "" || lookup == nil {
		return token
	}
	token = lookup(ctx)
	if token == "" {
		return ""
	}
	ic.mu.Lock()
	if ic.token == "" {
		ic.token = token
	}
	token = ic.token
	ic.mu.Unlock()
	return token
}

func (ic *Icons) do(req *http.Request) (*http.Response, error) {
	ic.auth(req)
	usedToken := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	resp, err := ic.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden ||
		ic.lookupToken == nil {
		return resp, err
	}
	token := ic.lookupToken(req.Context())
	if token == "" || token == usedToken {
		return resp, nil
	}
	resp.Body.Close()
	ic.mu.Lock()
	ic.token = token
	ic.mu.Unlock()
	retry := req.Clone(req.Context())
	retry.Header = req.Header.Clone()
	retry.Header.Del("Authorization")
	ic.auth(retry)
	return ic.client.Do(retry)
}

func plainName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func plainBotLogin(login string) bool {
	login = strings.TrimSuffix(login, "[bot]")
	return plainName(login)
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
	// Unlike the other credential-backed reads, icons are loaded by <img>, so
	// they cannot carry the dashboard's custom header. Fetch Metadata still
	// distinguishes the dashboard's same-origin image load from a hostile page
	// embedding arbitrarily many cache misses.
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon requests must come from this dashboard"})
		return
	}
	if err := s.addressedHere(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
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
