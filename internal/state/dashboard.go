package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	stateBegin    = "<!-- crq:state"
	stateEnd      = "-->"
	crqProjectURL = "https://github.com/kristofferR/coderabbit-queue"
)

// firstStranded finds an in-flight round that reserved the slot but no longer
// holds it: it cannot receive feedback (no command was posted) and Pump cannot
// advance it (no slot), so it needs naming wherever it sits in the list.
func firstStranded(inFlight []Round) *Round {
	for i := range inFlight {
		if inFlight[i].Phase == PhaseReserved {
			return &inFlight[i]
		}
	}
	return nil
}

func joinScope(scope []string) string {
	return strings.Join(scope, ",")
}

func dashboardLoc(cfg StoreConfig) *time.Location {
	if cfg.Timezone != "" {
		if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

func fmtStamp(t *time.Time, loc *time.Location) string {
	if t == nil {
		return "—"
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func minutesUntil(t time.Time, now time.Time) int {
	mins := int(t.Sub(now).Minutes()) + 1
	if mins < 1 {
		mins = 1
	}
	return mins
}

// inFlightRounds returns every round crq has already acted on and is still
// carrying: reserved (slot held, command not yet posted), fired, or reviewing —
// ordered by fire time.
//
// Together with State.Queue (queued + awaiting_retry) this PARTITIONS Active()
// by phase, which is what makes "every active round is on the dashboard" true
// by construction. The previous single "Feedback wait" row showed reviewing[0]
// and silently dropped every round behind it, along with any reserved round
// whose fire slot Normalize had cleared.
func inFlightRounds(st State) []Round {
	var out []Round
	for _, r := range st.Rounds {
		switch r.Phase {
		case PhaseReserved, PhaseFired, PhaseReviewing:
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return firedAtOf(out[i]).Before(firedAtOf(out[j]))
	})
	return out
}

func firedAtOf(r Round) time.Time {
	if r.FiredAt != nil {
		return *r.FiredAt
	}
	return r.EnqueuedAt
}

// requestedRounds gathers every round for which crq actually REQUESTED a review
// (active or archived) for the "Recently requested" table, newest first, capped.
//
// CoOnly rounds are excluded: they carry a FiredAt because it anchors their
// evidence floor, but crq never asked the primary reviewer for anything. Listing
// them crowded the table with repos that cannot use the queue at all — on a
// CodeRabbit-Free private repo every push produced a row for a review that was
// never requested, pushing the real history off the end of the cap.
func requestedRounds(st State) []Round {
	var out []Round
	for _, r := range st.Rounds {
		if r.FiredAt != nil && !r.CoOnly {
			out = append(out, r)
		}
	}
	for _, r := range st.Archive {
		if r.FiredAt != nil && !r.CoOnly {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FiredAt.After(*out[j].FiredAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// coBotMarks renders a round's co-reviewer trigger bookkeeping for the
// in-flight table's triggers column: ✓ = trigger posted/adopted, ⏳ = post
// claimed but not yet recorded. Empty when the round tracks no co-bots; the
// caller supplies the surrounding decoration.
func coBotMarks(r Round) string {
	if len(r.CoBots) == 0 {
		return ""
	}
	names := make([]string, 0, len(r.CoBots))
	for name := range r.CoBots {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		c := r.CoBots[name]
		switch {
		case c.CommandID != 0:
			parts = append(parts, name+" ✓")
		case c.ClaimedAt != nil:
			parts = append(parts, name+" ⏳")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// dash renders an empty cell as an em dash so a table row never collapses.
func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// RenderDashboard renders the human-facing dashboard for v3 state: rounds by
// phase instead of v2's queue/fired/awaiting maps.
func RenderDashboard(st State, cfg StoreConfig) string {
	loc := dashboardLoc(cfg)
	now := time.Now().UTC()
	queue := st.Queue(now, cfg.MinInterval)
	inFlight := inFlightRounds(st)
	slot := st.SlotRound()
	blocked := st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now)

	var b strings.Builder
	fmt.Fprintf(&b, "# 🐰 crq — CodeRabbit review queue\n\n")

	switch {
	case blocked:
		fmt.Fprintf(&b, "### 🔴 Blocked — next review in ~%dm\n\n", minutesUntil(*st.Account.BlockedUntil, now))
	case slot != nil:
		fmt.Fprintf(&b, "### 🟡 Reviewing %s#%d\n\n", slot.Repo, slot.PR)
	case len(inFlight) > 0:
		// A reserved round has not posted its command yet, so no feedback can be
		// on its way — and with no FireSlot behind it, Pump cannot move it either.
		// Calling that a feedback wait sends the reader looking for a review that
		// was never requested.
		//
		// Look past the first row: in flight is ordered by fire time, so an older
		// reviewing round hides a later stranded reservation — and a reviewing
		// round alongside a stranded one is the normal shape, not the exception.
		if stranded := firstStranded(inFlight); stranded != nil {
			fmt.Fprintf(&b, "### 🟠 Stranded reservation on %s#%d — no fire slot backs it\n\n", stranded.Repo, stranded.PR)
		} else {
			fmt.Fprintf(&b, "### 🟡 Awaiting feedback for %s#%d\n\n", inFlight[0].Repo, inFlight[0].PR)
		}
	case len(queue) > 0:
		// Nothing ready yet is still queued work, never idle — say when the front
		// of the queue opens instead of leaving the reader to guess.
		if next := queue[0].ReadyAt; !next.IsZero() {
			fmt.Fprintf(&b, "### 🟠 %d queued — next at %s\n\n", len(queue), fmtStamp(&next, loc))
		} else {
			fmt.Fprintf(&b, "### 🟠 %d queued\n\n", len(queue))
		}
	default:
		fmt.Fprintf(&b, "### 🟢 Idle\n\n")
	}

	via := ""
	if st.Account.Source != "" && st.Account.Source != "init" {
		via = fmt.Sprintf("  _(via %s)_", st.Account.Source)
	}
	remaining := "available now"
	if st.Account.Remaining != nil {
		remaining = fmt.Sprintf("%d", *st.Account.Remaining)
	}
	if blocked {
		remaining = "0 — account blocked"
	}

	fmt.Fprintf(&b, "|   |   |\n|---|---|\n")
	fmt.Fprintf(&b, "| **Scope** | `%s` |\n", st.Account.Scope)
	fmt.Fprintf(&b, "| **Reviews remaining** | %s%s |\n", remaining, via)
	if blocked {
		fmt.Fprintf(&b, "| **CodeRabbit quota** | ⚠️ account blocked |\n")
	} else {
		fmt.Fprintf(&b, "| **CodeRabbit quota** | ✅ not currently blocked |\n")
	}
	if cfg.CoReviewers != "" {
		fmt.Fprintf(&b, "| **Co-reviewers** | %s |\n", cfg.CoReviewers)
	}
	fmt.Fprintf(&b, "| **Last review fired** | %s |\n", fmtStamp(st.LastFired, loc))
	if st.Warn != "" {
		fmt.Fprintf(&b, "\n> ⚠️ %s\n", st.Warn)
	}

	fmt.Fprintf(&b, "\n## 🔬 In flight — %d\n\n", len(inFlight))
	if len(inFlight) == 0 {
		fmt.Fprintf(&b, "_None._\n")
	} else {
		fmt.Fprintf(&b, "| PR | commit | phase | fired | deadline | triggers | host |\n|---|---|---|---|---|---|---|\n")
		for _, r := range inFlight {
			fmt.Fprintf(&b, "| [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | %s | %s | %s | `%s` |\n",
				r.Repo, r.PR, r.Repo, r.PR, r.Head, r.Phase,
				fmtStamp(r.FiredAt, loc), fmtStamp(r.WaitDeadline, loc), dash(coBotMarks(r)), r.ByHost)
		}
	}

	fmt.Fprintf(&b, "\n## ⏳ Queue — %d waiting\n\n", len(queue))
	if len(queue) == 0 {
		fmt.Fprintf(&b, "_Nothing queued._\n")
	} else {
		fmt.Fprintf(&b, "| # | PR | commit | ready | why | attempts | enqueued | host |\n|--:|---|---|---|---|--:|---|---|\n")
		for i, e := range queue {
			// Absolute stamps only: a relative "in 11m" would re-hash the dashboard
			// on every render, and fmtStamp already honours CRQ_TZ.
			ready := "now"
			if !e.ReadyAt.IsZero() {
				at := e.ReadyAt
				ready = fmtStamp(&at, loc)
			}
			fmt.Fprintf(&b, "| %d | [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | %s | %d | %s | `%s` |\n",
				i+1, e.Repo, e.PR, e.Repo, e.PR, e.Head, ready, dash(e.Why),
				e.Attempts, fmtStamp(&e.EnqueuedAt, loc), e.ByHost)
		}
	}

	requested := requestedRounds(st)
	fmt.Fprintf(&b, "\n## 📨 Recently requested — last %d\n\n", len(requested))
	if len(requested) == 0 {
		fmt.Fprintf(&b, "_None yet._\n")
	} else {
		fmt.Fprintf(&b, "| PR | commit | requested | host |\n|---|---|---|---|\n")
		for _, r := range requested {
			fmt.Fprintf(&b, "| [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | `%s` |\n",
				r.Repo, r.PR, r.Repo, r.PR, r.Head, fmtStamp(r.FiredAt, loc), r.ByHost)
		}
	}

	fmt.Fprintf(&b, "\n---\n")
	fmt.Fprintf(&b, "<sub>🤖 Managed by [crq](%s) · rev %d · updated %s · do not edit by hand (machine state is in the hidden block at the top).</sub>\n",
		crqProjectURL, st.Rev, fmtStamp(st.UpdatedAt, loc))
	return b.String()
}

// RenderTitle summarizes the state for the dashboard issue title. The queue
// count is the WHOLE queue, cooling-down rounds included: a state whose only
// work is not yet fire-eligible is queued, never idle.
func RenderTitle(st State, cfg StoreConfig) string {
	now := time.Now().UTC()
	queue := len(st.Queue(now, cfg.MinInterval))
	switch {
	case st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now):
		return fmt.Sprintf("🐰 crq — blocked · queue %d", queue)
	case st.SlotRound() != nil:
		return fmt.Sprintf("🐰 crq — reviewing #%d · queue %d", st.SlotRound().PR, queue)
	case len(inFlightRounds(st)) > 0:
		// The body names a stranded reservation; the title must not contradict it
		// by reporting a feedback wait for a round that posted no command.
		if stranded := firstStranded(inFlightRounds(st)); stranded != nil {
			return fmt.Sprintf("🐰 crq — stranded #%d · queue %d", stranded.PR, queue)
		}
		return fmt.Sprintf("🐰 crq — awaiting feedback · queue %d", queue)
	case queue > 0:
		return fmt.Sprintf("🐰 crq — %d queued", queue)
	default:
		return "🐰 crq — idle"
	}
}

func IssueBody(st State, cfg StoreConfig) (string, error) {
	machine, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\n%s\n%s\n\n%s", stateBegin, machine, stateEnd, RenderDashboard(st, cfg)), nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
