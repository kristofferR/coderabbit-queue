package crq

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParsePreflightStream(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"review_context","reviewType":"uncommitted","currentBranch":"feature","baseBranch":"main","workingDirectory":"repo"}`,
		`{"type":"status","phase":"analyzing","status":"building_code_graph","message":"Mapping code changes"}`,
		`{"type":"heartbeat","status":"reviewing"}`,
		`{"type":"finding","severity":"major","fileName":"src/app.ts","startLine":42,"endLine":44,"title":"Guard nil config","comment":"This can crash.","codegenInstructions":"Add a nil guard.","suggestions":["if (!config) return;"],"fingerprint":"abc123"}`,
		`{"type":"complete","status":"review_completed","findings":1}`,
	}, "\n")
	report := PreflightReport{}
	if err := parsePreflightStream(strings.NewReader(input), &report); err != nil {
		t.Fatal(err)
	}
	if report.ReviewContext["currentBranch"] != "feature" {
		t.Fatalf("review context mismatch: %#v", report.ReviewContext)
	}
	if len(report.Statuses) != 2 {
		t.Fatalf("status count mismatch: %#v", report.Statuses)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("finding count mismatch: %#v", report.Findings)
	}
	finding := report.Findings[0]
	if finding.ID != "abc123" || finding.Severity != "major" || finding.Path != "src/app.ts" || finding.Line != 42 || finding.EndLine != 44 {
		t.Fatalf("finding mismatch: %#v", finding)
	}
	if finding.Body != "Add a nil guard." || len(finding.Suggestions) != 1 {
		t.Fatalf("finding guidance mismatch: %#v", finding)
	}
	if report.Complete["status"] != "review_completed" {
		t.Fatalf("complete mismatch: %#v", report.Complete)
	}
}

func TestPreflightParseFailureReportsExitOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary is POSIX-only")
	}
	// A fake CodeRabbit CLI that emits a non-JSON line then exits 0. The parse
	// failure must surface as exit code 1 — our own cancel() of the child (to
	// unblock Wait) must not be misreported as a timeout/cancellation (exit 2).
	dir := t.TempDir()
	script := filepath.Join(dir, "cr")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'not json'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, code, err := Preflight(context.Background(), PreflightOptions{Binary: script, Timeout: 10 * time.Second})
	if code != 1 || report.ExitCode != 1 {
		t.Fatalf("expected exit code 1 for a parse failure, got code=%d report.ExitCode=%d (err=%v)", code, report.ExitCode, err)
	}
	if report.Status != "error" || err == nil {
		t.Fatalf("expected an error report, got status=%q err=%v", report.Status, err)
	}
}

func TestPreflightFindingFallsBackToComment(t *testing.T) {
	finding := preflightFinding(map[string]any{
		"type":     "finding",
		"fileName": "main.go",
		"line":     float64(7),
		"comment":  "Potential issue: validate input.",
	})
	if finding.Body != "Potential issue: validate input." {
		t.Fatalf("body mismatch: %#v", finding)
	}
	if finding.ID == "" {
		t.Fatal("expected generated id")
	}
	if finding.Severity == "" {
		t.Fatal("expected inferred severity")
	}
}
