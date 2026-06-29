package crq

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("github resource not found")

type APIError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s failed: %d %s", e.Method, e.URL, e.Status, strings.TrimSpace(e.Body))
}

type GitHub struct {
	token          string
	tokenMu        sync.Mutex
	httpClient     *http.Client
	apiBase        string
	graphBase      string
	log            Logger
	maxRetries     int
	maxWait        time.Duration
	backoffBase    time.Duration
	networkMaxWait time.Duration
}

// lookupToken resolves a GitHub token from the environment or the gh CLI. gh can
// hand back a freshly-rotated OAuth token, which is why send re-runs this on a 401.
func lookupToken(ctx context.Context) string {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GH_TOKEN"))
	}
	if token == "" {
		if out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output(); err == nil {
			token = strings.TrimSpace(string(out))
		}
	}
	return token
}

func (g *GitHub) authToken() string {
	g.tokenMu.Lock()
	defer g.tokenMu.Unlock()
	return g.token
}

// refreshToken re-resolves the token (e.g. after a 401) in case gh rotated it.
func (g *GitHub) refreshToken(ctx context.Context) {
	if t := lookupToken(ctx); t != "" {
		g.tokenMu.Lock()
		g.token = t
		g.tokenMu.Unlock()
	}
}

func NewGitHub(ctx context.Context) (*GitHub, error) {
	token := lookupToken(ctx)
	if token == "" {
		return nil, errors.New("GitHub token not found (set GITHUB_TOKEN/GH_TOKEN or run 'gh auth login')")
	}
	maxWait := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("CRQ_GITHUB_MAX_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			maxWait = d
		}
	}
	maxRetries := 6
	if v := strings.TrimSpace(os.Getenv("CRQ_GITHUB_RETRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxRetries = n
		}
	}
	// 0 = no cap: ride out an outage and keep retrying until connectivity returns
	// (only caller cancellation stops it). Set CRQ_NETWORK_MAX_WAIT to bound it.
	networkMaxWait := time.Duration(0)
	if v := strings.TrimSpace(os.Getenv("CRQ_NETWORK_MAX_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			networkMaxWait = d
		}
	}
	return &GitHub{
		token:          token,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		apiBase:        "https://api.github.com",
		graphBase:      "https://api.github.com/graphql",
		maxRetries:     maxRetries,
		maxWait:        maxWait,
		backoffBase:    2 * time.Second,
		networkMaxWait: networkMaxWait,
	}, nil
}

// SetLogger attaches a logger so rate-limit backoff/retry is visible to humans and the daemon log.
func (g *GitHub) SetLogger(l Logger) { g.log = l }

func (g *GitHub) request(ctx context.Context, method, path string, in, out any) error {
	body, err := marshalBody(in)
	if err != nil {
		return err
	}
	resp, err := g.send(ctx, method, g.apiBase+path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: method, URL: path, Status: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHub) requestPaged(ctx context.Context, path string, out any) error {
	value := reflect.ValueOf(out)
	if value.Kind() != reflect.Pointer || value.Elem().Kind() != reflect.Slice {
		return errors.New("paged output must be pointer to slice")
	}
	next := g.apiBase + path
	for next != "" {
		resp, err := g.send(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return ErrNotFound
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return &APIError{Method: http.MethodGet, URL: next, Status: resp.StatusCode, Body: string(b)}
		}
		page := reflect.New(value.Elem().Type()).Interface()
		if err := json.NewDecoder(resp.Body).Decode(page); err != nil {
			resp.Body.Close()
			return err
		}
		link := resp.Header.Get("Link")
		resp.Body.Close()
		value.Elem().Set(reflect.AppendSlice(value.Elem(), reflect.ValueOf(page).Elem()))
		next = nextPage(link)
	}
	return nil
}

func (g *GitHub) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := marshalBody(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		resp, err := g.send(ctx, http.MethodPost, g.graphBase, body)
		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return &APIError{Method: http.MethodPost, URL: g.graphBase, Status: resp.StatusCode, Body: string(b)}
		}
		var envelope struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
		reset := resp.Header.Get("X-RateLimit-Reset")
		resp.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if len(envelope.Errors) > 0 {
			msg := envelope.Errors[0].Message
			// GraphQL reports rate limits as a 200 with a RATE_LIMITED error
			// rather than 403/429, so retry it with the same backoff as send.
			if strings.EqualFold(envelope.Errors[0].Type, "RATE_LIMITED") || strings.Contains(strings.ToLower(msg), "rate limit") {
				rl := &RateLimitError{Kind: "graphql", Method: http.MethodPost, URL: g.graphBase, Remaining: -1}
				if reset != "" {
					if epoch, perr := strconv.ParseInt(reset, 10, 64); perr == nil {
						rl.Until = time.Unix(epoch, 0)
					}
				}
				wait, ok := g.backoffWait(rl, attempt)
				if !ok {
					return rl
				}
				if g.log != nil {
					g.log.Printf("github graphql rate limit; backing off %s (attempt %d/%d)", wait.Round(time.Second), attempt+1, g.maxRetries)
				}
				if serr := sleepCtx(ctx, wait); serr != nil {
					return serr
				}
				continue
			}
			return errors.New(msg)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(envelope.Data, out)
	}
}

