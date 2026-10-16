//go:build windows

package custom

import (
	"os/exec"
	"strconv"
)

// killTree kills the process and everything it started (taskkill /T /F).
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}

// killGroupOnCancel makes a timeout kill the whole process tree. Compile-checked
// only: it has not been run on Windows.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return killTree(cmd) }
}

// killLeftovers kills what the command left running once it has finished. By
// then the process may be gone, so taskkill can fail; that is fine.
func killLeftovers(cmd *exec.Cmd) { _ = killTree(cmd) }
