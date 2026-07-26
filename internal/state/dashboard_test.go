package state

import (
	"strings"
	"testing"
	"time"
)

const nothingQueued = "_Nothing queued._"

func ptime(t time.Time) *time.Time { return &t }

// coolingRound is the shape that used to vanish from the dashboard: a waiting
// round whose own RetryAt has not passed yet.
func coolingRound(repo string, pr int, seq int64, now time.Time, retryIn time.Duration) Round {
	return Round{
		Repo:       repo,
		PR:         pr,
		Head:       "a9c688a1c",
		Seq:        seq,
		Phase:      PhaseAwaitingRetry,
		Attempts:   1,
		EnqueuedAt: now.Add(-10 * time.Minute),
		FiredAt:    ptime(now.Add(-9 * time.Minute)),
		RetryAt:    ptime(now.Add(retryIn)),
		ByHost:     "cachyos",
	}
}

func queuedRound(repo string, pr int, seq int64, now time.Time) Round {
	return Round{
		Repo:       repo,
		PR:         pr,
		Head:       "beefbeef1",
		Seq:        seq,
		Phase:      PhaseQueued,
		EnqueuedAt: now.Add(-time.Minute),
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

// queueSection returns just the "## ⏳ Queue" section of a rendered dashboard,
// so a match cannot come from the in-flight table or the requested history.
func queueSection(t *testing.T, out string) string {
	t.Helper()
	_, after, ok := strings.Cut(out, "## ⏳ Queue")
	if !ok {
		t.Fatalf("no queue section:\n%s", out)
	}
	before, _, _ := strings.Cut(after, "\n## ")
	return before
}

// A queue whose rounds are all cooling down must not render as an empty queue:
// that is the bug this design exists to prevent (a reader concluded their
// enqueue had been dropped).
func TestRenderDashboardCoolingDownOnly(t *testing.T) {
	now := time.Now().UTC()
	a := coolingRound("kristofferr/ha-adjustable-bed", 480, 1, now, 11*time.Minute)
	b := coolingRound("kristofferr/ha-adjustable-bed", 481, 2, now, 12*time.Minute)
	st := stateWith(a, b)

	out := RenderDashboard(st, StoreConfig{})
	if strings.Contains(out, nothingQueued) {
		t.Fatalf("cooling-down rounds rendered as an empty queue:\n%s", out)
	}
	if !strings.Contains(out, "## ⏳ Queue — 2 waiting") {
		t.Errorf("queue heading does not count cooling-down rounds:\n%s", out)
	}
	q := queueSection(t, out)
	for _, want := range []string{
		"kristofferr/ha-adjustable-bed#480",
		"https://github.com/kristofferr/ha-adjustable-bed/pull/480",
		"`a9c688a1c`",
		fmtStamp(a.RetryAt, time.UTC), // absolute ready time, not a relative "in 11m"
		WaitCoolingDown,
		"`cachyos`",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("queue section missing %q:\n%s", want, q)
		}
	}
	// The header must say when the front of the queue opens.
	if !strings.Contains(out, "2 queued — next at "+fmtStamp(a.RetryAt, time.UTC)) {
		t.Errorf("header does not report the next ready time:\n%s", out)
	}

	if got, want := RenderTitle(st, StoreConfig{}), "🐰 crq — 2 queued"; got != want {
		t.Errorf("RenderTitle = %q, want %q", got, want)
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
	if !strings.Contains(out, "## 🔬 In flight — 0\n\n_None._") {
		t.Errorf("empty state lost its empty in-flight section:\n%s", out)
	}
	if got, want := RenderTitle(st, StoreConfig{}), "🐰 crq — idle"; got != want {
		t.Errorf("RenderTitle = %q, want %q", got, want)
	}
}

// The queue is ordered the way rounds will actually fire: ready rounds first
// (by Seq), then by when each window opens — regardless of Seq.
func TestQueueOrdersByReadyThenSeq(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		coolingRound("kristofferr/a", 1, 1, now, 20*time.Minute), // lowest Seq, latest window
		coolingRound("kristofferr/b", 2, 2, now, 5*time.Minute),
		queuedRound("kristofferr/c", 3, 3, now),                  // ready now, highest Seq
		coolingRound("kristofferr/d", 4, 4, now, -2*time.Minute), // window already open
	)

	q := st.Queue(now, 0)
	var got []int
	for _, e := range q {
		got = append(got, e.PR)
	}
	// Ready now: c (Seq 3) and d (Seq 4, elapsed RetryAt) — Seq order among them.
	// Then b (+5m), then a (+20m).
	want := []int{3, 4, 2, 1}
	if len(got) != len(want) {
		t.Fatalf("Queue returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Queue order = %v, want %v", got, want)
		}
	}
	// An elapsed RetryAt is ready, not cooling down.
	if q[1].Why != "" || !q[1].ReadyAt.IsZero() {
		t.Errorf("elapsed retry window not treated as ready: %+v", q[1])
	}
	if q[2].Why != WaitCoolingDown {
		t.Errorf("q[2].Why = %q, want %q", q[2].Why, WaitCoolingDown)
	}
}

