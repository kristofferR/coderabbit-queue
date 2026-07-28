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
		"CRQ_DRY_RUN": "1", "CRQ_NO_OPEN": "1",
		"CRQ_DISPATCH_REPO": "owner/pr", "CRQ_DISPATCH_PR": "7",
		"CRQ_DISPATCH_HEAD": "abcdef", "CRQ_DISPATCH_FINDINGS": "/tmp/findings.json",
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
	for _, excluded := range []string{
		"CRQ_DRY_RUN", "CRQ_NO_OPEN", "CRQ_DISPATCH_REPO", "CRQ_DISPATCH_PR",
		"CRQ_DISPATCH_HEAD", "CRQ_DISPATCH_FINDINGS",
	} {
		if strings.Contains(unit, excluded) {
			t.Errorf("unit carries excluded %s:\n%s", excluded, unit)
		}
	}
}

func TestSystemdEnvironmentPreservesUnicodeAndUsesSupportedEscapes(t *testing.T) {
	value := "CRQ_FIX_PROMPT=blå\u0085\t\"quoted\""
	got := systemdEnvironment(value)
	want := "\"CRQ_FIX_PROMPT=blå\u0085\\t\\\"quoted\\\"\""
	if got != want {
		t.Fatalf("systemd environment = %q, want %q", got, want)
	}
	if got := systemdEnvironment("CRQ_FIX_PROMPT=" + string([]byte{0xff})); got != `"CRQ_FIX_PROMPT=\xff"` {
		t.Fatalf("invalid UTF-8 environment = %q, want a lossless hex escape", got)
	}
}
