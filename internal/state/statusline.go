package state

import (
	"fmt"
	"strings"
	"time"
)

// StatusLine renders the queue as one line, for a harness status bar.
//
// It exists so nobody has to ask a chat agent "is it still going?" — the most
// common question in a review session, and the one that cost the most tokens to
// answer, because answering it meant a tool call and a paragraph. A status bar
// answers it continuously for free.
//
// Everything here is already computed for the dashboard; this is a second
// rendering of the same reduced state, not new logic.
func StatusLine(st State, cfg StoreConfig) string {
	now := time.Now().UTC()
	queue := st.Queue(now, cfg.MinInterval)
	inFlight := inFlightRounds(st)
	held := heldRounds(st)

	// Something is ready to go right now: the queue put it at the front with no
	// reason to wait. That outranks the account block, because a quota-free round
	// is exactly what Queue leaves actionable while the window is shut — saying
	// "blocked" and then "next #7" in the same line contradicts itself.
	ready := len(queue) > 0 && queue[0].ReadyAt.IsZero() && queue[0].Why == ""

	var parts []string
	heldPrimary := false
	stranded := firstStranded(st, inFlight)
	switch {
	case st.Autofix.Unhealthy():
		// Above everything else: a queue that looks busy while no session can
		// start is the state that hid a wedged dispatcher for hours.
		parts = append(parts, fmt.Sprintf("🚨 dispatch failing (%d)", st.Autofix.ConsecutiveFailures))
	case stranded != nil:
		parts = append(parts, fmt.Sprintf("⚠ #%d stranded", stranded.PR))
	case !ready && st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now):
		parts = append(parts, fmt.Sprintf("⏳ blocked %dm", minutesUntil(*st.Account.BlockedUntil, now)))
	case len(inFlight) > 0:
		parts = append(parts, fmt.Sprintf("🔬 #%d reviewing", inFlight[0].PR))
	case len(queue) == 0 && len(held) > 0:
		parts = append(parts, fmt.Sprintf("⏸ %d held", len(held)))
		heldPrimary = true
	case len(queue) == 0:
		return "✅ crq idle"
	default:
		parts = append(parts, "🟢 ready")
	}

	if n := len(queue); n > 0 {
		next := ""
		// Name the next PR only when the queue knows which it is (see Queue) and
		// nothing is stranded: a stranded reservation is the whole line, and
		// pointing at what comes after it reads as though the queue is moving.
		if ready && stranded == nil {
			next = fmt.Sprintf(" next #%d", queue[0].PR)
		}
		parts = append(parts, fmt.Sprintf("%d queued%s", n, next))
	}
	if len(inFlight) > 1 {
		parts = append(parts, fmt.Sprintf("%d in flight", len(inFlight)))
	}
	if len(held) > 0 && !heldPrimary {
		parts = append(parts, fmt.Sprintf("%d held", len(held)))
	}
	return "crq " + strings.Join(parts, " · ")
}
