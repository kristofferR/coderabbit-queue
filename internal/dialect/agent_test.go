package dialect

import (
	"fmt"
	"testing"
	"time"
)

func TestClassifyAgentFailure(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	log := []byte(fmt.Sprintf(`{"type":"result","error": "rate_limit","resetsAt":%d}`, reset.Unix()))
	got := ClassifyAgentFailure(log, now)
	if !got.Unavailable {
		t.Fatal("machine-readable provider outage was not classified")
	}
	if got.RetryAt.Unix() != reset.Unix() {
		t.Fatalf("retry = %s, want timestamp from the response", got.RetryAt)
	}
	if ordinary := ClassifyAgentFailure([]byte("tests failed after editing"), reset); ordinary.Unavailable {
		t.Fatal("an ordinary bad fix was refunded as a provider outage")
	}
	repositoryOutput := []byte(`The review says "service unavailable".
The test printed: rate limit exceeded.
source := ` + "`\"type\":\"overloaded_error\"`" + `
`)
	if ordinary := ClassifyAgentFailure(repositoryOutput, reset); ordinary.Unavailable {
		t.Fatal("repository-controlled text was treated as a provider outage")
	}
	repositoryJSON := []byte(
		`{"type":"assistant","message":{"content":[{"type":"tool_result","content":{"error":"rate_limit","resetsAt":4102444800}}]}}`,
	)
	if ordinary := ClassifyAgentFailure(repositoryJSON, reset); ordinary.Unavailable {
		t.Fatal("repository-controlled JSON was treated as a provider outage")
	}
	apiError := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	if outage := ClassifyAgentFailure(apiError, reset); !outage.Unavailable {
		t.Fatal("the provider's top-level error envelope was not classified")
	}
}
