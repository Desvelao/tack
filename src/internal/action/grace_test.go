//go:build !windows

package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTermTrapExitsCleanlyWithinTheGrace(t *testing.T) {
	dir := t.TempDir()
	s := spec("sh", "-c", `trap 'echo cleaned > "$1"; exit 0' TERM; echo ready; while :; do sleep 0.05; done`, "sh", filepath.Join(dir, "clean"))
	s.Timeout, s.KillGrace = "300ms", "5s"
	start := time.Now()
	res, err := s.Run(context.Background(), sample, dir)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v (%q)", err, res.Stdout)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("waited the whole grace: %s", time.Since(start))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "clean")); strings.TrimSpace(string(b)) != "cleaned" {
		t.Fatalf("the TERM handler did not run: %q", b)
	}
}

func TestIgnoredTermIsKilledAfterTheGrace(t *testing.T) {
	s := spec("sh", "-c", `trap '' TERM; while :; do sleep 0.05; done`)
	s.Timeout, s.KillGrace = "200ms", "700ms"
	start := time.Now()
	_, err := s.Run(context.Background(), sample, t.TempDir())
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if took < 800*time.Millisecond || took > 4*time.Second {
		t.Fatalf("killed after %s, want about timeout+grace (900ms)", took)
	}
}

func TestZeroGraceKillsAtOnce(t *testing.T) {
	s := spec("sh", "-c", `trap '' TERM; while :; do sleep 0.05; done`)
	s.Timeout, s.KillGrace = "200ms", "0s"
	start := time.Now()
	if _, err := s.Run(context.Background(), sample, t.TempDir()); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestCancelGivesTheGraceToo(t *testing.T) {
	dir := t.TempDir()
	s := spec("sh", "-c", `trap 'echo bye > "$1"; exit 0' TERM; echo ready; while :; do sleep 0.05; done`, "sh", filepath.Join(dir, "bye"))
	j := s.Start(context.Background(), sample, dir)
	for !strings.Contains(j.Snapshot().Stdout, "ready") {
		time.Sleep(10 * time.Millisecond)
	}
	j.Cancel()
	<-j.Done()
	if snap := j.Snapshot(); !snap.Canceled || snap.Err == nil {
		t.Fatalf("%+v", snap)
	}
	if _, err := os.Stat(filepath.Join(dir, "bye")); err != nil {
		t.Fatal("the TERM handler did not run")
	}
}

func TestMaxOutputIsPerAction(t *testing.T) {
	s := spec("sh", "-c", "head -c 5000 /dev/zero | tr '\\0' a")
	s.MaxOutput = 1
	res, err := s.Run(context.Background(), sample, t.TempDir())
	if err != nil || len(res.Stdout) != 1024 {
		t.Fatalf("len %d, err %v", len(res.Stdout), err)
	}
}

func TestExclusiveAllowsOneRunPerCard(t *testing.T) {
	s := spec("sleep", "30")
	s.Exclusive = true
	dir := t.TempDir()
	first := s.Start(context.Background(), sample, dir)
	second := s.Start(context.Background(), sample, dir)
	<-second.Done()
	if snap := second.Snapshot(); snap.Err == nil || !strings.Contains(snap.Err.Error(), "already running") {
		t.Fatalf("%+v", snap)
	}
	other := sample
	other.ID = "8"
	third := s.Start(context.Background(), other, dir)
	if snap := third.Snapshot(); !snap.Running {
		t.Fatalf("another card must run: %+v", snap)
	}
	first.Cancel()
	third.Cancel()
	<-first.Done()
	<-third.Done()
	again := s.Start(context.Background(), sample, dir)
	if snap := again.Snapshot(); !snap.Running {
		t.Fatalf("the card must be free after the run ended: %+v", snap)
	}
	again.Cancel()
	<-again.Done()
}

func TestInteractiveTimeoutEndsTheCommand(t *testing.T) {
	s := spec("sh", "-c", "sleep 30")
	s.InteractiveTimeout, s.KillGrace = "200ms", "0s"
	cmd, err := s.Cmd(sample, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := cmd.Run(); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestValidateNewKeys(t *testing.T) {
	for _, bad := range []func(*Spec){
		func(s *Spec) { s.KillGrace = "soon" },
		func(s *Spec) { s.KillGrace = "-1s" },
		func(s *Spec) { s.InteractiveTimeout = "0s" },
		func(s *Spec) { s.MaxOutput = -1 },
	} {
		s := spec("true")
		bad(&s)
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	s := spec("true")
	s.KillGrace, s.InteractiveTimeout, s.MaxOutput = "0s", "2h", 10
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