func (g *GitHub) decorate(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.authToken())
	req.Header.Set("User-Agent", "crq/"+Version)
}

func marshalBody(in any) ([]byte, error) {
	if in == nil {
		return nil, nil
	}
	return json.Marshal(in)
}

// RateLimitError is returned when GitHub rate-limits a request and crq could not
// wait it out within its retry budget. It carries the reset time so callers (and
// humans) get an actionable message instead of an opaque 403.
type RateLimitError struct {
	Method    string
	URL       string
	Kind      string // "primary", "secondary", or "graphql"
	Remaining int    // x-ratelimit-remaining when known, else -1
	Until     time.Time
}

func (e *RateLimitError) Error() string {
	if e.Until.IsZero() {
		return fmt.Sprintf("github %s rate limit hit (%s %s); retry shortly", e.Kind, e.Method, shortURL(e.URL))
	}
	wait := time.Until(e.Until).Round(time.Second)
	if wait < 0 {
		wait = 0
	}
	return fmt.Sprintf("github %s rate limit hit (%s %s); resets %s (~%s)", e.Kind, e.Method, shortURL(e.URL), e.Until.UTC().Format(time.RFC3339), wait)
}

// IsRateLimited reports whether err is (or wraps) a GitHub rate-limit error.
func IsRateLimited(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

// isCommentCapError reports whether err is GitHub's hard cap of 2500 comments per
// issue ("Commenting is disabled on issues with more than 2500 comments").
func isCommentCapError(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	b := strings.ToLower(api.Body)
	return strings.Contains(b, "commenting is disabled") || strings.Contains(b, "more than 2500 comments")
}

// rateLimitWait returns how long to wait before retrying a rate-limited error.
// The bool is true when err is a rate limit; the duration is 0 when GitHub gave
// no reset hint (the caller should apply its own default backoff).
func rateLimitWait(err error) (time.Duration, bool) {
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		return 0, false
	}
	if rl.Until.IsZero() {
		return 0, true
	}
	d := time.Until(rl.Until)
	if d < 0 {
		d = 0
	}
	return d, true
}

