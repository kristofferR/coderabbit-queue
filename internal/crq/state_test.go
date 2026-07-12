package crq

import (
	"strings"
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

func TestNormalizeProtectsAwaitingFeedbackMarkers(t *testing.T) {
	cfg := Config{FiredMax: 1, FeedbackWaitTimeout: time.Minute}
	now := time.Now().UTC()
	st := State{
		Fired: map[string]string{
			"owner/aaa#1": "head-aaa",
			"owner/zzz#1": "head-zzz",
		},
		AwaitingFeedback: map[string]FeedbackWait{
			"owner/aaa#1": {
				Repo:      "owner/aaa",
				PR:        1,
				Head:      "head-aaa",
				StartedAt: now,
				Deadline:  now.Add(time.Minute),
			},
		},
	}
	st.Normalize(cfg)

	if st.Fired["owner/aaa#1"] != "head-aaa" {
		t.Fatalf("awaiting feedback marker must protect its fired dedupe marker: %#v", st.Fired)
	}
	if len(st.Fired) != cfg.FiredMax {
		t.Fatalf("expected FiredMax trimming to keep exactly one marker, got %#v", st.Fired)
	}
}

func TestNormalizeRestoresFiredFromAwaitingFeedback(t *testing.T) {
	now := time.Now().UTC()
	st := State{
		AwaitingFeedback: map[string]FeedbackWait{
			"Owner/Repo#7": {
				Head:      "abcdef123",
				StartedAt: now,
				Deadline:  now.Add(time.Minute),
			},
		},
	}
	st.Normalize(Config{FiredMax: 500})

	key := QueueKey("owner/repo", 7)
	if st.Fired[key] != "abcdef123" {
		t.Fatalf("normalization should restore the fired marker from awaiting feedback, got %#v", st.Fired)
	}
	if wait := st.AwaitingFeedback[key]; wait.Repo != "owner/repo" || wait.PR != 7 {
		t.Fatalf("awaiting feedback key/fields should normalize together, got %#v", st.AwaitingFeedback)
	}
}

func TestDashboardLabelsFireHistoryAsRequested(t *testing.T) {
	state := DefaultState(Config{})
	state.History = []HistoryItem{{Repo: "owner/repo", PR: 7, Commit: "abcdef123", At: time.Now().UTC(), Host: "host"}}
	dashboard := renderDashboard(state, Config{})

	if !strings.Contains(dashboard, "Recently requested") || !strings.Contains(dashboard, "| PR | commit | requested | host |") {
		t.Fatalf("fire history must be labeled as requested, got:\n%s", dashboard)
	}
	if strings.Contains(dashboard, "Recently reviewed") {
		t.Fatalf("fire history must not claim reviews completed, got:\n%s", dashboard)
	}
}
