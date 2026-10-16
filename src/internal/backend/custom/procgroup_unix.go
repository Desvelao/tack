//go:build !windows

package custom

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts the command in its own process group and, on
// timeout, kills the whole group so children of a wrapper script die too.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killLeftovers kills whatever is still in the command's process group once the
// command itself has finished (a wrapper script's background children).
func killLeftovers(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH when nothing is left
	}
}
