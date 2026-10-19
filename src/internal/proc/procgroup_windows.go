//go:build windows

package proc

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// taskkillTimeout bounds the taskkill helper so a stuck one cannot hang a cancel.
const taskkillTimeout = 5 * time.Second

// killTree kills the process and everything it started (taskkill /T /F). It
// is only used while the process is known to be alive: once it has been
// waited for, its PID may belong to an unrelated process.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	tk := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if tk.Run() != nil {
		_ = cmd.Process.Kill() // at least the process itself
	}
}

// KillGroupOnCancel makes a timeout kill the whole process tree. Compile-checked
// only: it has not been run on Windows. A failing taskkill does not turn a
// normal timeout into a different error: the command is killed either way.
// grace is ignored: there is no SIGTERM, the tree is always killed at once.
func KillGroupOnCancel(cmd *exec.Cmd, grace time.Duration) {
	cmd.Cancel = func() error {
		killTree(cmd)
		return nil
	}
}

// KillLeftovers does nothing on Windows: after the command was waited for, its
// PID may be reused and taskkill /T cannot find its orphans reliably, so
// killing "what is left" could hit an unrelated process. Children that outlive
// the command keep running (a Job Object would be the way to track them).
func KillLeftovers(cmd *exec.Cmd) {}
