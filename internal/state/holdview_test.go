package state

import (
	"strings"
	"testing"
	"time"
)

// Holding an untracked PR is the ordinary case — Enqueue deliberately refuses to
// create a round for one — so a view built from rounds showed nothing, and the
// dashboard said "Idle" immediately after a hold succeeded.
func TestHeldViewShowsAHoldWithNoRound(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	st := New()
	st.Hold("Owner/Repo", 12, "waiting on a decision", "cachyos", now)

	held := heldRounds(st)
	if len(held) != 1 {
		t.Fatalf("heldRounds = %+v, want the standalone hold", held)
	}
	if held[0].PR != 12 || held[0].Repo != "owner/repo" {
		t.Errorf("entry = %s#%d, want owner/repo#12", held[0].Repo, held[0].PR)
	}

	body := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(body, "Held — 1") {
		t.Error("the dashboard does not count a hold with no round")
	}
	if !strings.Contains(body, "owner/repo#12") {
		t.Error("the dashboard does not name the held PR")
	}
}

// A hold reason is whatever an operator typed. A pipe ends the column early and
// a newline ends the row, so the table rewrites itself around the text.
func TestHoldReasonCannotRewriteTheTable(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	st := New()
	st.Hold("owner/repo", 3, "waiting on API | security decision\nand more", "host|name", now)

	body := RenderDashboard(st, StoreConfig{})
	var row string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "owner/repo#3") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("the held row is missing")
	}
	// Six columns means seven pipes; an unescaped one in the reason adds more.
	if got := strings.Count(row, "|") - strings.Count(row, `\|`); got != 7 {
		t.Errorf("row has %d structural pipes, want 7: %s", got, row)
	}
	if !strings.Contains(row, "and more") {
		t.Errorf("the reason was truncated instead of flattened: %s", row)
	}
}

// A queued round has never reserved the fire slot, so it has no host — which is
// every row of the queue table. Rendering that as code produced an empty pair of
// backticks in the issue, which reads as a bug in crq rather than as "nobody has
// taken this yet".
func TestTheHostColumnSaysNobodyRatherThanNothing(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	st := New()
	if _, err := st.NewRound("owner/repo", 7, "aaaaaaaa1", now); err != nil {
		t.Fatal(err)
	}

	body := RenderDashboard(st, StoreConfig{})
	var row string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "owner/repo#7") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("the queued round is missing from the dashboard")
	}
	if strings.Contains(row, "``") {
		t.Errorf("the host column rendered as empty backticks: %s", row)
	}
	if !strings.HasSuffix(strings.TrimSpace(row), "— |") {
		t.Errorf("the host column does not say there is no host: %s", row)
	}
}

// Reviewers are per repository, so the header row is a FLEET DEFAULT. A bare
// list claims more than it knows: a reader of the issue sees three bots and has
// no way to tell that one repository has been set to something else.
func TestCoReviewerRowSaysItIsTheDefaultAndNamesExceptions(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := StoreConfig{CoReviewers: "codex (required, always) · bugbot (selfheal)"}

	st := New()
	body := RenderDashboard(st, cfg)
	if !strings.Contains(body, "fleet default") {
		t.Error("the row does not say the list is a default")
	}

	// Once a repository overrides it, the row names which.
	st.SetRepoOverride("owner/special", RepoReviewers{SetCoBots: true, UpdatedAt: &now})
	body = RenderDashboard(st, cfg)
	if !strings.Contains(body, "owner/special") {
		t.Errorf("the row does not name the repository that differs:\n%s", body)
	}
	if !strings.Contains(body, "override") {
		t.Error("the row does not say the named repository overrides the default")
	}

	// Many overrides are truncated rather than allowed to wrap the table.
	for _, repo := range []string{"owner/a", "owner/b", "owner/c", "owner/d"} {
		st.SetRepoOverride(repo, RepoReviewers{SetCoBots: true, UpdatedAt: &now})
	}
	body = RenderDashboard(st, cfg)
	if !strings.Contains(body, "+2 more") {
		t.Errorf("five overrides were not summarised:\n%s", body)
	}
}
