package crq

import (
	"strings"
	"testing"
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
