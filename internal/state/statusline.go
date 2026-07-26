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

	var parts []string
	switch {
	case firstStranded(st, inFlight) != nil:
		s := firstStranded(st, inFlight)
		parts = append(parts, fmt.Sprintf("⚠ #%d stranded", s.PR))
	case st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now):
		parts = append(parts, fmt.Sprintf("⏳ blocked %dm", minutesUntil(*st.Account.BlockedUntil, now)))
	case len(inFlight) > 0:
		parts = append(parts, fmt.Sprintf("🔬 #%d reviewing", inFlight[0].PR))
	case len(queue) == 0:
		return "✅ crq idle"
	default:
		parts = append(parts, "🟢 ready")
	}

	if n := len(queue); n > 0 {
		next := ""
		// Name the next PR only when the queue knows which it is; see Queue.
		if queue[0].ReadyAt.IsZero() && queue[0].Why == "" {
			next = fmt.Sprintf(" next #%d", queue[0].PR)
		}
		parts = append(parts, fmt.Sprintf("%d queued%s", n, next))
	}
	if len(inFlight) > 1 {
		parts = append(parts, fmt.Sprintf("%d in flight", len(inFlight)))
	}
	return "crq " + strings.Join(parts, " · ")
}