// rateLimitFrom classifies a 403/429 response as a GitHub primary or secondary
// rate limit, or returns nil if it is an ordinary error (e.g. a permission 403).
func rateLimitFrom(resp *http.Response, body string) *RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	lower := strings.ToLower(body)
	// Secondary limit: honor an explicit Retry-After (seconds).
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return &RateLimitError{Kind: "secondary", Remaining: -1, Until: time.Now().Add(time.Duration(secs) * time.Second)}
		}
	}
	remaining := -1
	if r := resp.Header.Get("X-RateLimit-Remaining"); r != "" {
		if n, err := strconv.Atoi(r); err == nil {
			remaining = n
		}
	}
	resetUntil := func() time.Time {
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return time.Unix(epoch, 0)
			}
		}
		return time.Time{}
	}
	// Primary limit: the quota is exhausted; wait until the window resets.
	if remaining == 0 || strings.Contains(lower, "api rate limit exceeded") {
		return &RateLimitError{Kind: "primary", Remaining: 0, Until: resetUntil()}
	}
	// Secondary/abuse limit without a Retry-After header: caller backs off.
	if strings.Contains(lower, "secondary rate limit") || strings.Contains(lower, "exceeded a secondary") || strings.Contains(lower, "abuse detection") {
		return &RateLimitError{Kind: "secondary", Remaining: remaining}
	}
	return nil
}

