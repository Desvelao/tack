//go:build linux

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gone reports whether pid no longer runs (a zombie nobody reaped counts).
func gone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := strings.LastIndexByte(string(b), ')')
	return i < 0 || i+2 >= len(b) || b[i+2] == 'Z'
}

// A wrapper that exits and leaves a background child behind: KillLeftovers
// kills the child, which is still in the wrapper's process group.
func TestKillLeftoversKillsWhatTheWrapperLeftBehind(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $! >"+pidFile)
	KillGroupOnCancel(cmd, 0)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || gone(pid) {
		t.Fatalf("the background child is not running: %q %v", b, err)
	}
	KillLeftovers(cmd)
	for i := 0; i < 100 && !gone(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !gone(pid) {
		t.Fatalf("leftover %d still runs", pid)
	}
}

// KillLeftovers also drops the pending grace-period SIGKILL, so it cannot fire
// at a pid that is free (or reused) by then.
func TestKillLeftoversCancelsTheGraceTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "trap '' TERM; sleep 60")
	KillGroupOnCancel(cmd, time.Hour)
	time.AfterFunc(200*time.Millisecond, cancel)
	done := make(chan struct{})
	go func() { cmd.Run(); close(done) }()
	for i := 0; ; i++ {
		if _, ok := graceTimers.Load(cmd); ok {
			break
		}
		if i > 100 {
			t.Fatal("no grace timer was armed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	KillLeftovers(cmd) // the shell ignores TERM, so this is what ends it
	<-done
	if _, ok := graceTimers.Load(cmd); ok {
		t.Fatal("the grace timer survived KillLeftovers")
	}
}
