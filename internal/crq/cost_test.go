package crq

import (
	"strings"
	"testing"
)

func TestCostSummaryDoesNotCallAnEntirelyUnpricedRoundFree(t *testing.T) {
	cost := RoundCost{
		Reviewers: []CostEstimate{{Bot: "unknown", Unknown: true}},
		Unpriced:  []string{"unknown"},
	}
	if got := costSummary(cost); got != "no basis to price 1 reviewer(s)" {
		t.Fatalf("costSummary = %q", got)
	}

	cost.Reviewers = append(cost.Reviewers, CostEstimate{Bot: "free", Exact: true})
	if got := costSummary(cost); !strings.Contains(got, "$0.00") {
		t.Fatalf("a known free reviewer should keep its figure, got %q", got)
	}
}
