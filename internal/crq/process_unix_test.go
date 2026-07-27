//go:build darwin || linux

package crq

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDispatchCancellationStopsDescendants(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	finished := filepath.Join(dir, "finished")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c",
		`(sleep 1; printf done > "$1") & printf ready > "$2"; wait`, "sh", finished, ready)
	configureDispatchProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("canceled process exited successfully")
	}

	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(finished); !os.IsNotExist(err) {
		t.Fatalf("descendant survived cancellation: %v", err)
	}
}
