package crq

import (
	"testing"
	"time"
)

func TestNormalizeKeepsRecentlyFiredMarkers(t *testing.T) {
	cfg := Config{FiredMax: 2}
	now := time.Now().UTC()
	st := State{
		Fired: map[string]string{
			"owner/aaa#1": "head-aaa", // sorts first; a plain lexicographic trim would drop it
			"owner/mmm#1": "head-mmm",
			"owner/zzz#1": "head-zzz",
		},
		// History marks owner/aaa#1 as just-fired — it must survive the trim.
		History: []HistoryItem{{Repo: "owner/aaa", PR: 1, Commit: "head-aaa", At: now}},
	}
	st.Normalize(cfg)

	if st.Fired["owner/aaa#1"] != "head-aaa" {
		t.Fatalf("recently-fired marker was evicted by the FiredMax trim: %#v", st.Fired)
	}
	if len(st.Fired) > cfg.FiredMax {
		t.Fatalf("expected at most FiredMax markers after protection, got %d: %#v", len(st.Fired), st.Fired)
	}
}
