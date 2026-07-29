package crq

import "testing"

func TestSameHostReportComparesCapabilitiesForTheReportingRole(t *testing.T) {
	prev := HostReport{
		Version: "newer", Caps: 3,
		RoleCaps: map[string]int{"autofix": 3, "serve": 1},
	}
	serve := HostReport{Version: "older", Caps: 1, Roles: []string{"serve"}}
	if !sameHostReport(prev, serve) {
		t.Fatal("an unchanged older serve role was treated as different from another role's newer report")
	}
	serve.Caps = 2
	if sameHostReport(prev, serve) {
		t.Fatal("a changed serve capability was not detected")
	}
}

func TestSameHostReportLetsAutofixClearItsAgent(t *testing.T) {
	prev := HostReport{Agent: "claude", Roles: []string{"autofix"}}
	if sameHostReport(prev, HostReport{Roles: []string{"autofix"}}) {
		t.Fatal("autofix removing its agent must be recorded")
	}
	if !sameHostReport(prev, HostReport{Roles: []string{"serve"}}) {
		t.Fatal("a role that does not choose the fix agent must preserve it silently")
	}
}
