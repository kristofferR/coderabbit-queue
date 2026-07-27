package main

import (
	"os"
	"path/filepath"
	"testing"
)

// One stale crq erased every dispatch claim in the fleet, and nothing said so.
// doctor cannot stop it — the binary that does the damage is older than any
// mechanism meant to catch it — but it can name it.
func TestOtherInstallsNamesADifferentBuild(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mine, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}

	stale := t.TempDir()
	same := t.TempDir()
	if err := os.WriteFile(filepath.Join(stale, "crq"), []byte("an older build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(same, "crq"), mine, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stale+string(os.PathListSeparator)+same)
	t.Setenv("GOPATH", t.TempDir()) // no crq there: an empty directory is not a finding

	found := otherInstalls()
	if len(found) != 2 {
		t.Fatalf("found %d installs, want the two planted ones: %+v", len(found), found)
	}
	byPath := map[string]otherInstall{}
	for _, in := range found {
		byPath[in.Path] = in
	}
	if in, ok := byPath[filepath.Join(stale, "crq")]; !ok || in.SameBuild {
		t.Errorf("the stale build was not reported as different: %+v", in)
	}
	if in, ok := byPath[filepath.Join(same, "crq")]; !ok || !in.SameBuild {
		t.Errorf("a copy of this build was reported as different: %+v", in)
	}
}

// A directory on PATH that holds no crq, or holds something unexecutable, is
// not a second install.
func TestOtherInstallsIgnoresWhatItCannotRun(t *testing.T) {
	empty := t.TempDir()
	unexec := t.TempDir()
	if err := os.WriteFile(filepath.Join(unexec, "crq"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", empty+string(os.PathListSeparator)+unexec)
	t.Setenv("GOPATH", t.TempDir())
	if found := otherInstalls(); len(found) != 0 {
		t.Errorf("reported %+v, want nothing", found)
	}
}
