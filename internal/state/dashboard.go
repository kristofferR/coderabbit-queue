package state

import (
	"crypto/sha1"
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

func firedAtOf(r Round) time.Time {
	if r.FiredAt != nil {
		return *r.FiredAt
	}
	return r.EnqueuedAt
}

// requestedRounds gathers every round that has fired (active or archived) for
// the "Recently requested" table, newest first, capped.
func requestedRounds(st State) []Round {
	var out []Round
	for _, r := range st.Rounds {
		if r.FiredAt != nil {
			out = append(out, r)
		}
	}
	for _, r := range st.Archive {
		if r.FiredAt != nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FiredAt.After(*out[j].FiredAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// RenderDashboard renders the human-facing dashboard for v3 state: rounds by
// phase instead of v2's queue/fired/awaiting maps.
func RenderDashboard(st State, cfg StoreConfig) string {
	loc := dashboardLoc(cfg)
	now := time.Now().UTC()
	queue := st.QueuedRounds(now)
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
	default:
		fmt.Fprintf(&b, "### 🟢 Idle\n\n")
	}

	via := ""
	if st.Account.Source != "" && st.Account.Source != "init" {
		via = fmt.Sprintf("  _(via %s)_", st.Account.Source)
	}
	remaining := "available now"
	if blocked {
		remaining = "0 — rate-limited"
	}

	fmt.Fprintf(&b, "|   |   |\n|---|---|\n")
	fmt.Fprintf(&b, "| **Scope** | `%s` |\n", st.Account.Scope)
	fmt.Fprintf(&b, "| **Reviews remaining** | %s%s |\n", remaining, via)
	if blocked {
		fmt.Fprintf(&b, "| **Rate limit** | ⚠️ rate limited |\n")
	} else {
		fmt.Fprintf(&b, "| **Rate limit** | ✅ not currently limited |\n")
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
		fmt.Fprintf(&b, "| **Feedback wait** | [%s#%d](https://github.com/%s/pull/%d) · `%s` · deadline %s |\n",
			r.Repo, r.PR, r.Repo, r.PR, r.Head, fmtStamp(r.WaitDeadline, loc))
	} else {
		fmt.Fprintf(&b, "| **Feedback wait** | — |\n")
	}
	if st.Warn != "" {
		fmt.Fprintf(&b, "\n> ⚠️ %s\n", st.Warn)
	}

	fmt.Fprintf(&b, "\n## ⏳ Queue — %d waiting\n\n", len(queue))
	if len(queue) == 0 {
		fmt.Fprintf(&b, "_Nothing queued._\n")
	} else {
		fmt.Fprintf(&b, "| # | PR | enqueued | host |\n|--:|---|---|---|\n")
		for i, r := range queue {
			fmt.Fprintf(&b, "| %d | [%s#%d](https://github.com/%s/pull/%d) | %s | `%s` |\n",
				i+1, r.Repo, r.PR, r.Repo, r.PR, fmtStamp(&r.EnqueuedAt, loc), r.ByHost)
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
	switch {
	case st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now):
		return fmt.Sprintf("🐰 crq — blocked · queue %d", queue)
	case st.SlotRound() != nil:
		return fmt.Sprintf("🐰 crq — reviewing #%d · queue %d", st.SlotRound().PR, queue)
	case len(reviewingRounds(st)) > 0:
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
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}
