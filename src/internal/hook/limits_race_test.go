package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tack/internal/task"
)

// waitReturns fails the test when Wait does not come back: a miscounted wg
// token (too many) hangs it, a missing one panics with a negative counter.
func waitReturns(t *testing.T, run *Runner) {
	t.Helper()
	done := make(chan struct{})
	go func() { run.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return")
	}
	if n := run.Running(); n != 0 {
		t.Fatalf("Running() = %d after Wait, want 0", n)
	}
}

// startedIn lists the ids a hook command appended to out.
func startedIn(out string) []string {
	b, _ := os.ReadFile(out)
	return strings.Fields(string(b))
}

// runnerFor builds a Runner over specs whose commands append their id to
// out, then run `tail` (a shell snippet).
func runnerFor(t *testing.T, tail string, specs ...Spec) (*Runner, string) {
	t.Helper()
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	for i := range specs {
		specs[i].Command = []string{"sh", "-c", `echo "$0" >> ` + out + `; ` + tail, "{id}"}
	}
	if err := ValidateAll(specs); err != nil {
		t.Fatal(err)
	}
	return New(specs, dir, "cli", nil, &rec{}), out
}

// Whatever the queue mode and limits, every event fired is either started or
// counted as dropped, and Wait comes back with nothing left running.
func TestLimitsAccountForEveryEvent(t *testing.T) {
	const events = 12
	for _, queue := range []string{"drop", "latest", "all"} {
		for _, perHook := range []int{1, 2} {
			for _, global := range []int{0, 1, 3} {
				t.Run(fmt.Sprintf("%s/max_parallel=%d/global=%d", queue, perHook, global), func(t *testing.T) {
					a, b := spec("a", "", "task.moved"), spec("b", "", "task.moved")
					for _, s := range []*Spec{&a, &b} {
						s.Queue, s.MaxParallel = queue, perHook
					}
					run, out := runnerFor(t, "sleep 0.05", a, b)
					run.SetMaxParallel(global)

					var wg sync.WaitGroup
					for i := 0; i < events; i++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							run.Fire(Event{Name: TaskMoved, New: task.Task{ID: fmt.Sprint("t", i), Backend: "m"}})
						}()
					}
					wg.Wait()
					waitReturns(t, run)

					started := len(startedIn(out))
					dropped := run.Dropped("a") + run.Dropped("b")
					if started+dropped != 2*events {
						t.Errorf("started %d + dropped %d != %d fired", started, dropped, 2*events)
					}
					if queue == "all" && dropped != 0 {
						t.Errorf("queue all dropped %d events with room in the queue", dropped)
					}
				})
			}
		}
	}
}

// With hook_max_parallel 1 and queue all, a slot freed by one hook runs the
// queued events of the other one too.
func TestFreedGlobalSlotPumpsOtherHooksQueue(t *testing.T) {
	a, b := spec("a", "", "task.moved"), spec("b", "", "task.moved")
	a.Queue, b.Queue = "all", "all"
	run, out := runnerFor(t, "sleep 0.05", a, b)
	run.SetMaxParallel(1)
	fireIDs(run, TaskMoved, "1", "2", "3")
	waitReturns(t, run)
	if got := len(startedIn(out)); got != 6 {
		t.Fatalf("started %d, want 6 (3 events x 2 hooks)", got)
	}
}

// Cancel with events queued behind an armed min_interval timer: the timer is
// stopped, the queued tokens are released and nothing starts later.
func TestCancelWithArmedMinIntervalTimer(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.MinInterval, s.Queue = "1h", "all"
	run, out := runnerFor(t, "true", s)
	fireIDs(run, TaskMoved, "1", "2", "3")
	waitFor(t, "first start", func() bool { return len(startedIn(out)) == 1 })
	waitFor(t, "the queue to hold the others", func() bool { return run.Running() >= 2 })
	run.Cancel()
	waitReturns(t, run)
	time.Sleep(50 * time.Millisecond)
	eq(t, startedIn(out), "1")
}

// A retry waiting out its delay ends when Cancel comes, without another start.
func TestCancelInterruptsRetries(t *testing.T) {
	s := spec("flaky", "", "task.moved")
	s.Retries, s.RetryDelay = 5, "30s"
	run, out := runnerFor(t, "exit 1", s)
	fireIDs(run, TaskMoved, "1")
	waitFor(t, "first attempt", func() bool { return len(startedIn(out)) == 1 })
	time.Sleep(50 * time.Millisecond) // into the retry delay
	start := time.Now()
	run.Cancel()
	waitReturns(t, run)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Wait took %s after Cancel", d)
	}
	eq(t, startedIn(out), "1")
}

// Events keep arriving while Cancel runs and the running jobs pump their
// queues: no token may be lost or released twice (run under make race).
func TestCancelWhileFiring(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Queue, s.MaxParallel = "all", 2
	run, _ := runnerFor(t, "sleep 0.02", s)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				fireIDs(run, TaskMoved, fmt.Sprint(g, "-", i))
				time.Sleep(time.Millisecond)
			}
		}()
	}
	time.Sleep(15 * time.Millisecond)
	run.Cancel()
	wg.Wait()
	waitReturns(t, run)
}

// unique: task drops the event for a task that is queued, then lets it run
// once the first run ended.
func TestUniqueTaskWithQueueAllAndLimits(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Unique, s.Queue, s.MaxParallel = UniqueTask, "all", 1
	run, out := runnerFor(t, "sleep 0.1", s)
	fireIDs(run, TaskMoved, "1", "2", "2", "1", "3")
	waitReturns(t, run)
	eq(t, sorted(startedIn(out)), "1", "2", "3")
	if d := run.Dropped("slow"); d != 2 {
		t.Errorf("dropped %d, want 2", d)
	}
}