// The account-wide quota block gates firing too, so a round whose own RetryAt
// falls inside a longer block must display the block's end, not its own — the
// dashboard must not promise a time DecideFire will refuse.
func TestQueueAccountBlockDominatesRetryAt(t *testing.T) {
	now := time.Now().UTC()
	r := coolingRound("kristofferr/ha-adjustable-bed", 480, 1, now, 11*time.Minute)
	st := stateWith(r)
	blockedUntil := now.Add(44 * time.Minute)
	st.Account.BlockedUntil = &blockedUntil

	q := st.Queue(now, 0)
	if len(q) != 1 {
		t.Fatalf("Queue returned %d entries, want 1", len(q))
	}
	if !q[0].ReadyAt.Equal(blockedUntil.UTC()) {
		t.Errorf("ReadyAt = %s, want the account block end %s", q[0].ReadyAt, blockedUntil.UTC())
	}
	if q[0].Why != WaitAccountBlocked {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitAccountBlocked)
	}

	out := queueSection(t, RenderDashboard(st, StoreConfig{}))
	if !strings.Contains(out, fmtStamp(&blockedUntil, time.UTC)) {
		t.Errorf("queue does not show the account-block end:\n%s", out)
	}
	if strings.Contains(out, fmtStamp(r.RetryAt, time.UTC)) {
		t.Errorf("queue shows the round's own RetryAt, which the fire gate would refuse:\n%s", out)
	}
}

// A round waiting only because another PR holds the fire slot is ready now; the
// slot is what it waits on.
func TestQueueSlotBusy(t *testing.T) {
	now := time.Now().UTC()
	holder := Round{
		Repo: "kristofferr/a", PR: 1, Head: "aaaaaaaa1", Seq: 1,
		Phase: PhaseReserved, EnqueuedAt: now.Add(-2 * time.Minute),
		ReservedAt: ptime(now.Add(-time.Minute)), Token: "tok", ByHost: "cachyos",
	}
	st := stateWith(holder, queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: Key(holder.Repo, holder.PR), Token: "tok"}

	q := st.Queue(now, 0)
	if len(q) != 1 || q[0].PR != 2 {
		t.Fatalf("Queue = %+v, want only the queued round", q)
	}
	if q[0].Why != WaitSlotBusy {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitSlotBusy)
	}
}

