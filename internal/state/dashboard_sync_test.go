package state

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	gh "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// issueServer is a minimal gate-issue endpoint that records how it was used.
// The counts ARE the assertions: PATCHes are charged writes against a shared
// account budget and are the endpoint class that trips GitHub's secondary
// limits, so "how many writes did this cost" is the property under test.
type issueServer struct {
	mu          sync.Mutex
	patches     int
	gets        int
	notModified int
	title       string
	body        string
	etag        string
}

func (s *issueServer) start(t *testing.T) (*httptest.Server, *gh.GitHub) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/issues/") {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodPatch:
			var payload struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.patches++
			s.title, s.body = payload.Title, payload.Body
			s.etag = ""
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": s.title, "body": s.body})
		case http.MethodGet:
			s.gets++
			if s.etag == "" {
				s.etag = `"v` + strconv.Itoa(s.patches) + `"`
			}
			w.Header().Set("ETag", s.etag)
			// Model the real endpoint: an unchanged issue answers 304 and costs
			// no quota, which is what makes reading-before-writing affordable.
			if r.Header.Get("If-None-Match") == s.etag {
				s.notModified++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": s.title, "body": s.body, "state": "open"})
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, gh.NewTestClient(srv.URL, srv.Client())
}

func syncTestState(t *testing.T) State {
	t.Helper()
	st := New()
	st.Account.Scope = "owner"
	if _, err := st.NewRound("owner/repo", 7, "abcdef123", t0); err != nil {
		t.Fatal(err)
	}
	st.Normalize(t0)
	return st
}

// Every crq command that touches state syncs the dashboard, including
// read-mostly paths like Enqueue that usually change nothing. An unconditional
// PATCH there made a plain `crq next` a write, so an unchanged sync must cost
// nothing.
func TestSyncDashboardWritesOnlyOnChange(t *testing.T) {
	srv := &issueServer{}
	_, client := srv.start(t)
	cfg := StoreConfig{GateRepo: "owner/state", StateRef: "crq-state-v3", DashboardIssue: 1, Scope: []string{"owner"}}
	store := NewGitStateStore(cfg, client, nil)
	ctx := context.Background()

	st := syncTestState(t)
	for i := 0; i < 4; i++ {
		if err := store.SyncDashboard(ctx, st, cfg); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	srv.mu.Lock()
	patches := srv.patches
	srv.mu.Unlock()
	if patches != 1 {
		t.Fatalf("four identical syncs wrote %d times, want exactly 1", patches)
	}
	srv.mu.Lock()
	notModified := srv.notModified
	srv.mu.Unlock()
	if notModified == 0 {
		t.Error("the repeat syncs must revalidate conditionally, so they cost no quota")
	}

	// Real movement must still reach the issue.
	if _, err := st.NewRound("owner/repo", 8, "beefcafe1", t0); err != nil {
		t.Fatal(err)
	}
	st.Normalize(t0)
	if err := store.SyncDashboard(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	patches = srv.patches
	srv.mu.Unlock()
	if patches != 2 {
		t.Fatalf("a changed dashboard wrote %d times total, want 2", patches)
	}
}

// A short-lived `crq next` starts cold every time, so the check has to work
// across processes: if the daemon already wrote this exact content, there is
// nothing left to write.
func TestSyncDashboardSkipsWriteWhenTheIssueAlreadyMatches(t *testing.T) {
	srv := &issueServer{}
	httpSrv, client := srv.start(t)
	cfg := StoreConfig{GateRepo: "owner/state", StateRef: "crq-state-v3", DashboardIssue: 1, Scope: []string{"owner"}}
	ctx := context.Background()
	st := syncTestState(t)

	// One process (think: the daemon) publishes the dashboard.
	if err := NewGitStateStore(cfg, client, nil).SyncDashboard(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	after := srv.patches
	srv.mu.Unlock()
	if after != 1 {
		t.Fatalf("initial publish wrote %d times, want 1", after)
	}

	// A genuinely cold second process against the same issue: its own client, so
	// it shares no ETag cache with the first.
	cold := NewGitStateStore(cfg, gh.NewTestClient(httpSrv.URL, httpSrv.Client()), nil)
	if err := cold.SyncDashboard(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.patches != 1 {
		t.Fatalf("a cold process re-wrote an already-current dashboard (%d writes), want 1", srv.patches)
	}
	if srv.gets == 0 {
		t.Fatal("the cross-process check must actually read the issue")
	}
}

func TestSyncDashboardUsesTheSuppliedEffectiveRenderingConfig(t *testing.T) {
	srv := &issueServer{}
	_, client := srv.start(t)
	startup := StoreConfig{
		GateRepo: "owner/state", StateRef: "crq-state-v3", DashboardIssue: 1,
		Scope: []string{"owner"}, CoReviewers: "codex (selfheal)",
	}
	effective := startup
	effective.CoReviewers = "codex (required, always)"
	st := syncTestState(t)

	if err := NewGitStateStore(startup, client, nil).SyncDashboard(context.Background(), st, effective); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !strings.Contains(srv.body, effective.CoReviewers) || strings.Contains(srv.body, startup.CoReviewers) {
		t.Fatalf("dashboard body did not use the effective reviewer summary:\n%s", srv.body)
	}
}

// dashboard.md is rewritten on EVERY state commit, so it has to be rendered
// with the same effective policy the gate issue is. Rendering it from the
// writing host's startup configuration would put that machine's co-reviewer set
// in the shared file — disagreeing with the issue, and flapping between hosts
// that are each configured differently.
func TestStateCommitsRenderTheDashboardFileWithTheEffectiveConfig(t *testing.T) {
	var blobs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/"):
			http.NotFound(w, r) // no state ref yet: the first write creates it
		case strings.HasSuffix(r.URL.Path, "/git/blobs"):
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			blobs = append(blobs, payload.Content)
			enc.Encode(map[string]string{"sha": "blobsha"})
		case strings.HasSuffix(r.URL.Path, "/git/trees"):
			enc.Encode(map[string]string{"sha": "treesha"})
		case strings.HasSuffix(r.URL.Path, "/git/commits"):
			enc.Encode(map[string]string{"sha": "commitsha"})
		case strings.HasSuffix(r.URL.Path, "/git/refs"):
			enc.Encode(map[string]any{"object": map[string]string{"sha": "commitsha"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	startup := StoreConfig{
		GateRepo: "owner/state", StateRef: "crq-state-v3", DashboardIssue: 1,
		Scope: []string{"owner"}, CoReviewers: "codex (selfheal)",
	}
	store := NewGitStateStore(startup, gh.NewTestClient(srv.URL, srv.Client()), nil)
	store.SetRenderConfig(func(State) StoreConfig {
		effective := startup
		effective.CoReviewers = "codex (required, always)"
		return effective
	})
	if _, err := store.Update(context.Background(), func(st *State) error {
		_, err := st.NewRound("owner/repo", 7, "abcdef123", t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	written := strings.Join(blobs, "\n")
	if !strings.Contains(written, "codex (required, always)") || strings.Contains(written, "codex (selfheal)") {
		t.Fatalf("the committed dashboard did not use the effective reviewer summary:\n%s", written)
	}
}