// send performs an HTTP request with rate-limit and failure resilience. It rides
// out internet outages by retrying transient transport errors (timeouts,
// refused/reset connections, DNS hiccups, TLS failures, short EOFs) on a backoff
// that plateaus at 30s — by default with no time cap, so it keeps probing until
// connectivity returns rather than ever failing the agent on a network drop
// (set CRQ_NETWORK_MAX_WAIT to bound it). The retry attempt is itself the probe.
// It also retries 5xx and backs off GitHub rate limits with the bounded
// maxRetries/maxWait budget. Real caller cancellation (ctx done) is never retried.
func (g *GitHub) send(ctx context.Context, method, fullURL string, body []byte) (*http.Response, error) {
	attempt := 0    // bounded retries for 5xx + rate limits
	netAttempt := 0 // consecutive transient-network retries
	var offlineSince time.Time
	for {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
		if err != nil {
			return nil, err
		}
		g.decorate(req)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := g.httpClient.Do(req)
		if err != nil {
			// Caller cancelled or its deadline passed: surface that, don't retry.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if isRetryableNetErr(err) {
				if offlineSince.IsZero() {
					offlineSince = time.Now()
				}
				down := time.Since(offlineSince)
				// networkMaxWait <= 0 means no cap: keep retrying until the
				// network is back (only caller cancellation, handled above, stops us).
				if g.networkMaxWait > 0 && down > g.networkMaxWait {
					return nil, fmt.Errorf("github unreachable for %s (%s %s): %w", down.Round(time.Second), method, shortURL(fullURL), err)
				}
				wait := networkRetryWait(g.backoffBase, netAttempt)
				netAttempt++
				if g.log != nil {
					capStr := "no cap"
					if g.networkMaxWait > 0 {
						capStr = g.networkMaxWait.String()
					}
					g.log.Printf("github unreachable on %s %s (%v); retrying in %s (offline %s / cap %s)", method, shortURL(fullURL), err, wait.Round(time.Second), down.Round(time.Second), capStr)
				}
				if serr := sleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, err
		}
		// A response came back: connectivity is fine, reset the offline tracker.
		if !offlineSince.IsZero() && g.log != nil {
			g.log.Printf("github reachable again after %s offline; resuming", time.Since(offlineSince).Round(time.Second))
		}
		netAttempt, offlineSince = 0, time.Time{}
		// Retry transient server errors (500/502/503/504).
		if isRetryableStatus(resp.StatusCode) {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if wait, ok := g.retryBackoff(attempt); ok {
				attempt++
				if g.log != nil {
					g.log.Printf("github %s %s: HTTP %d; retrying in %s (attempt %d/%d)", method, shortURL(fullURL), resp.StatusCode, wait.Round(time.Second), attempt, g.maxRetries)
				}
				if serr := sleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		// GitHub's API always returns JSON; a non-2xx with an HTML body is a
		// transient edge error (a "Bad request" / "Unicorn!" page served before the
		// request reaches the API), not a real API error — retry rather than fail.
		if resp.StatusCode >= 400 && isHTMLResponse(resp) {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if wait, ok := g.retryBackoff(attempt); ok {
				attempt++
				if g.log != nil {
					g.log.Printf("github %s %s: HTTP %d with an HTML body (edge error); retrying in %s (attempt %d/%d)", method, shortURL(fullURL), resp.StatusCode, wait.Round(time.Second), attempt, g.maxRetries)
				}
				if serr := sleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		// A 401 is often transient (a spurious GitHub error, or a gh OAuth token
		// that just rotated). Refresh the token and retry a bounded number of times
		// before surfacing it as a real auth failure.
		if resp.StatusCode == http.StatusUnauthorized {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if wait, ok := g.retryBackoff(attempt); ok {
				g.refreshToken(ctx)
				attempt++
				if g.log != nil {
					g.log.Printf("github %s %s: 401 unauthorized; refreshing token and retrying in %s (attempt %d/%d)", method, shortURL(fullURL), wait.Round(time.Second), attempt, g.maxRetries)
				}
				if serr := sleepCtx(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		rl := rateLimitFrom(resp, string(b))
		if rl == nil {
			return nil, &APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(b)}
		}
		rl.Method, rl.URL = method, fullURL
		wait, ok := g.backoffWait(rl, attempt)
		if !ok {
			return nil, rl
		}
		attempt++
		if g.log != nil {
			g.log.Printf("github %s rate limit on %s %s; backing off %s (attempt %d/%d)", rl.Kind, method, shortURL(fullURL), wait.Round(time.Second), attempt, g.maxRetries)
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// networkRetryWait is exponential backoff that plateaus at 30s, so during a long
// outage crq keeps probing every ~30s until connectivity returns.
func networkRetryWait(base time.Duration, attempt int) time.Duration {
	shift := attempt
	if shift > 5 {
		shift = 5
	}
	wait := base << uint(shift)
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}

// backoffWait computes how long to wait before the next rate-limited retry:
// honor the reset hint when present, else exponential backoff. ok is false when
// the retry budget is exhausted (too many attempts, or a single wait exceeding
// maxWait), signalling the caller to surface the RateLimitError instead.
func (g *GitHub) backoffWait(rl *RateLimitError, attempt int) (time.Duration, bool) {
	var wait time.Duration
	if rl.Until.IsZero() {
		wait = g.backoffBase << uint(attempt) // 2s, 4s, 8s, ... for hint-less secondary limits
	} else {
		wait = time.Until(rl.Until) + time.Second // clock-skew buffer
	}
	if wait < 0 {
		wait = 0
	}
	if attempt >= g.maxRetries || wait > g.maxWait {
		return 0, false
	}
	return wait, true
}

// retryBackoff is the exponential backoff for transient network / 5xx retries,
// clamped to maxWait and bounded by maxRetries. Unlike rate-limit backoff it
// clamps (rather than gives up) so a brief outage gets the full wait.
func (g *GitHub) retryBackoff(attempt int) (time.Duration, bool) {
	if attempt >= g.maxRetries {
		return 0, false
	}
	wait := g.backoffBase << uint(attempt)
	if wait > g.maxWait {
		wait = g.maxWait
	}
	return wait, true
}

func isHTMLResponse(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html")
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// isRetryableNetErr reports whether a transport error is a transient network
// failure worth retrying (timeouts, refused/reset connections, DNS hiccups, TLS
// handshake failures, short EOFs). Callers must rule out ctx cancellation first.
func isRetryableNetErr(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"timeout", "deadline exceeded", "connection refused", "connection reset",
		"no such host", "network is unreachable", "host is unreachable",
		"tls handshake", "i/o timeout", "broken pipe", "server misbehaving",
		"temporary failure", "unexpected eof", "connection closed", "eof",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func shortURL(u string) string {
	return strings.TrimPrefix(u, "https://api.github.com")
}

func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 || !strings.Contains(sections[1], `rel="next"`) {
			continue
		}
		raw := strings.TrimSpace(sections[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		return raw
	}
	return ""
}

func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func addQuery(path string, values url.Values) string {
	if strings.Contains(path, "?") {
		return path + "&" + values.Encode()
	}
	return path + "?" + values.Encode()
}

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

type Pull struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Merged bool `json:"merged"`
}

type RepoInfo struct {
	DefaultBranch string `json:"default_branch"`
}

type IssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Review struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
	HTMLURL     string    `json:"html_url"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

type ReviewComment struct {
	ID                  int64     `json:"id"`
	PullRequestReviewID int64     `json:"pull_request_review_id"`
	Body                string    `json:"body"`
	Path                string    `json:"path"`
	Line                int       `json:"line"`
	OriginalLine        int       `json:"original_line"`
	CommitID            string    `json:"commit_id"`
	OriginalCommitID    string    `json:"original_commit_id"`
	URL                 string    `json:"html_url"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	User                struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Reaction struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (g *GitHub) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	var out Issue
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repoPath(repo), number), nil, &out)
	return out, err
}

func (g *GitHub) PatchIssue(ctx context.Context, repo string, number int, title, body string) error {
	in := map[string]string{"title": title, "body": body}
	return g.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repoPath(repo), number), in, nil)
}

func (g *GitHub) CreateIssue(ctx context.Context, repo, title, body string) (Issue, error) {
	var out Issue
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", repoPath(repo)), map[string]string{
		"title": title,
		"body":  body,
	}, &out)
	return out, err
}

func (g *GitHub) GetPull(ctx context.Context, repo string, pr int) (Pull, error) {
	var out Pull
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repoPath(repo), pr), nil, &out)
	return out, err
}

func (g *GitHub) ListPulls(ctx context.Context, repo string, query url.Values) ([]Pull, error) {
	var out []Pull
	path := fmt.Sprintf("/repos/%s/pulls?per_page=100", repoPath(repo))
	if len(query) > 0 {
		path += "&" + query.Encode()
	}
	err := g.requestPaged(ctx, path, &out)
	return out, err
}

func (g *GitHub) CreatePull(ctx context.Context, repo, base, head, title, body string, draft bool) (Pull, error) {
	var out Pull
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls", repoPath(repo)), map[string]any{
		"base":  base,
		"head":  head,
		"title": title,
		"body":  body,
		"draft": draft,
	}, &out)
	return out, err
}

