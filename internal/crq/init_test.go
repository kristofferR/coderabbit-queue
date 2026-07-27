package crq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

func TestInitSyncsDashboardWithFleetReviewers(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedConfig(t, map[string]string{
		"CRQ_REPO":          "owner/gate",
		"CRQ_ISSUE":         "7",
		"CRQ_CAL_PR":        "8",
		"CRQ_COBOTS":        "codex",
		"CRQ_REQUIRED_BOTS": "coderabbitai[bot]",
	})
	inner := NewMemoryStore(cfg)
	if _, err := inner.Update(ctx, func(st *State) error {
		st.SetFleetValue("required-bots", "coderabbitai[bot],"+dialect.CodexBotLogin)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store := &dashboardConfigStore{StateStore: inner}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	if _, err := Init(ctx, cfg, ghapi.NewTestClient(server.URL, server.Client()), store); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(store.render.CoReviewers, "codex (required") {
		t.Fatalf("dashboard co-reviewers = %q, want fleet-required codex", store.render.CoReviewers)
	}
}
