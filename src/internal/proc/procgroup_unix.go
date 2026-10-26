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

// KillGroupOnCancel starts the command in its own session (hence its own process group) and, on
// timeout or cancel, ends the whole group so children of a wrapper script die
// too. With grace > 0 the group gets SIGTERM first and SIGKILL once grace has
// passed (unless the command is gone by then); with 0 it is SIGKILL at once.
// It raises cmd.WaitDelay to grace plus a second when the cancel comes, or Wait
// would kill the command sooner (a normal exit keeps the WaitDelay set before).
func KillGroupOnCancel(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // own session (and group): no controlling terminal, so `bash -ic` in a background action cannot stop on SIGTTIN/SIGTTOU
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
//
// The group id is the leader's pid (Setsid), and the leader has been reaped by
// now. That is safe while any member is left: the kernel does not hand a pid out
// again while a process group or session still uses it, so -pid can only be
// the leftovers. With no member left the call finds nothing (ESRCH); a different
// group could only get that number after a whole pid wrap-around, which does
// not happen in the instant between Wait and this call. The grace timer is the
// longer window, which is why this stops it, and why every caller of
// KillGroupOnCancel with a grace must call KillLeftovers after Wait.
func KillLeftovers(cmd *exec.Cmd) {
	if t, ok := graceTimers.LoadAndDelete(cmd); ok {
		t.(*time.Timer).Stop()
	}
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH when nothing is left
	}
}
