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
	// RoleSeen dates each role separately, because the record as a whole cannot.
	// Every service on a host writes the SAME record and merges the roles it
	// found there, so the one still running refreshed the stopped one's claim on
	// every pass and the table showed a dead service running for ever.
	RoleSeen map[string]time.Time `json:"role_seen,omitempty"`
	// Tools is what this host can run, resolved across the roles reporting here
	// — see RoleTools for why that is not simply the last report.
	Tools []ToolReport `json:"tools,omitempty"`
	// RoleTools is what each role probed on ITS own PATH, kept apart because the
	// PATHs differ: the autofix unit adds the selected agent's directory and
	// serve does not. Merged into one list, the two took turns overwriting each
	// other and the dashboard alternated between "the fix agent is here" and
	// "it is missing" while the service that runs sessions never changed.
	RoleTools map[string][]ToolReport `json:"role_tools,omitempty"`
	At        time.Time               `json:"at"`

	// unknown carries members a newer binary wrote inside this record. State's
	// carrier cannot: it recognises "host_reports" and hands each report to an
	// ordinary decoder. See tolerant.go.
	unknown unknownFields
}

// toolRoleRank orders roles by whose PATH the answer should come from. The
// autofix service is the one that actually starts fix sessions, so its view of
// "is the agent reachable" is the operative one; a dashboard that merely
// renders the answer is the least authoritative.
func toolRoleRank(role string) int {
	switch role {
	case "autofix":
		return 0
	case "autoreview":
		return 1
	case "serve":
		return 3
	default:
		return 2
	}
}

// ToolReport is one executable on one host.
type ToolReport struct {
	Name string `json:"name"`
	// Path is empty when it was not found at all.
	Path string `json:"path,omitempty"`
	// Version is the tool's own, when it will say; some do not.
	Version string `json:"version,omitempty"`

	// unknown carries members a newer binary wrote inside this record, for the
	// same reason HostReport does — it is nested deeper still. See tolerant.go.
	unknown unknownFields
}

// Found reports whether the tool is reachable at all.
func (t ToolReport) Found() bool { return t.Path != "" }

// HostReportTTL is how long a host's self-report is worth showing as current.
// Past it the record is kept and marked stale rather than dropped: "this host
// last said it had claude, two days ago" is more useful than silence.
const HostReportTTL = 30 * time.Minute

// SetHostReport records what a host says about itself.
//
// Roles MERGE rather than replace, because a machine runs more than one crq
// service and each reports only its own: the autofix watcher and the review
// daemon on the same host would otherwise take turns overwriting each other,
// and the table would show whichever wrote last as the only thing running
// there.
//
// Each role therefore expires on its OWN last sighting, not on the record's. A
// shared At cannot answer this: the surviving service refreshes it every pass,
// so a carried-forward role from a service that stopped was renewed for ever
// and the dashboard reported it running indefinitely.
//
// Tool probes merge the same way and for the same reason: they describe the
// PATH of the service that took them, so they are kept per role and resolved
// into Tools rather than replaced wholesale by whoever wrote last.
func (s *State) SetHostReport(r HostReport, now time.Time) {
	if s.HostReports == nil {
		s.HostReports = map[string]HostReport{}
	}
	now = now.UTC()
	seen := map[string]time.Time{}
	tools := map[string][]ToolReport{}
	if prev, ok := s.HostReports[r.Host]; ok {
		for role, at := range prev.RoleSeen {
			seen[role] = at
		}
		for role, probed := range prev.RoleTools {
			tools[role] = probed
		}
		for _, role := range prev.Roles {
			// A record written before roles were dated (an older binary, or one
			// whose write dropped the member): its roles are exactly as old as it
			// is, which is the honest date to expire them from.
			if _, dated := seen[role]; !dated {
				seen[role] = prev.At
			}
			// Likewise for a record written before probes were kept per role:
			// its one list is the best any of its roles could say, so it stands
			// in for all of them until each reports again on its own.
			if _, probed := tools[role]; !probed && len(prev.Tools) > 0 {
				tools[role] = prev.Tools
			}
		}
	}
	for _, role := range r.Roles {
		seen[role] = now
		if len(r.Tools) > 0 {
			tools[role] = r.Tools
		}
	}
	roles := make([]string, 0, len(seen))
	for role, at := range seen {
		if now.Sub(at) > HostReportTTL {
			delete(seen, role)
			delete(tools, role)
			continue
		}
		roles = append(roles, role)
	}
	sort.Strings(roles)
	r.Roles = roles
	if len(seen) == 0 {
		seen = nil
	}
	r.RoleSeen = seen
	r.Tools = resolveTools(tools, roles, r.Tools)
	if len(tools) == 0 {
		tools = nil
	}
	r.RoleTools = tools
	r.At = now
	s.HostReports[r.Host] = r
}

// resolveTools answers "what can this host run" from the per-role probes, one
// tool at a time: the highest-ranked role that has an opinion about a tool owns
// the answer for it. fallback covers a host whose roles have all aged out —
// there is nobody left to speak for it, so the reporter's own list stands.
func resolveTools(byRole map[string][]ToolReport, roles []string, fallback []ToolReport) []ToolReport {
	ranked := append([]string(nil), roles...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return toolRoleRank(ranked[i]) < toolRoleRank(ranked[j])
	})
	var out []ToolReport
	claimed := map[string]bool{}
	for _, role := range ranked {
		for _, t := range byRole[role] {
			if claimed[t.Name] {
				continue
			}
			claimed[t.Name] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// ToolsReportedBy is what one role probed, or the record's resolved list when
// that role predates per-role probes. It is what a reporter must compare its
// own probe against: Tools is resolved across every service on the host, so a
// reporter whose answer another role outranks would read the difference as a
// change and rewrite state on every pass.
func (r HostReport) ToolsReportedBy(role string) []ToolReport {
	if probed, ok := r.RoleTools[role]; ok {
		return probed
	}
	return r.Tools
}

// StaleRole reports whether this record still names a role whose own last
// sighting has aged out. The record itself may be fresh — a second service on
// the host keeps writing it — which is why a caller deciding "nothing changed,
// skip the write" has to ask: skipping is what would keep the dead role listed.
func (r HostReport) StaleRole(now time.Time) bool {
	for _, role := range r.Roles {
		at, ok := r.RoleSeen[role]
		if !ok {
			at = r.At
		}
		if now.Sub(at) > HostReportTTL {
			return true
		}
	}
	return false
}

// RolesFresh reports whether every role in want was last seen within the given
// age. It is what a caller deciding "nothing changed, skip the write" must ask
// instead of looking at the record's own At: another service on the same host
// keeps that fresh, so a reporter that trusted it never refreshed its OWN role
// and let it age out from under itself — at which point the next writer of any
// kind prunes the still-running service from the table.
func (r HostReport) RolesFresh(want []string, now time.Time, within time.Duration) bool {
	for _, role := range want {
		at, ok := r.RoleSeen[role]
		if !ok {
			// Undated: only the record's own age can speak for it, and only if
			// the record still lists the role at all.
			if !r.lists(role) {
				return false
			}
			at = r.At
		}
		if now.Sub(at) >= within {
			return false
		}
	}
	return true
}

func (r HostReport) lists(role string) bool {
	for _, have := range r.Roles {
		if have == role {
			return true
		}
	}
	return false
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
