package crq

import (
	"os"
	"path/filepath"
	"testing"
)

// readSource returns a repository file's contents, for the few tests whose
// subject is the shape of the code rather than its behaviour.
func readSource(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, rel)
		if data, err := os.ReadFile(path); err == nil {
			return string(data)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find %s from %s", rel, dir)
	return ""
}
