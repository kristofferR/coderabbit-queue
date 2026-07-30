package crq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixSessionPromptRejectsWhitespaceOnlyInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRQ_FIX_PROMPT_FILE", path)
	t.Setenv("CRQ_FIX_PROMPT", " ")

	if _, err := fixSessionPrompt(); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("fixSessionPrompt error = %v, want an empty-prompt error", err)
	}
}
