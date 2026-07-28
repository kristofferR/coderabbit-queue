package crq

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// probedTools is what crq wants to know about a host. The purpose text lives in
// the dashboard; what matters here is the list, and that every host probes the
// same one so two reports can be compared.
var probedTools = []string{"crq", "git", "gh", "claude", "codex", "coderabbit", "macroscope"}

// ReportHost records what THIS machine can reach, so the fleet can be read as a
// fleet rather than as whichever host you happened to ask.
//
// The PATH it probes is the reporting process's own, which is the point: a
// daemon reports the PATH its service runs with, and that is the one that
// decides whether a fix session can start. A tool installed for the shell and
// invisible to the service is the failure this exists to make visible, and it
// cannot be seen from the shell.
func (s *Service) ReportHost(ctx context.Context, roles ...string) {
	report := HostReport{
		Host:    s.cfg.Host,
		Version: Version,
		Caps:    WriterCaps,
		Roles:   roles,
	}
	for _, name := range probedTools {
		t := ToolReport{Name: name}
		if path, err := exec.LookPath(name); err == nil {
			t.Path = path
			t.Version = toolVersion(ctx, path)
		}
		report.Tools = append(report.Tools, t)
	}

	now := s.clock().UTC()
	if _, err := s.store.Update(ctx, func(st *State) error {
		// Compared against what the merge WOULD produce, not against this
		// process's own view: another service on this host may have added a
		// role since, and treating that as "nothing changed" would leave the
		// merged record un-refreshed until it aged out.
		//
		// Freshness is asked of THIS reporter's own roles, not of the record.
		// A host running two services has the other one refreshing At on every
		// pass, so a record-wide check suppressed this write until the role
		// being reported crossed the TTL — and then whichever service wrote
		// next pruned a service that never stopped running.
		if prev, ok := st.HostReports[report.Host]; ok && sameHostReport(prev, report) &&
			!prev.StaleRole(now) && prev.RolesFresh(report.Roles, now, HostReportTTL/2) {
			// Nothing changed and the record is still fresh. Rewriting it would
			// bump the state revision on every pass and make the dashboard's
			// change feed report a fleet that is constantly doing something.
			return ErrNoChange
		}
		st.SetHostReport(report, now)
		return nil
	}); err != nil && !errors.Is(err, ErrNoChange) && s.log != nil {
		s.log.Printf("warning: reporting host tools: %v", err)
	}
}

// toolVersion asks a tool what it is, briefly. Anything that does not answer in
// two seconds, or answers with something unhelpful, reports no version rather
// than holding up a pass: this is for a person reading a table.
func toolVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	if len(line) > 60 {
		line = line[:60]
	}
	return line
}

// sameHostReport reports whether the stored record already says what this
// reporter is about to say. The tool comparison is per ROLE: the record's own
// Tools list is resolved across every service on the host, so comparing against
// it made a reporter whose PATH another role outranks see a difference on every
// pass and rewrite state for ever.
func sameHostReport(prev, next HostReport) bool {
	if prev.Version != next.Version || prev.Caps != next.Caps {
		return false
	}
	for _, role := range next.Roles {
		stored := prev.ToolsReportedBy(role)
		if len(stored) != len(next.Tools) {
			return false
		}
		for i := range stored {
			if stored[i] != next.Tools[i] {
				return false
			}
		}
	}
	return true
}
