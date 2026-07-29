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
