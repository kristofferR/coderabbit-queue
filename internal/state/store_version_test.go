package state

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// refServer serves the four git-data reads Load makes, over one state payload.
func refServer(t *testing.T, payload string) *gh.GitHub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/"):
			enc.Encode(map[string]any{"object": map[string]string{"sha": "commitsha"}})
		case strings.Contains(r.URL.Path, "/git/commits/"):
			enc.Encode(map[string]any{"sha": "commitsha", "tree": map[string]string{"sha": "treesha"}})
		case strings.Contains(r.URL.Path, "/git/trees/"):
			enc.Encode(map[string]any{"sha": "treesha", "tree": []map[string]string{
				{"path": statePath, "type": "blob", "sha": "blobsha"},
			}})
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			enc.Encode(map[string]any{"sha": "blobsha", "encoding": "utf-8", "content": payload})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return gh.NewTestClient(srv.URL, srv.Client())
}

func versionStore(t *testing.T, payload string) *GitStateStore {
	t.Helper()
	cfg := StoreConfig{GateRepo: "owner/state", StateRef: "crq-state-v3", DashboardIssue: 1, Scope: []string{"owner"}}
	return NewGitStateStore(cfg, refServer(t, payload), nil)
}

// The fleet runs mixed binary versions during a rolling deploy. An old binary
// meeting state a newer one wrote used to reinitialize — so the first stale
// process to wake up erased every live round in the account, silently, in the
// exact situation the version field exists to detect. It must refuse instead.
func TestLoadRefusesStateFromANewerBinary(t *testing.T) {
	store := versionStore(t, `{"v":99,"rounds":{"owner/repo#7":{"repo":"owner/repo","pr":7,"phase":"fired"}}}`)

	_, _, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("a newer schema must be refused, not reinitialized over")
	}
	// The message has to be actionable: which ref, and how to reset on purpose.
	for _, want := range []string{"crq-state-v3", "v99", "Upgrade crq"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A v3 payload this binary cannot decode is a shape change inside the current
// version, not an obsolete one — the rounds it describes are live.
func TestLoadRefusesUndecodableCurrentState(t *testing.T) {
	if _, _, err := versionStore(t, `{"v":3,"rounds":"not-a-map"}`).Load(context.Background()); err == nil {
		t.Fatal("an undecodable v3 payload must be refused, not reinitialized over")
	}
	if _, _, err := versionStore(t, `not json at all`).Load(context.Background()); err == nil {
		t.Fatal("an unparseable payload must be refused")
	}
}

// An OLDER payload is genuinely obsolete: crq is pre-release, there is no
// migration, and a v2 state describes a world this binary cannot act on.
func TestLoadReinitializesOlderState(t *testing.T) {
	st, _, err := versionStore(t, `{"v":2,"queue":[{"repo":"owner/repo","pr":7}]}`).Load(context.Background())
	if err != nil {
		t.Fatalf("an older schema must reinitialize, got %v", err)
	}
	if st.Version != SchemaVersion {
		t.Errorf("version = %d, want a fresh v%d", st.Version, SchemaVersion)
	}
	if len(st.Rounds) != 0 {
		t.Errorf("rounds = %v, want none carried from v2", st.Rounds)
	}
}