func (g *GitHub) ListIssueComments(ctx context.Context, repo string, issue int) ([]IssueComment, error) {
	var out []IssueComment
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repoPath(repo), issue), &out)
	return out, err
}

// ListIssueCommentsPage fetches a single page (GitHub returns oldest-first), so
// callers that only need the oldest comments (e.g. calibration pruning) don't
// page through thousands of them.
func (g *GitHub) ListIssueCommentsPage(ctx context.Context, repo string, issue, page, perPage int) ([]IssueComment, error) {
	var out []IssueComment
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d", repoPath(repo), issue, perPage, page), nil, &out)
	return out, err
}

func (g *GitHub) DeleteIssueComment(ctx context.Context, repo string, commentID int64) error {
	return g.request(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/comments/%d", repoPath(repo), commentID), nil, nil)
}

func (g *GitHub) ListReviewComments(ctx context.Context, repo string, pr int) ([]ReviewComment, error) {
	var out []ReviewComment
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", repoPath(repo), pr), &out)
	return out, err
}

func (g *GitHub) ListReviews(ctx context.Context, repo string, pr int) ([]Review, error) {
	var out []Review
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repoPath(repo), pr), &out)
	return out, err
}

func (g *GitHub) PostIssueComment(ctx context.Context, repo string, issue int, body string) (IssueComment, error) {
	var out IssueComment
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repoPath(repo), issue), map[string]string{"body": body}, &out)
	return out, err
}

func (g *GitHub) ListCommentReactions(ctx context.Context, repo string, commentID int64) ([]Reaction, error) {
	var out []Reaction
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/issues/comments/%d/reactions?per_page=100", repoPath(repo), commentID), &out)
	return out, err
}