// In flight (reserved/fired/reviewing) and Queue (queued/awaiting_retry)
// partition Active(), so every active round is on the dashboard exactly once.
// The old single "Feedback wait" row hid every reviewing round past the first
// and any reserved round whose fire slot had been cleared.
func TestRenderDashboardPartitionsActiveRounds(t *testing.T) {
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
	completed := Round{ // not active: the reviewed-head dedup marker
		Repo: "kristofferr/e", PR: 5, Head: "eeeeeeee5", Seq: 5,
		Phase: PhaseCompleted, EnqueuedAt: now.Add(-time.Hour), ByHost: "cachyos",
	}
	st := stateWith(fired, reviewing, reserved, completed,
		coolingRound("kristofferr/d", 4, 4, now, 30*time.Minute),
		queuedRound("kristofferr/f", 6, 6, now))

	out := RenderDashboard(st, StoreConfig{})
	// The "Recently requested" history lists fired rounds regardless of phase, so
	// it must not count as accounting for a live round.
	live, _, _ := strings.Cut(out, "## 📨 Recently requested")
	inFlight, queue, ok := strings.Cut(live, "## ⏳ Queue")
	if !ok {
		t.Fatalf("no queue section:\n%s", out)
	}
	for _, r := range st.Rounds {
		key := Key(r.Repo, r.PR)
		inA, inB := strings.Contains(inFlight, key), strings.Contains(queue, key)
		if !r.Active() {
			if inA || inB {
				t.Errorf("inactive round %s rendered as live work", key)
			}
			continue
		}
		if inA == inB {
			t.Errorf("active round %s is in %d sections, want exactly 1:\n%s", key, btoi(inA)+btoi(inB), live)
		}
	}
	if !strings.Contains(out, "## 🔬 In flight — 3") {
		t.Errorf("want 3 in-flight rounds:\n%s", out)
	}
	if !strings.Contains(out, "## ⏳ Queue — 2 waiting") {
		t.Errorf("want 2 waiting rounds:\n%s", out)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The in-flight table carries each round's co-reviewer trigger marks.
func TestRenderDashboardInFlightTriggers(t *testing.T) {
	now := time.Now().UTC()
	r := Round{
		Repo: "kristofferr/a", PR: 1, Head: "aaaaaaaa1", Seq: 1,
		Phase: PhaseReviewing, EnqueuedAt: now.Add(-3 * time.Minute),
		FiredAt: ptime(now.Add(-2 * time.Minute)), WaitDeadline: ptime(now.Add(18 * time.Minute)),
		ByHost: "cachyos",
	}
	r.SetCoCommand("chatgpt-codex-connector", 42, now.Add(-2*time.Minute))
	st := stateWith(r)

	out := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(out, "chatgpt-codex-connector ✓") {
		t.Errorf("in-flight row missing trigger marks:\n%s", out)
	}
	if !strings.Contains(out, fmtStamp(r.WaitDeadline, time.UTC)) {
		t.Errorf("in-flight row missing the wait deadline:\n%s", out)
	}
}

// The ready column honours CRQ_TZ via the shared fmtStamp helper.
func TestRenderDashboardQueueHonoursTimezone(t *testing.T) {
	now := time.Now().UTC()
	r := coolingRound("kristofferr/ha-adjustable-bed", 480, 1, now, 11*time.Minute)
	st := stateWith(r)

	loc, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	out := RenderDashboard(st, StoreConfig{Timezone: "Europe/Oslo"})
	if !strings.Contains(out, fmtStamp(r.RetryAt, loc)) {
		t.Errorf("ready time not rendered in Europe/Oslo:\n%s", out)
	}
}

// The dashboard's "ready" column is a promise about when firing will accept a
// round, so every gate DecideFire applies has to be reflected. These three cases
// each rendered "ready: now" for a round the fire gate would have refused.

// Pacing: after a fire, DecideFire rejects everything until LastFired +
// MinInterval. Excluding it as "sub-minute churn" was wrong twice over — it is an
// absolute boundary, so it does not churn between renders, and CRQ_MIN_INTERVAL
// can be configured far beyond 90s.
func TestQueueReflectsThePacingGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	fired := now.Add(-30 * time.Second)
	st.LastFired = &fired

	q := st.Queue(now, 90*time.Second)
	if len(q) != 1 {
		t.Fatalf("Queue = %d entries, want 1", len(q))
	}
	if want := fired.Add(90 * time.Second); !q[0].ReadyAt.Equal(want) {
		t.Errorf("ReadyAt = %v, want the pacing boundary %s", q[0].ReadyAt, want)
	}
	if q[0].Why != WaitPacing {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitPacing)
	}

	// Once the interval has elapsed it stops being a gate at all.
	if q := st.Queue(now.Add(2*time.Minute), 90*time.Second); len(q) != 1 || !q[0].ReadyAt.IsZero() {
		t.Errorf("an elapsed interval must not gate: %+v", q)
	}
}

