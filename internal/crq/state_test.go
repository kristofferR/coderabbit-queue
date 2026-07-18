package crq

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDashboardLabelsFireHistoryAsRequested(t *testing.T) {
	cfg := Config{FeedbackWaitTimeout: time.Hour}
	store := NewMemoryStore(cfg)
	seedRound(t, store, cfg, "owner/repo", 7, "abcdef123", PhaseFired, time.Now().UTC(), 1)
	state, _, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dashboard := renderDashboard(state, cfg)

	if !strings.Contains(dashboard, "Recently requested") || !strings.Contains(dashboard, "| PR | commit | requested | host |") {
		t.Fatalf("fire history must be labeled as requested, got:\n%s", dashboard)
	}
	if strings.Contains(dashboard, "Recently reviewed") {
		t.Fatalf("fire history must not claim reviews completed, got:\n%s", dashboard)
	}
}
