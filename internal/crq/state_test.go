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

func TestNormalizeCapsProtectedHistoryMarkers(t *testing.T) {
	cfg := Config{FiredMax: 2}
	now := time.Now().UTC()
	st := State{
		Fired: map[string]string{
			"owner/old#1":    "head-old",
			"owner/newest#1": "head-newest",
			"owner/middle#1": "head-middle",
			"owner/extra#1":  "head-extra",
		},
		History: []HistoryItem{
			{Repo: "owner/old", PR: 1, Commit: "head-old", At: now.Add(-2 * time.Minute)},
			{Repo: "owner/newest", PR: 1, Commit: "head-newest", At: now},
			{Repo: "owner/middle", PR: 1, Commit: "head-middle", At: now.Add(-time.Minute)},
		},
	}
	st.Normalize(cfg)

	if len(st.Fired) != cfg.FiredMax {
		t.Fatalf("expected exactly FiredMax markers, got %d: %#v", len(st.Fired), st.Fired)
	}
	if st.Fired["owner/newest#1"] != "head-newest" || st.Fired["owner/middle#1"] != "head-middle" {
		t.Fatalf("expected the most recent matching history markers to survive: %#v", st.Fired)
	}
	if _, ok := st.Fired["owner/old#1"]; ok {
		t.Fatalf("older history marker should not exceed FiredMax protection budget: %#v", st.Fired)
	}
}
