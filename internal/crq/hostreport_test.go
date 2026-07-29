package crq

import "testing"

func TestSameHostReportComparesCapabilitiesForTheReportingRole(t *testing.T) {
	prev := HostReport{
		Version: "2.1.0", Caps: 3,
		RoleCaps:     map[string]int{"autofix": 3, "serve": 1},
		RoleVersions: map[string]string{"autofix": "2.1.0", "serve": "2.0.0"},
	}
	serve := HostReport{Version: "2.0.0", Caps: 1, Roles: []string{"serve"}}
	if !sameHostReport(prev, serve) {
		t.Fatal("an unchanged older serve role was treated as different from another role's newer report")
	}
	serve.Caps = 2
	if sameHostReport(prev, serve) {
		t.Fatal("a changed serve capability was not detected")
	}
	serve.Caps, serve.Version = 1, "2.1.0"
	if sameHostReport(prev, serve) {
		t.Fatal("an upgraded serve binary was not detected when its capabilities stayed the same")
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
