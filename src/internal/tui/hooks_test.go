package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tkan/internal/board"
	"tkan/internal/hook"
)

// hookLog returns a Runner whose hooks append "event id" to a file, and a reader of that file.
func hookLog(t *testing.T, on ...string) (*hook.Runner, func() []string) {
	t.Helper()
	t.Setenv(hook.DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "events.txt")
	s := hook.Spec{On: on}
	s.Name = "log"
	s.Command = []string{"sh", "-c", `echo "$0" >> ` + out, "{event}:{backend}:{id}"}
	if err := hook.ValidateAll([]hook.Spec{s}); err != nil {
		t.Fatal(err)
	}
	r := hook.New([]hook.Spec{s}, dir, "tui", nil, nil)
	return r, func() []string {
		r.Wait()
		b, _ := os.ReadFile(out)
		return strings.Fields(string(b))
	}
}

func twoBackends(t *testing.T) (Model, string, string) {
	t.Helper()
	dir := t.TempDir()
	p1, p2 := filepath.Join(dir, "a.csv"), filepath.Join(dir, "b.csv")
	for p, rows := range map[string]string{p1: "1,One,pending\n2,Two,pending\n", p2: "1,Uno,pending\n2,Dos,pending\n3,Tres,pending\n"} {
		if err := os.WriteFile(p, []byte("id,title,status\n"+rows), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(openSet(t, p1, p2), "x", board.DefaultSpec()), p1, p2
}

// A backend that failed to load and comes back must not report all of its
// tasks as new, and the first load must not report anything.
func TestAppearedDoesNotFloodWhenABackendRecovers(t *testing.T) {
	r, events := hookLog(t, hook.TaskAppeared, hook.TaskDisappeared)
	m, _, p2 := twoBackends(t)
	m = m.WithHooks(r)
	m.loaded, m.partial = false, nil // the model was built before the runner was attached
	m.reload()                       // first load: nothing is news
	if got := events(); len(got) != 0 {
		t.Fatalf("first load fired %v", got)
	}

	saved, _ := os.ReadFile(p2)
	if err := os.Remove(p2); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err == nil {
		t.Fatal("expected the failed backend to be reported")
	}
	if got := events(); len(got) != 0 {
		t.Fatalf("a backend failing fired %v", got)
	}
	if err := os.WriteFile(p2, saved, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}
	if got := events(); len(got) != 0 {
		t.Fatalf("the recovery fired %v", got)
	}

	// Real outside changes are seen again once it has been stable for a list.
	if err := os.WriteFile(p2, append(saved, []byte("4,Cuatro,pending\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	m.reload()
	if got := events(); len(got) != 1 || got[0] != "task.appeared:csv2:4" {
		t.Fatalf("got %v", got)
	}
}

func TestFirstLoadThatIsPartialDoesNotFloodOnTheSecond(t *testing.T) {
	r, events := hookLog(t, hook.TaskAppeared)
	dir := t.TempDir()
	good, missing := filepath.Join(dir, "a.csv"), filepath.Join(dir, "missing.csv")
	if err := os.WriteFile(good, []byte("id,title,status\n1,One,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missing, []byte("id,title,status\n1,Uno,pending\n2,Dos,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	be := openSet(t, good, missing)
	hidden := missing + ".away"
	if err := os.Rename(missing, hidden); err != nil {
		t.Fatal(err)
	}
	m := New(be, "x", board.DefaultSpec()).WithHooks(r) // New's own first load saw csv2 failing
	m.loaded, m.partial = false, nil
	m.reload() // partial first load
	if err := os.Rename(hidden, missing); err != nil {
		t.Fatal(err)
	}
	m.reload()
	if got := events(); len(got) != 0 {
		t.Fatalf("fired %v", got)
	}
}

// The board's undo is told apart from a normal edit by the event's source.
func TestUndoHasItsOwnHookSource(t *testing.T) {
	t.Setenv(hook.DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "events.txt")
	s := hook.Spec{On: []string{hook.TaskMoved}}
	s.Name = "log"
	s.Command = []string{"sh", "-c", `echo "$0" >> ` + out, "{event}:{source}:{id}"}
	if err := hook.ValidateAll([]hook.Spec{s}); err != nil {
		t.Fatal(err)
	}
	runner := hook.New([]hook.Spec{s}, dir, "tui", nil, nil)
	m, be := newModel(t)
	be.SetWriteListener(runner.OnWrite, runner.NeedsOld())
	m = m.WithHooks(runner)
	m = press(m, "L", "u") // move #1, then undo it
	runner.Wait()
	b, _ := os.ReadFile(out)
	got := strings.Fields(string(b))
	if len(got) != 2 || got[0] != "task.moved:tui:1" || got[1] != "task.moved:undo:1" {
		t.Fatalf("events %v", got)
	}
}

// What the board writes itself was announced when it was written: the reload
// that follows must not report the same change again as appeared / disappeared.
func TestOwnWritesAreNotReportedByTheReload(t *testing.T) {
	r, events := hookLog(t, hook.TaskAppeared, hook.TaskDisappeared, hook.TaskCreateSuccess)
	m, be := newModel(t)
	be.SetWriteListener(r.OnWrite, r.NeedsOld())
	m = m.WithHooks(r)
	m = press(m, "n", "N", "e", "w", "enter") // create (reloads)
	m = press(m, "d", "y")                    // delete the selected card (reloads)
	got := events()
	if len(got) != 1 || !strings.HasPrefix(got[0], "task.create.success:csv1:") {
		t.Fatalf("events %v", got)
	}
	// An outside change in the same file is still seen.
	if err := be.Delete(context.Background(), "csv1", "2"); err != nil { // our own, via the Set: not news either
		t.Fatal(err)
	}
	m.reload()
	if got := events(); len(got) != 1 {
		t.Fatalf("events %v", got)
	}
}
