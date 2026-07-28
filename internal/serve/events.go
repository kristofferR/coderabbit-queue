package serve

import (
	"fmt"
	"sync"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Event is one thing that happened, derived by comparing consecutive state
// revisions.
//
// This is deliberately NOT an audit log, and the UI says so. Only the process
// running `crq watch` emits real events; everything else — the daemon, the CLI,
// the other host — changes state silently. Diffing revisions is the only way to
// see all of it, and it costs two honest limitations: the feed starts when this
// server starts, and anything that happens and reverts between two polls is
// never seen.
type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Level  string    `json:"level"` // ok|warn|bad|info
	Repo   string    `json:"repo,omitempty"`
	PR     int       `json:"pr,omitempty"`
	Head   string    `json:"head,omitempty"`
	Text   string    `json:"text"`
	Detail string    `json:"detail,omitempty"`
}

// eventLog is a bounded ring. Old events are dropped rather than paged: this is
// a "what just happened" panel, and pretending to hold history would invite
// someone to rely on it.
type eventLog struct {
	mu     sync.Mutex
	events []Event
	max    int
}

func newEventLog(max int) *eventLog { return &eventLog{max: max} }

func (l *eventLog) add(events ...Event) {
	if len(events) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, events...)
	if len(l.events) > l.max {
		l.events = l.events[len(l.events)-l.max:]
	}
}

// list returns newest first.
func (l *eventLog) list() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, 0, len(l.events))
	for i := len(l.events) - 1; i >= 0; i-- {
		out = append(out, l.events[i])
	}
	return out
}

// diffStates reports what changed between two revisions. `prev` being empty
// (the first poll) yields nothing: a fresh server has no basis to call anything
// "new", and announcing the entire existing queue as events would be a lie.
func diffStates(prev, next state.State, now time.Time) []Event {
	if prev.Rev == 0 || prev.Rev == next.Rev {
		return nil
	}
	var out []Event
	ev := func(kind, level, text string, r state.Round, detail string) {
		out = append(out, Event{At: now, Kind: kind, Level: level, Repo: r.Repo, PR: r.PR,
			Head: r.Head, Text: text, Detail: detail})
	}

	for key, cur := range next.Rounds {
		old, existed := prev.Rounds[key]
		switch {
		case !existed:
			what := "Enqueued"
			if cur.CoOnly {
				what = "Enqueued for co-review only"
			}
			ev("enqueued", "info", what, cur, cur.Note)
		case old.Head != cur.Head:
			ev("head", "info", "New head pushed", cur,
				fmt.Sprintf("was %s — the previous round was superseded", old.Head))
		case old.Phase != cur.Phase:
			level, text := "info", string(cur.Phase)
			switch cur.Phase {
			case state.PhaseFired:
				level, text = "ok", "Review requested"
			case state.PhaseReviewing:
				level, text = "ok", "Acknowledged — the fire slot was released"
			case state.PhaseCompleted:
				level, text = "ok", "Round completed"
			case state.PhaseAwaitingRetry:
				level, text = "warn", "Parked for retry"
			case state.PhaseReserved:
				text = "Took the fire slot"
			}
			ev("phase", level, text, cur, cur.Note)
		}
	}

	// A round that vanished from Rounds ended; the archive says how.
	archived := map[string]state.Round{}
	for _, r := range next.Archive {
		archived[state.Key(r.Repo, r.PR)] = r
	}
	for key, old := range prev.Rounds {
		if _, still := next.Rounds[key]; still {
			continue
		}
		if r, ok := archived[key]; ok {
			ev("ended", "info", "Round ended — "+string(r.Phase), r, r.Note)
			continue
		}
		ev("ended", "info", "Round cleared", old, old.Note)
	}

	for key, h := range next.Holds {
		if _, had := prev.Holds[key]; had {
			continue
		}
		repo, pr := splitKey(key)
		out = append(out, Event{At: now, Kind: "hold", Level: "bad", Repo: repo, PR: pr,
			Text: "Held by " + h.By, Detail: h.Reason})
	}
	for key := range prev.Holds {
		if _, still := next.Holds[key]; still {
			continue
		}
		repo, pr := splitKey(key)
		out = append(out, Event{At: now, Kind: "unhold", Level: "ok", Repo: repo, PR: pr,
			Text: "Hold lifted — it rejoins the queue"})
	}

	for key, d := range next.Dispatches {
		if _, had := prev.Dispatches[key]; had {
			continue
		}
		repo, pr := splitKey(key)
		out = append(out, Event{At: now, Kind: "fixing", Level: "ok", Repo: repo, PR: pr,
			Text:   "Fix session started on " + hostOf(d.Host),
			Detail: fmt.Sprintf("attempt %d", d.Attempts)})
	}
	for key := range prev.Dispatches {
		if _, still := next.Dispatches[key]; still {
			continue
		}
		repo, pr := splitKey(key)
		out = append(out, Event{At: now, Kind: "fixed", Level: "info", Repo: repo, PR: pr,
			Text: "Fix session finished"})
	}

	// Quota is the fleet's most consequential state, so both edges are events.
	prevBlocked := prev.Account.BlockedUntil != nil && prev.Account.BlockedUntil.After(now)
	nextBlocked := next.Account.BlockedUntil != nil && next.Account.BlockedUntil.After(now)
	switch {
	case !prevBlocked && nextBlocked:
		out = append(out, Event{At: now, Kind: "quota", Level: "warn",
			Text:   "CodeRabbit quota blocked",
			Detail: "reopens " + next.Account.BlockedUntil.Format("15:04") + " — co-review and autofix continue"})
	case prevBlocked && !nextBlocked:
		out = append(out, Event{At: now, Kind: "quota", Level: "ok",
			Text: "Quota window reopened — metered reviews can fire again"})
	}

	for host, cur := range next.AutofixByHost {
		old := prev.AutofixByHost[host]
		if cur.ConsecutiveFailures > old.ConsecutiveFailures {
			out = append(out, Event{At: now, Kind: "autofix", Level: "bad",
				Text:   fmt.Sprintf("Fix session failed on %s (%d in a row)", host, cur.ConsecutiveFailures),
				Detail: cur.LastError})
		}
	}

	for repo, cur := range next.Repos {
		old, had := prev.Repos[repo]
		if had && sameTime(old.UpdatedAt, cur.UpdatedAt) {
			continue
		}
		out = append(out, Event{At: now, Kind: "settings", Level: "info", Repo: repo,
			Text: "Reviewer override changed", Detail: "by " + cur.By})
	}
	for repo, cur := range next.RepoAutofix {
		old, had := prev.RepoAutofix[repo]
		if had && sameTime(old.UpdatedAt, cur.UpdatedAt) {
			continue
		}
		text := "Autofix turned off"
		if cur.Enabled {
			text = "Autofix turned on"
		}
		out = append(out, Event{At: now, Kind: "settings", Level: "info", Repo: repo,
			Text: text, Detail: cur.Reason})
	}

	// Clearing an override is as much a change as setting one — it hands the
	// repo back to the fleet default, which is rarely what was there before.
	for repo := range prev.Repos {
		if _, still := next.Repos[repo]; !still {
			out = append(out, Event{At: now, Kind: "settings", Level: "info", Repo: repo,
				Text: "Reviewer override cleared — back to the fleet default"})
		}
	}
	for repo := range prev.RepoAutofix {
		if _, still := next.RepoAutofix[repo]; !still {
			out = append(out, Event{At: now, Kind: "settings", Level: "info", Repo: repo,
				Text: "Autofix override cleared — back to the fleet default"})
		}
	}

	return out
}

func sameTime(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return a.Equal(*b)
}
