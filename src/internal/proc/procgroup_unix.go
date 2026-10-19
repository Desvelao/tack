//go:build !windows

package proc

import (
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// graceTimers holds, per command, the pending SIGKILL of a grace period.
var graceTimers sync.Map // *exec.Cmd -> *time.Timer

// KillGroupOnCancel starts the command in its own process group and, on
// timeout or cancel, ends the whole group so children of a wrapper script die
// too. With grace > 0 the group gets SIGTERM first and SIGKILL once grace has
// passed (unless the command is gone by then); with 0 it is SIGKILL at once.
// It raises cmd.WaitDelay to grace plus a second when the cancel comes, or Wait
// would kill the command sooner (a normal exit keeps the WaitDelay set before).
func KillGroupOnCancel(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		if cmd.WaitDelay < grace+time.Second {
			cmd.WaitDelay = grace + time.Second // only the cancelling goroutine and the Wait after it touch this
		}
		if grace <= 0 {
			return syscall.Kill(-pid, syscall.SIGKILL)
		}
		err := syscall.Kill(-pid, syscall.SIGTERM)
		graceTimers.Store(cmd, time.AfterFunc(grace, func() {
			graceTimers.Delete(cmd)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}))
		return err
	}
}

// KillLeftovers kills whatever is still in the command's process group once the
// command itself has finished (a wrapper script's background children). It also
// cancels a pending grace-period kill: this one does the job.
func KillLeftovers(cmd *exec.Cmd) {
	if t, ok := graceTimers.LoadAndDelete(cmd); ok {
		t.(*time.Timer).Stop()
	}
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH when nothing is left
	}
}
