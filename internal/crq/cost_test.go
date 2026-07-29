package crq

import (
	"strings"
	"testing"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
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

func TestCostUsesZeroLowerBoundForConditionalPaidReviewer(t *testing.T) {
	cfg, err := BuildConfig(map[string]string{
		"CRQ_REPO":   "o/gate",
		"CRQ_COBOTS": "macroscope",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: cfg}
	cost := svc.costWith(DefaultState(cfg), "o/repo", 1, "abcdef123", dialect.DiffStat{
		Additions: 10, ChangedFiles: 1,
	}, dialect.Allowance{})
	for _, estimate := range cost.Reviewers {
		if !strings.Contains(strings.ToLower(estimate.Bot), "macroscope") {
			continue
		}
		if estimate.Low != 0 || estimate.High <= 0 || estimate.Exact {
			t.Fatalf("conditional reviewer estimate = %+v, want a zero-to-price range", estimate)
		}
		return
	}
	t.Fatal("macroscope estimate was not included")
}
