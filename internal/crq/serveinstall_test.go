package crq

import (
	"context"
	"strings"
	"testing"
)

func TestServeUnitCarriesShellProvidedConfiguration(t *testing.T) {
	env := map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_STATE_REF": "custom-state",
		"CRQ_SCOPE": "owner,second", "CRQ_HOST": "testhost", "CRQ_COBOTS": "",
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	cfg, err := BuildConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	plan, err := svc.InstallAutoReview(context.Background(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	unit := serveUnitBody(plan)
	for _, want := range []string{
		"CRQ_REPO=owner/gate",
		"CRQ_STATE_REF=custom-state",
		"CRQ_SCOPE=owner,second",
		"CRQ_HOST=testhost",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not carry %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "GITHUB_TOKEN") || strings.Contains(unit, "GH_TOKEN") {
		t.Errorf("unit carries a GitHub credential:\n%s", unit)
	}
}
