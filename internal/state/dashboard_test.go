package state

import (
	"strings"
	"testing"
	"time"
)

const nothingQueued = "_Nothing queued._"

func ptime(t time.Time) *time.Time { return &t }

// parkedRound is the shape that used to vanish from the dashboard: an
// awaiting_retry round whose RetryAt has not passed yet.
func parkedRound(repo string, pr int, seq int64, now time.Time) Round {
	return Round{
		Repo:       repo,
		PR:         pr,
		Head:       "a9c688a1c",
		Seq:        seq,
		Phase:      PhaseAwaitingRetry,
		Attempts:   1,
		EnqueuedAt: now.Add(-10 * time.Minute),
		FiredAt:    ptime(now.Add(-9 * time.Minute)),
		RetryAt:    ptime(now.Add(11 * time.Minute)),
		ByHost:     "cachyos",
	}
}

func stateWith(rounds ...Round) State {
	st := New()
	for _, r := range rounds {
		st.PutRound(r)
	}
	return st
}

// A parked-only state must not render as an empty queue: that is the bug this
// section exists to prevent (a reader concluded their enqueue was dropped).
func TestRenderDashboardParkedOnly(t *testing.T) {
	now := time.Now().UTC()
	r := parkedRound("kristofferr/ha-adjustable-bed", 480, 1, now)
	st := stateWith(r)

	out := RenderDashboard(st, StoreConfig{})
	if strings.Contains(out, nothingQueued) {
		t.Fatalf("parked round rendered as an empty queue:\n%s", out)
	}
	for _, want := range []string{
		"kristofferr/ha-adjustable-bed#480",
		"https://github.com/kristofferr/ha-adjustable-bed/pull/480",
		"`a9c688a1c`",
		fmtStamp(r.RetryAt, time.UTC), // absolute retry_at, not a relative "in 11m"
		"`cachyos`",
		"1 parked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard missing %q:\n%s", want, out)
		}
	}

	if title := RenderTitle(st); strings.Contains(title, "idle") {
		t.Errorf("parked-only state titled idle: %q", title)
	}
}

// Guard against over-correcting: a genuinely empty state keeps its empty-state
// text and its idle title.
func TestRenderDashboardEmpty(t *testing.T) {
	st := New()

	out := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(out, nothingQueued) {
		t.Errorf("empty state lost its empty-state text:\n%s", out)
	}
	if strings.Contains(out, "Parked") {
		t.Errorf("empty state rendered a parked section:\n%s", out)
	}
	if got, want := RenderTitle(st), "🐰 crq — idle"; got != want {
		t.Errorf("RenderTitle = %q, want %q", got, want)
	}
}

// A fire-eligible round and a parked one land in their own sections, each
// counted exactly once.
func TestRenderDashboardMixed(t *testing.T) {
	now := time.Now().UTC()
	queued := Round{
		Repo:       "kristofferr/coderabbit-queue",
		PR:         12,
		Head:       "beefbeef1",
		Seq:        1,
		Phase:      PhaseQueued,
		EnqueuedAt: now.Add(-time.Minute),
		ByHost:     "cachyos",
	}
	st := stateWith(queued, parkedRound("kristofferr/ha-adjustable-bed", 480, 2, now))

	out := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(out, "## ⏳ Queue — 1 waiting · 1 parked") {
		t.Errorf("queue heading does not report both counts:\n%s", out)
	}
	if strings.Contains(out, nothingQueued) {
		t.Errorf("non-empty queue rendered the empty state:\n%s", out)
	}

	queueBody, parkedBody, ok := strings.Cut(out, "### 🅿️ Parked — 1 not yet eligible")
	if !ok {
		t.Fatalf("no parked section:\n%s", out)
	}
	if !strings.Contains(queueBody, "coderabbit-queue#12") {
		t.Errorf("fire-eligible round missing from the queue table:\n%s", queueBody)
	}
	if strings.Contains(queueBody, "ha-adjustable-bed#480") {
		t.Errorf("parked round leaked into the queue table:\n%s", queueBody)
	}
	if strings.Contains(parkedBody, "coderabbit-queue#12") {
		t.Errorf("fire-eligible round double-counted in the parked table:\n%s", parkedBody)
	}

	if got, want := RenderTitle(st), "🐰 crq — 1 queued · 1 parked"; got != want {
		t.Errorf("RenderTitle = %q, want %q", got, want)
	}
}

