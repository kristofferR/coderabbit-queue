package state

import (
	"sort"
	"time"
)

// HostReport is what one machine says about itself: which crq it runs, and
// which tools it can actually reach.
//
// It exists because every question about a fleet turns into a per-host
// question, and crq could only answer for whichever machine you happened to be
// asking from. "Is claude installed" has a different answer on each host, and
// the one that matters — is it on the PATH the SERVICE runs with — has a
// different answer again from the shell you are typing in.
//
// Reported rather than inferred. A host writes its own record; nothing else may
// write one for it, because a machine that has stopped reporting is exactly the
// machine whose last claim about itself should stop being trusted.
type HostReport struct {
	Host string `json:"host"`
	// Version is the crq that wrote this. Two hosts on different versions is
	// the single most common cause of "that setting did nothing".
	Version string `json:"version,omitempty"`
	// Caps is what that binary understands, so a reader can see WHY a host
	// ignores a setting rather than only that it does.
	Caps int `json:"caps,omitempty"`
	// Roles are the crq services running here ("autoreview", "autofix",
	// "serve"), so a fleet can be read as who does what.
	Roles []string `json:"roles,omitempty"`
	// Tools is what this host can run, probed on the PATH the reporting process
	// had. A daemon reports the service's PATH, which is the one that decides
	// whether a fix session can start.
	Tools []ToolReport `json:"tools,omitempty"`
	At    time.Time    `json:"at"`
}

// ToolReport is one executable on one host.
type ToolReport struct {
	Name string `json:"name"`
	// Path is empty when it was not found at all.
	Path string `json:"path,omitempty"`
	// Version is the tool's own, when it will say; some do not.
	Version string `json:"version,omitempty"`
}

// Found reports whether the tool is reachable at all.
func (t ToolReport) Found() bool { return t.Path != "" }

// HostReportTTL is how long a host's self-report is worth showing as current.
// Past it the record is kept and marked stale rather than dropped: "this host
// last said it had claude, two days ago" is more useful than silence.
const HostReportTTL = 30 * time.Minute

// SetHostReport records what a host says about itself.
func (s *State) SetHostReport(r HostReport, now time.Time) {
	if s.HostReports == nil {
		s.HostReports = map[string]HostReport{}
	}
	r.At = now.UTC()
	s.HostReports[r.Host] = r
}

// HostReportList is every host's self-report, most recently heard from first.
func (s *State) HostReportList() []HostReport {
	out := make([]HostReport, 0, len(s.HostReports))
	for _, r := range s.HostReports {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// ToolOn reports what a named host says about one tool, and whether that host
// has said anything at all.
func (s *State) ToolOn(host, tool string) (ToolReport, bool) {
	r, ok := s.HostReports[host]
	if !ok {
		return ToolReport{}, false
	}
	for _, t := range r.Tools {
		if t.Name == tool {
			return t, true
		}
	}
	return ToolReport{Name: tool}, true
}
