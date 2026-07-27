package crq

import (
	"strings"
	"testing"
)

// observe must consult the configuration it was HANDED, not the Service's own —
// otherwise a per-repository setting is read at the top and quietly ignored
// underneath: check runs for a bot this repo enabled would be fetched and then
// discarded, and a repo-specific review command would be missed and reposted.
//
// A grep is the honest test here. The bug is a whole class of site, not one
// behaviour, and every one of them compiles and passes today.
func TestObserveUsesOnlyTheConfigItWasGiven(t *testing.T) {
	// Scoped to observe itself. Checking the whole file would fail on an
	// unrelated method, or on the words appearing in a comment.
	body, ok := funcBody(readSource(t, "internal/crq/observe.go"), "func (s *Service) observe(")
	if !ok {
		t.Fatal("observe not found; this test is checking the wrong thing")
	}
	if !strings.Contains(body, "cfg Config") {
		t.Fatal("observe no longer takes a Config; this test is checking the wrong thing")
	}
	if n := strings.Count(body, "s.cfg."); n != 0 {
		t.Errorf("observe reaches for the Service configuration %d times; it must use the cfg parameter", n)
	}
}

// funcBody returns the source of the function whose signature starts with
// prefix, from the signature to the closing brace at column 0.
func funcBody(source, prefix string) (string, bool) {
	start := strings.Index(source, prefix)
	if start < 0 {
		return "", false
	}
	rest := source[start:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end], true
	}
	return rest, true
}
