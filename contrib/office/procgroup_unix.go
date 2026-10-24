//go:build unix

package office

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a process group of its own and makes its
// cancellation kill the whole group: soffice is a launcher that starts
// soffice.bin, and killing the launcher alone would leave the converter
// running.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// killGroup kills whatever cmd's group still holds after cmd ended, so a
// finished conversion leaves no process behind. The group is gone when
// nothing is left, which is not an error.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