func (g *GitHub) SearchOpenPRs(ctx context.Context, target string, byRepo bool, limit int) ([]SearchPR, error) {
	var out []SearchPR
	query := "type:pr state:open archived:false "
	if byRepo {
		query += "repo:" + target
	} else {
		query += "user:" + target
	}
	page := 1
	for len(out) < limit {
		values := url.Values{}
		values.Set("q", query)
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		values.Set("sort", "updated")
		values.Set("order", "desc")
		var result struct {
			Items []struct {
				Number        int    `json:"number"`
				RepositoryURL string `json:"repository_url"`
			} `json:"items"`
		}
		if err := g.request(ctx, http.MethodGet, "/search/issues?"+values.Encode(), nil, &result); err != nil {
			return out, err
		}
		if len(result.Items) == 0 {
			break
		}
		for _, item := range result.Items {
			repo := strings.TrimPrefix(item.RepositoryURL, "https://api.github.com/repos/")
			if repo != "" {
				out = append(out, SearchPR{Repo: repo, Number: item.Number})
			}
			if len(out) >= limit {
				break
			}
		}
		if len(result.Items) < 100 {
			break
		}
		page++
	}
	return out, nil
}

type SearchPR struct {
	Repo   string
	Number int
}

type gitRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

type gitCommit struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Committer struct {
		Date time.Time `json:"date"`
	} `json:"committer"`
}

type gitTree struct {
	SHA  string `json:"sha"`
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"tree"`
}

type gitBlob struct {
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (g *GitHub) GetRef(ctx context.Context, repo, ref string) (string, error) {
	var out gitRef
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/ref/heads/%s", repoPath(repo), refPath(ref)), nil, &out)
	if err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (g *GitHub) CreateRef(ctx context.Context, repo, ref, sha string) error {
	return g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/refs", repoPath(repo)), map[string]string{
		"ref": "refs/heads/" + ref,
		"sha": sha,
	}, nil)
}

func (g *GitHub) UpdateRef(ctx context.Context, repo, ref, sha string, force bool) error {
	return g.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/git/refs/heads/%s", repoPath(repo), refPath(ref)), map[string]any{
		"sha":   sha,
		"force": force,
	}, nil)
}

func (g *GitHub) CreateBlob(ctx context.Context, repo string, content []byte) (string, error) {
	var out gitBlob
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/blobs", repoPath(repo)), map[string]string{
		"content":  string(content),
		"encoding": "utf-8",
	}, &out)
	return out.SHA, err
}

func (g *GitHub) CreateTree(ctx context.Context, repo, baseTree string, entries []map[string]any) (string, error) {
	in := map[string]any{"tree": entries}
	if baseTree != "" {
		in["base_tree"] = baseTree
	}
	var out gitTree
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/trees", repoPath(repo)), in, &out)
	return out.SHA, err
}

func (g *GitHub) CreateCommit(ctx context.Context, repo, message, tree string, parents []string) (string, error) {
	var out gitCommit
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/commits", repoPath(repo)), map[string]any{
		"message": message,
		"tree":    tree,
		"parents": parents,
	}, &out)
	return out.SHA, err
}

func (g *GitHub) GetCommit(ctx context.Context, repo, sha string) (gitCommit, error) {
	var out gitCommit
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/commits/%s", repoPath(repo), url.PathEscape(sha)), nil, &out)
	return out, err
}

func (g *GitHub) GetTree(ctx context.Context, repo, sha string) (gitTree, error) {
	var out gitTree
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", repoPath(repo), url.PathEscape(sha)), nil, &out)
	return out, err
}

func (g *GitHub) GetBlob(ctx context.Context, repo, sha string) ([]byte, error) {
	var out gitBlob
	if err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/blobs/%s", repoPath(repo), url.PathEscape(sha)), nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	}
	return []byte(out.Content), nil
}

func (g *GitHub) RepoExists(ctx context.Context, repo string) (bool, error) {
	err := g.request(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, nil)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (g *GitHub) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var out RepoInfo
	err := g.request(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, &out)
	return out, err
}

func refPath(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
