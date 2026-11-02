//go:build !unix

package procx

import (
	"os/exec"
	"strconv"
)

// group makes the cancel stop the command with the processes it started:
// on Windows, taskkill over the process tree. Elsewhere the command alone.
func group(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if kill.Run() != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
