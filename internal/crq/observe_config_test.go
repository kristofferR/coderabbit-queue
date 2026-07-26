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
	source := readSource(t, "internal/crq/observe.go")
	if n := strings.Count(source, "s.cfg."); n != 0 {
		t.Errorf("observe.go reaches for the Service configuration %d times; it must use the cfg parameter", n)
	}
	if !strings.Contains(source, "func (s *Service) observe(ctx context.Context, cfg Config,") {
		t.Error("observe no longer takes a Config; this test is checking the wrong thing")
	}
}
