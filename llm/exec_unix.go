//go:build unix

package llm

import (
	"os/exec"
	"syscall"
)

// setGroupKill puts the child in its own process group and makes cancellation
// SIGKILL the group, so helpers the CLI spawned die with it.
func setGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
