//go:build darwin || linux

package crq

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureDispatchProcess gives one fix session its own process group. A
// canceled watcher must stop the agent and every shell, test, or git process it
// started before the dispatch claim and worktree are released.
func configureDispatchProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
