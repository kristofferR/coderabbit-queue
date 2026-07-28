package state

import (
	"sort"
	"time"
)

// FireLogWindow is how far back the fire log keeps timestamps. Two weeks, so a
// seven-day rolling count is always answered from data that is fully inside the
// log rather than from its truncated edge.
const FireLogWindow = 14 * 24 * time.Hour

// FireLogMax bounds the log whatever the window says. A fleet that somehow
// fires far more than any plan allows must not grow the state blob without
// limit; the newest entries are the ones a rolling count needs.
const FireLogMax = 1000

// NoteFire records that a metered review was requested at t.
//
// This is the only time series crq keeps, and it exists for one reason: the
// vendor's weekly fair-use throttle. crq already RECOGNISES that throttle when
// CodeRabbit announces it — the classifier and its corpus file have been there
// all along — but recognising it is recognising an ~80% throughput collapse
// after it has happened. Counting fires is what lets crq see the cliff coming,
// and a fleet orchestrator is exactly the thing that walks off it.
//
// Timestamps only. What was reviewed is already in the rounds; how many were
// reviewed is the question no other record can answer, because rounds are
// superseded and the archive is a 50-entry ring that a busy day evicts in hours.
func (s *State) NoteFire(t time.Time) {
	s.Account.Fires = append(s.Account.Fires, t.UTC())
	s.trimFireLog()
}

// trimFireLog drops what the window and the cap put out of scope. Sorting first
// makes it correct when two hosts' clocks disagree: entries arrive from whoever
// won the CAS, not in order.
//
// The window is measured from the NEWEST entry rather than from the one just
// appended: recording a fire that is itself old must not widen the window and
// resurrect everything before it.
func (s *State) trimFireLog() {
	if len(s.Account.Fires) == 0 {
		return
	}
	sort.Slice(s.Account.Fires, func(i, j int) bool {
		return s.Account.Fires[i].Before(s.Account.Fires[j])
	})
	newest := s.Account.Fires[len(s.Account.Fires)-1]
	cutoff := newest.Add(-FireLogWindow)
	keep := s.Account.Fires[:0]
	for _, at := range s.Account.Fires {
		if at.After(cutoff) {
			keep = append(keep, at)
		}
	}
	if len(keep) > FireLogMax {
		keep = keep[len(keep)-FireLogMax:]
	}
	s.Account.Fires = keep
}

// FiresSince counts recorded fires at or after t.
func (s *State) FiresSince(t time.Time) int {
	n := 0
	for _, at := range s.Account.Fires {
		if !at.Before(t.UTC()) {
			n++
		}
	}
	return n
}

// WeeklyFires is the rolling seven-day count — the number the fair-use
// threshold is expressed in.
func (s *State) WeeklyFires(now time.Time) int {
	return s.FiresSince(now.Add(-7 * 24 * time.Hour))
}

// FireLogSince is when the log starts, which is what makes its count honest: a
// count over three days of history is not a weekly count, and a caller that
// cannot tell the difference will read a fresh log as a quiet week.
func (s *State) FireLogSince() *time.Time {
	if len(s.Account.Fires) == 0 {
		return nil
	}
	first := s.Account.Fires[0]
	return &first
}

// WeeklyUsage is the fair-use picture: how many metered reviews this account
// has spent in the rolling week, against the threshold beyond which the vendor
// throttles it to one review an hour.
type WeeklyUsage struct {
	Fires int `json:"fires"`
	// Limit is the configured weekly threshold; 0 means none is configured and
	// the rest of this is a count without a verdict.
	Limit int `json:"limit,omitempty"`
	// Complete says the log covers a full seven days. Until it does, Fires is a
	// floor — crq only knows about fires since it started keeping the log.
	Complete bool `json:"complete"`
	// Since is when the log starts, so a partial count can say how partial.
	Since *time.Time `json:"since,omitempty"`
	// Level is ok | warn | over. warn begins at 80% of the limit, which is
	// roughly a day's headroom on a busy fleet — enough to act on.
	Level string `json:"level"`
	Note  string `json:"note"`
}

// FairUse reports the rolling-week usage against limit. A limit of 0 (not
// configured) still counts, because the count alone is worth seeing; it just
// never claims the account is close to anything.
func (s *State) FairUse(now time.Time, limit int) WeeklyUsage {
	out := WeeklyUsage{Fires: s.WeeklyFires(now), Limit: limit, Level: "ok"}
	out.Since = s.FireLogSince()
	weekAgo := now.UTC().Add(-7 * 24 * time.Hour)
	out.Complete = out.Since != nil && !out.Since.After(weekAgo)
	switch {
	case limit <= 0:
		out.Note = "no weekly threshold is configured, so this is a count without a verdict"
	case out.Fires >= limit:
		out.Level = "over"
		out.Note = "past the weekly fair-use threshold — the vendor throttles to about one review an hour from here"
	case out.Fires*5 >= limit*4:
		out.Level = "warn"
		out.Note = "close to the weekly fair-use threshold, beyond which reviews are throttled to about one an hour"
	default:
		out.Note = "inside the weekly fair-use allowance"
	}
	switch {
	case out.Since == nil:
		// An empty log is not a quiet week. Saying so matters most right after
		// an upgrade, when every fleet's log is empty and the count reads as an
		// account that has done nothing.
		out.Note = "no metered review has been recorded yet — this log starts with the first one"
	case !out.Complete:
		out.Note += "; the log starts " + out.Since.Format("2006-01-02") + ", so this is a floor"
	}
	return out
}