// An awaiting_retry round whose window has opened is fire-eligible: it belongs
// in the normal queue, not the parked view.
func TestRenderDashboardRetryWindowOpen(t *testing.T) {
	now := time.Now().UTC()
	r := parkedRound("kristofferr/ha-adjustable-bed", 480, 1, now)
	r.RetryAt = ptime(now.Add(-time.Minute))
	st := stateWith(r)

	out := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(out, "## ⏳ Queue — 1 waiting\n") {
		t.Errorf("elapsed retry window not counted as waiting:\n%s", out)
	}
	if strings.Contains(out, "Parked") {
		t.Errorf("fire-eligible round rendered as parked:\n%s", out)
	}
	if got, want := RenderTitle(st), "🐰 crq — 1 queued"; got != want {
		t.Errorf("RenderTitle = %q, want %q", got, want)
	}
}

// Every active round must appear in one of "In flight", "Feedback wait",
// "Queue" or "Parked" — the blind spot that hid parked rounds also hid a
// reserved round whose fire slot was cleared and every reviewing round past
// the first.
func TestRenderDashboardAccountsForEveryActiveRound(t *testing.T) {
	now := time.Now().UTC()
	fired := Round{
		Repo: "kristofferr/a", PR: 1, Head: "aaaaaaaa1", Seq: 1,
		Phase: PhaseFired, EnqueuedAt: now.Add(-5 * time.Minute),
		FiredAt: ptime(now.Add(-4 * time.Minute)), ByHost: "cachyos",
	}
	reviewing := Round{
		Repo: "kristofferr/b", PR: 2, Head: "bbbbbbbb2", Seq: 2,
		Phase: PhaseReviewing, EnqueuedAt: now.Add(-3 * time.Minute),
		FiredAt: ptime(now.Add(-2 * time.Minute)), ByHost: "cachyos",
	}
	reserved := Round{ // slot-less reserved round (FireSlot cleared by Normalize)
		Repo: "kristofferr/c", PR: 3, Head: "cccccccc3", Seq: 3,
		Phase: PhaseReserved, EnqueuedAt: now.Add(-time.Minute),
		ReservedAt: ptime(now), ByHost: "cachyos",
	}
	st := stateWith(fired, reviewing, reserved,
		parkedRound("kristofferr/d", 4, 4, now))

	out := RenderDashboard(st, StoreConfig{})
	// The "Recently requested" history lists fired rounds regardless of phase,
	// so it must not count as accounting for a live round.
	live, _, _ := strings.Cut(out, "## 📨 Recently requested")
	for _, r := range st.Rounds {
		if !r.Active() {
			continue
		}
		if !strings.Contains(live, Key(r.Repo, r.PR)) {
			t.Errorf("active round %s appears nowhere on the dashboard:\n%s", Key(r.Repo, r.PR), out)
		}
	}
	if !strings.Contains(out, "3 not yet eligible") {
		t.Errorf("want reviewing tail + reserved + parked in the parked table:\n%s", out)
	}
}

// The parked table honours CRQ_TZ via the shared fmtStamp helper.
func TestRenderDashboardParkedHonoursTimezone(t *testing.T) {
	now := time.Now().UTC()
	r := parkedRound("kristofferr/ha-adjustable-bed", 480, 1, now)
	st := stateWith(r)

	loc, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	out := RenderDashboard(st, StoreConfig{Timezone: "Europe/Oslo"})
	if !strings.Contains(out, fmtStamp(r.RetryAt, loc)) {
		t.Errorf("parked retry_at not rendered in Europe/Oslo:\n%s", out)
	}
}