// A slot held by another PR has no knowable release time. Leaving ReadyAt empty
// rendered as "now", which is the one thing that is definitely false.
func TestQueueDoesNotClaimSlotBlockedRoundsAreReadyNow(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now), queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: "kristofferr/b#2", Token: "tok", Since: now.Add(-time.Minute)}
	st.Rounds["kristofferr/b#2"] = func() Round {
		r := st.Rounds["kristofferr/b#2"]
		r.Phase = PhaseFired
		r.Token = "tok"
		return r
	}()

	for _, e := range st.Queue(now, 0) {
		if e.Why != WaitSlotBusy {
			continue
		}
		if !e.ReadyAt.IsZero() {
			t.Errorf("a slot-blocked entry must not carry a ready time, got %s", e.ReadyAt)
		}
	}
}

// A reserved round has not posted its command, so no feedback can be coming —
// and with no FireSlot behind it, Pump cannot advance it either. Reporting a
// feedback wait sent the reader looking for a review nobody requested.
func TestRenderNamesAStrandedReservation(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase = PhaseReserved
		reserved := now.Add(-time.Minute)
		r.ReservedAt = &reserved
		return r
	}()

	got := RenderDashboard(st, StoreConfig{})
	if strings.Contains(got, "Awaiting feedback") {
		t.Error("a reserved round with no fire slot is not a feedback wait")
	}
	if !strings.Contains(got, "Stranded reservation") {
		t.Errorf("the stranded reservation must be named, got:\n%s", got)
	}
}

// Gates compose: the ready time is the latest of them, and the reason is
// whichever one binds. Taking the first that matched let a short cooldown hide a
// long account block, advertising a time firing would refuse for hours.
func TestQueueTakesTheLatestBindingGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(coolingRound("kristofferr/a", 1, 1, now, time.Minute))
	blocked := now.Add(2 * time.Hour)
	st.Account.BlockedUntil = &blocked

	q := st.Queue(now, 0)
	if len(q) != 1 {
		t.Fatalf("Queue = %d entries, want 1", len(q))
	}
	if !q[0].ReadyAt.Equal(blocked.UTC()) {
		t.Errorf("ReadyAt = %v, want the dominating account block %s", q[0].ReadyAt, blocked.UTC())
	}
	if q[0].Why != WaitAccountBlocked {
		t.Errorf("Why = %q, want %q", q[0].Why, WaitAccountBlocked)
	}

	// And the other way round: a pacing boundary beyond a short cooldown binds.
	st2 := stateWith(coolingRound("kristofferr/b", 2, 2, now, time.Second))
	fired := now
	st2.LastFired = &fired
	q2 := st2.Queue(now, 10*time.Minute)
	if len(q2) != 1 || q2[0].Why != WaitPacing || !q2[0].ReadyAt.Equal(fired.Add(10*time.Minute).UTC()) {
		t.Errorf("pacing must bind past a shorter cooldown, got %+v", q2)
	}
}

// Pacing is serial, so a round behind the front cannot start until the front
// fires — but crq does not know when that is, and a time derived from render
// time would give an unchanged state a different DashboardSHA every minute (the
// churn that argued against surfacing pacing at all). So the queue says "behind
// an earlier round" rather than naming a time it cannot know.
func TestQueueNamesRoundsBehindTheFrontWithoutInventingATime(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		queuedRound("kristofferr/a", 1, 1, now),
		queuedRound("kristofferr/b", 2, 2, now),
		queuedRound("kristofferr/c", 3, 3, now),
	)

	q := st.Queue(now, 90*time.Second)
	if len(q) != 3 {
		t.Fatalf("Queue = %d entries, want 3", len(q))
	}
	if !q[0].ReadyAt.IsZero() || q[0].Why != "" {
		t.Errorf("the front is ready now with no gate, got ready=%v why=%q", q[0].ReadyAt, q[0].Why)
	}
	for i := 1; i < len(q); i++ {
		if !q[i].ReadyAt.IsZero() {
			t.Errorf("entry %d invented a ready time %s; only absolute gates may be shown", i, q[i].ReadyAt)
		}
		if q[i].Why != WaitBehind {
			t.Errorf("entry %d Why = %q, want %q", i, q[i].Why, WaitBehind)
		}
	}

	// Stability is the point: the same state rendered a minute later must produce
	// the same queue, or every render becomes a dashboard write.
	later := st.Queue(now.Add(time.Minute), 90*time.Second)
	for i := range q {
		if !later[i].ReadyAt.Equal(q[i].ReadyAt) || later[i].Why != q[i].Why {
			t.Errorf("entry %d changed with render time: %v/%q -> %v/%q",
				i, q[i].ReadyAt, q[i].Why, later[i].ReadyAt, later[i].Why)
		}
	}
}

