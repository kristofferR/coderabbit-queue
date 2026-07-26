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

// reviewingRounds returns the rounds that fired and are still open (fired or
// reviewing), ordered by fire time — the v3 equivalent of the "awaiting
// feedback" set (a fired round whose slot may already be released).
func reviewingRounds(st State) []Round {
	var out []Round
	for _, r := range st.Rounds {
		if r.Phase == PhaseFired || r.Phase == PhaseReviewing {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return firedAtOf(out[i]).Before(firedAtOf(out[j]))
	})
	return out
}

// headlineKeys returns the rounds the dashboard's summary table already names
// in its "In flight" / "Feedback wait" rows — the only active rounds rendered
// outside the Queue and Parked sections.
func headlineKeys(st State) map[string]bool {
	shown := map[string]bool{}
	if slot := st.SlotRound(); slot != nil {
		shown[Key(slot.Repo, slot.PR)] = true
	}
	if rev := reviewingRounds(st); len(rev) > 0 {
		shown[Key(rev[0].Repo, rev[0].PR)] = true
	}
	return shown
}

// parkedRounds returns the active rounds that no other part of the dashboard
// accounts for: not fire-eligible (so absent from the Queue section) and not
// named in the "In flight"/"Feedback wait" rows. In practice that is an
// awaiting_retry round whose RetryAt is still in the future, plus the tail of a
// multi-round reviewing set and any reserved round whose fire slot was cleared.
//
// Without this, such rounds vanished entirely and a parked-only state rendered
// byte-identically to an empty queue — "no work" and "two PRs parked until
// 00:07Z" must not look the same. Ordered by Seq like the queue.
func parkedRounds(st State, now time.Time) []Round {
	shown := headlineKeys(st)
	var out []Round
	for _, r := range st.Rounds {
		if !r.Active() || r.FireEligible(now) || shown[Key(r.Repo, r.PR)] {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// nextRetry returns the earliest RetryAt among rounds, or nil when none of
// them carries one (fmtStamp renders that as "—").
func nextRetry(rounds []Round) *time.Time {
	var best *time.Time
	for _, r := range rounds {
		if r.RetryAt == nil {
			continue
		}
		if best == nil || r.RetryAt.Before(*best) {
			best = r.RetryAt
		}
	}
	return best
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
// feedback-wait row: ✓ = trigger posted/adopted, ⏳ = post claimed but not
// yet recorded. Empty (byte-identical row) when the round tracks no co-bots.
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
	return " · triggers: " + strings.Join(parts, ", ")
}

// RenderDashboard renders the human-facing dashboard for v3 state: rounds by
// phase instead of v2's queue/fired/awaiting maps.
func RenderDashboard(st State, cfg StoreConfig) string {
	loc := dashboardLoc(cfg)
	now := time.Now().UTC()
	queue := st.QueuedRounds(now)
	parked := parkedRounds(st, now)
	reviewing := reviewingRounds(st)
	slot := st.SlotRound()
	blocked := st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now)

	var b strings.Builder
	fmt.Fprintf(&b, "# 🐰 crq — CodeRabbit review queue\n\n")

	switch {
	case blocked:
		fmt.Fprintf(&b, "### 🔴 Blocked — next review in ~%dm\n\n", minutesUntil(*st.Account.BlockedUntil, now))
	case slot != nil:
		fmt.Fprintf(&b, "### 🟡 Reviewing %s#%d\n\n", slot.Repo, slot.PR)
	case len(reviewing) > 0:
		fmt.Fprintf(&b, "### 🟡 Awaiting feedback for %s#%d\n\n", reviewing[0].Repo, reviewing[0].PR)
	case len(queue) > 0:
		fmt.Fprintf(&b, "### 🟠 %d queued\n\n", len(queue))
	case len(parked) > 0:
		fmt.Fprintf(&b, "### 🅿️ %d parked — next retry %s\n\n", len(parked), fmtStamp(nextRetry(parked), loc))
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
	if slot != nil {
		fmt.Fprintf(&b, "| **In flight** | [%s#%d](https://github.com/%s/pull/%d) · fired %s · `%s` |\n",
			slot.Repo, slot.PR, slot.Repo, slot.PR, fmtStamp(slot.FiredAt, loc), slot.ByHost)
	} else {
		fmt.Fprintf(&b, "| **In flight** | — |\n")
	}
	if len(reviewing) > 0 {
		r := reviewing[0]
		fmt.Fprintf(&b, "| **Feedback wait** | [%s#%d](https://github.com/%s/pull/%d) · `%s` · deadline %s%s |\n",
			r.Repo, r.PR, r.Repo, r.PR, r.Head, fmtStamp(r.WaitDeadline, loc), coBotMarks(r))
	} else {
		fmt.Fprintf(&b, "| **Feedback wait** | — |\n")
	}
	if st.Warn != "" {
		fmt.Fprintf(&b, "\n> ⚠️ %s\n", st.Warn)
	}

	fmt.Fprintf(&b, "\n## ⏳ Queue — %d waiting", len(queue))
	if len(parked) > 0 {
		fmt.Fprintf(&b, " · %d parked", len(parked))
	}
	fmt.Fprintf(&b, "\n\n")
	switch {
	case len(queue) > 0:
		fmt.Fprintf(&b, "| # | PR | enqueued | host |\n|--:|---|---|---|\n")
		for i, r := range queue {
			fmt.Fprintf(&b, "| %d | [%s#%d](https://github.com/%s/pull/%d) | %s | `%s` |\n",
				i+1, r.Repo, r.PR, r.Repo, r.PR, fmtStamp(&r.EnqueuedAt, loc), r.ByHost)
		}
	case len(parked) > 0:
		// Never the "nothing queued" empty state: work exists, it is just not
		// fire-eligible yet. The parked table below is the honest answer.
		fmt.Fprintf(&b, "_Nothing fire-eligible right now._\n")
	default:
		fmt.Fprintf(&b, "_Nothing queued._\n")
	}

	if len(parked) > 0 {
		fmt.Fprintf(&b, "\n### 🅿️ Parked — %d not yet eligible\n\n", len(parked))
		fmt.Fprintf(&b, "| PR | commit | phase | attempts | enqueued | retry at | host |\n|---|---|---|--:|---|---|---|\n")
		for _, r := range parked {
			fmt.Fprintf(&b, "| [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | %d | %s | %s | `%s` |\n",
				r.Repo, r.PR, r.Repo, r.PR, r.Head, r.Phase, r.Attempts,
				fmtStamp(&r.EnqueuedAt, loc), fmtStamp(r.RetryAt, loc), r.ByHost)
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

func RenderTitle(st State) string {
	now := time.Now().UTC()
	queue := len(st.QueuedRounds(now))
	parked := len(parkedRounds(st, now))
	// Parked rounds are real work; a state holding only those must never read
	// as "idle" or "queue 0".
	count := fmt.Sprintf("queue %d", queue)
	if parked > 0 {
		count += fmt.Sprintf(" · %d parked", parked)
	}
	switch {
	case st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now):
		return fmt.Sprintf("🐰 crq — blocked · %s", count)
	case st.SlotRound() != nil:
		return fmt.Sprintf("🐰 crq — reviewing #%d · %s", st.SlotRound().PR, count)
	case len(reviewingRounds(st)) > 0:
		return fmt.Sprintf("🐰 crq — awaiting feedback · %s", count)
	case queue > 0 && parked > 0:
		return fmt.Sprintf("🐰 crq — %d queued · %d parked", queue, parked)
	case queue > 0:
		return fmt.Sprintf("🐰 crq — %d queued", queue)
	case parked > 0:
		return fmt.Sprintf("🐰 crq — %d parked", parked)
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
