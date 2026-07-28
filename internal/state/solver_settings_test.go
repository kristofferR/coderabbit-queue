package state

import "testing"

func TestSolverSettingsWithLegacyAskModeIsNotEmpty(t *testing.T) {
	if (SolverSettings{AskMode: "uncertain"}).Empty() {
		t.Fatal("a legacy ask-mode value is configuration, not an empty record")
	}
}
