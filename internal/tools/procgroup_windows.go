//go:build windows

package tools

import "os/exec"

// Windows has no process groups in the POSIX sense, and metron does not ship
// Windows builds. These keep the package compiling for anyone building there:
// the command is still killed when the context expires, but its children are
// not.
func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
