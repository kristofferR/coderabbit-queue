package state

import "testing"

func TestSolverSettingsWithLegacyAskModeIsNotEmpty(t *testing.T) {
	if (SolverSettings{AskMode: "uncertain"}).Empty() {
		t.Fatal("a legacy ask-mode value is configuration, not an empty record")
	}
}

func TestSolverSettingsExplicitEmptyPromptOverridesFleetPrompt(t *testing.T) {
	fleet := SolverSettings{Prompt: "fleet instructions"}
	repo := SolverSettings{SetPrompt: true}

	got := fleet.Merge(repo)
	if got.Prompt != "" || !got.SetPrompt {
		t.Fatalf("merged prompt = %q (set=%t), want an explicit empty prompt", got.Prompt, got.SetPrompt)
	}
	if repo.Empty() {
		t.Fatal("an explicitly empty prompt must keep the repository record")
	}
}
