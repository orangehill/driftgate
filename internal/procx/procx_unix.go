//go:build unix

package procx

import (
	"os/exec"
	"syscall"
)

// group starts the command as the leader of a new process group and makes
// the cancel kill the group. Until the command is waited for, its ID is
// still the group's, so the signal cannot reach another process.
func group(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
