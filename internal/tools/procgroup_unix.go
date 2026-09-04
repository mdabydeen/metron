//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group, so that killing it
// kills whatever it started as well.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the whole group. The negative pid is what makes it a
// group signal rather than a signal to the leader alone -- `go test` builds and
// then runs a separate test binary, and killing only `go` leaves that binary
// running well past the timeout that was meant to stop it.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