// The order has to be what firing will actually do. NextEligible takes the lowest
// Seq among rounds eligible at that moment, so a round still cooling down when the
// front fires can overtake a ready round with a higher Seq. Sorting by ready time
// alone rendered 1, 3, 2 where the queue really runs 1, 2, 3.
func TestQueueOrderMatchesWhatFiringWillDo(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(
		queuedRound("kristofferr/a", 1, 1, now),                  // ready now
		coolingRound("kristofferr/b", 2, 2, now, 60*time.Second), // eligible before the next slot
		queuedRound("kristofferr/c", 3, 3, now),                  // ready now, higher Seq
	)

	var got []int
	for _, e := range st.Queue(now, 90*time.Second) {
		got = append(got, e.PR)
	}
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("Queue = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Queue = %v, want %v (b is eligible by the time the slot frees)", got, want)
		}
	}
}

// A slot held by another PR has no knowable release time, so it outranks every
// projected timestamp — otherwise a cooldown or pacing boundary is presented as a
// promise while the round cannot fire at all.
func TestQueueLetsSlotBusyDominateATimedGate(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(coolingRound("kristofferr/a", 1, 1, now, 5*time.Minute), queuedRound("kristofferr/b", 2, 2, now))
	st.FireSlot = &FireSlot{Key: "kristofferr/b#2", Token: "tok", Since: now.Add(-time.Minute)}
	st.Rounds["kristofferr/b#2"] = func() Round {
		r := st.Rounds["kristofferr/b#2"]
		r.Phase, r.Token = PhaseFired, "tok"
		return r
	}()

	for _, e := range st.Queue(now, 0) {
		if e.Why != WaitSlotBusy {
			t.Errorf("entry %d: Why = %q, want %q while the slot is held", e.PR, e.Why, WaitSlotBusy)
		}
		if !e.ReadyAt.IsZero() {
			t.Errorf("entry %d: ReadyAt = %s, want no promised time", e.PR, e.ReadyAt)
		}
	}
}

// In flight is ordered by fire time, so an older reviewing round would hide a
// later stranded reservation — and that pairing is the normal shape.
func TestRenderFindsAStrandedReservationBehindAReviewingRound(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now), queuedRound("kristofferr/b", 2, 2, now))
	fired := now.Add(-10 * time.Minute)
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase, r.FiredAt = PhaseReviewing, &fired
		return r
	}()
	reserved := now.Add(-time.Minute)
	st.Rounds["kristofferr/b#2"] = func() Round {
		r := st.Rounds["kristofferr/b#2"]
		r.Phase, r.ReservedAt = PhaseReserved, &reserved
		return r
	}()

	got := RenderDashboard(st, StoreConfig{})
	if !strings.Contains(got, "Stranded reservation on kristofferr/b#2") {
		t.Errorf("the stranded reservation must be named even behind a reviewing round, got:\n%s", got)
	}
}

// The title is what a human sees first, so it must not contradict the body: a
// reserved round whose slot was cleared cannot be awaiting feedback.
func TestRenderTitleNamesAStrandedReservation(t *testing.T) {
	now := time.Now().UTC()
	st := stateWith(queuedRound("kristofferr/a", 1, 1, now))
	reserved := now.Add(-time.Minute)
	st.Rounds["kristofferr/a#1"] = func() Round {
		r := st.Rounds["kristofferr/a#1"]
		r.Phase, r.ReservedAt = PhaseReserved, &reserved
		return r
	}()

	got := RenderTitle(st, StoreConfig{})
	if strings.Contains(got, "awaiting feedback") {
		t.Errorf("title claims a feedback wait for a stranded reservation: %q", got)
	}
	if !strings.Contains(got, "stranded") {
		t.Errorf("title = %q, want it to name the stranded reservation", got)
	}
}
