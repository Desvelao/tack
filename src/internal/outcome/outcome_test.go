package outcome

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/task"
)

// memBackend is an in-memory store that checks WithExpect like a real one. It
// is a Patcher so Set.Update leaves the guard to it; beforeWrite runs just
// before the guard, to play the part of a writer that got there first.
type memBackend struct {
	tasks       []task.Task
	beforeWrite func(*memBackend)
}

func (m *memBackend) List(context.Context) ([]task.Task, error) {
	return append([]task.Task(nil), m.tasks...), nil
}
func (m *memBackend) Statuses() []string { return nil }
func (m *memBackend) Update(ctx context.Context, t task.Task) error {
	return m.Patch(ctx, t, t)
}
func (m *memBackend) Patch(ctx context.Context, _, t task.Task) error {
	if m.beforeWrite != nil {
		m.beforeWrite(m)
	}
	for i, c := range m.tasks {
		if c.ID == t.ID {
			if err := backend.CheckExpect(ctx, c); err != nil {
				return err
			}
			m.tasks[i] = t
			return nil
		}
	}
	return fmt.Errorf("%w: %s", backend.ErrNotFound, t.ID)
}
func (m *memBackend) Create(context.Context, task.Task) (task.Task, error) {
	return task.Task{}, backend.ErrUnsupported
}
func (m *memBackend) Delete(context.Context, string) error { return backend.ErrUnsupported }
func (m *memBackend) Close() error                         { return nil }

func (m *memBackend) get(id string) task.Task {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return task.Task{}
}

func newFixture(t *testing.T) (*memBackend, *backend.Set, board.Board) {
	t.Helper()
	m := &memBackend{tasks: []task.Task{{ID: "1", Title: "One", Status: "in_progress", Notes: "old", Backend: "mem"}}}
	b, err := board.New(board.DefaultSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, backend.NewSet([]backend.Named{{Name: "mem", Type: "mem", Backend: m}}), b
}

func okRun(stdout string) actionlog.Record {
	zero := 0
	return actionlog.Record{RunID: "r1", Result: actionlog.ResultOK, Stdout: stdout, ExitCode: &zero}
}

func failedRun() actionlog.Record {
	one := 1
	return actionlog.Record{RunID: "r2", Result: actionlog.ResultError, Error: "exit status 1", ExitCode: &one}
}

func TestApplyMovesAndAppendsNotes(t *testing.T) {
	m, set, b := newFixture(t)
	s := action.Spec{Name: "x", Capture: action.CaptureNotes, OnSuccess: action.Outcome{Status: "done"}}
	started := m.get("1")
	after, err := Apply(context.Background(), set, b, s, started, okRun("hello"), false, "")
	if err != nil || after != "done" {
		t.Fatalf("after %q err %v", after, err)
	}
	if got := m.get("1"); got.Status != "done" || !strings.HasPrefix(got.Notes, "old\n\n[run r1 x ok exit 0]\nhello") {
		t.Errorf("%+v", got)
	}
}

func TestApplyDoesNothingWhenCanceledOrNotWanted(t *testing.T) {
	m, set, b := newFixture(t)
	s := action.Spec{Name: "x", OnSuccess: action.Outcome{Status: "done"}}
	if after, err := Apply(context.Background(), set, b, s, m.get("1"), okRun("o"), true, ""); after != "" || err != nil {
		t.Errorf("canceled: %q %v", after, err)
	}
	if after, err := Apply(context.Background(), set, b, action.Spec{Name: "x"}, m.get("1"), okRun("o"), false, ""); after != "" || err != nil {
		t.Errorf("not wanted: %q %v", after, err)
	}
	if got := m.get("1"); got.Status != "in_progress" || got.Notes != "old" {
		t.Errorf("changed: %+v", got)
	}
}

func TestApplyOnFailureAppendsTheError(t *testing.T) {
	m, set, b := newFixture(t)
	s := action.Spec{Name: "x", OnFailure: action.Outcome{Status: "failed", AppendNotes: action.NotesError}}
	after, err := Apply(context.Background(), set, b, s, m.get("1"), failedRun(), false, "")
	if err != nil || after != "failed" {
		t.Fatalf("after %q err %v", after, err)
	}
	if got := m.get("1"); got.Status != "failed" || !strings.Contains(got.Notes, "exit status 1") {
		t.Errorf("%+v", got)
	}
	// A successful run does not use on_failure.
	m2, set2, b2 := newFixture(t)
	if after, err := Apply(context.Background(), set2, b2, s, m2.get("1"), okRun("o"), false, ""); after != "" || err != nil || m2.get("1").Status != "in_progress" {
		t.Errorf("ok run: %q %v %+v", after, err, m2.get("1"))
	}
}

func TestApplyUnknownColumnStillAppendsNotes(t *testing.T) {
	m, set, b := newFixture(t)
	s := action.Spec{Name: "x", OnSuccess: action.Outcome{Status: "nowhere", AppendNotes: action.NotesStdout}}
	after, err := Apply(context.Background(), set, b, s, m.get("1"), okRun("kept"), false, "")
	if err == nil || !strings.Contains(err.Error(), `no status or column "nowhere"`) {
		t.Fatalf("err %v", err)
	}
	if after != "in_progress" {
		t.Errorf("after %q", after)
	}
	if got := m.get("1"); got.Status != "in_progress" || !strings.Contains(got.Notes, "kept") {
		t.Errorf("%+v", got)
	}
	// Without notes there is nothing to write: only the error.
	m2, set2, b2 := newFixture(t)
	s.OnSuccess.AppendNotes = ""
	if after, err := Apply(context.Background(), set2, b2, s, m2.get("1"), okRun("x"), false, ""); after != "" || err == nil {
		t.Errorf("%q %v", after, err)
	}
}

func TestApplyMissingTask(t *testing.T) {
	m, set, b := newFixture(t)
	s := action.Spec{Name: "x", OnSuccess: action.Outcome{Status: "done"}}
	gone := m.get("1")
	m.tasks = nil
	after, err := Apply(context.Background(), set, b, s, gone, okRun("o"), false, "")
	if err == nil || !errors.Is(err, backend.ErrNotFound) || after != "" {
		t.Errorf("after %q err %v", after, err)
	}
}

func TestApplyLeavesATaskMovedDuringTheRun(t *testing.T) {
	m, set, b := newFixture(t)
	started := m.get("1")
	m.tasks[0].Status = "failed" // an agent ran `tack report -status failed`
	s := action.Spec{Name: "x", OnSuccess: action.Outcome{Status: "done", AppendNotes: action.NotesStdout}}
	after, err := Apply(context.Background(), set, b, s, started, okRun("out"), false, "")
	if err != nil || after != "failed" {
		t.Fatalf("after %q err %v", after, err)
	}
	if got := m.get("1"); got.Status != "failed" || !strings.Contains(got.Notes, "out") {
		t.Errorf("%+v", got)
	}
}

func TestApplyKeepsNotesWhenMovedBetweenFindAndUpdate(t *testing.T) {
	m, set, b := newFixture(t)
	started := m.get("1")
	fired := false
	m.beforeWrite = func(m *memBackend) { // after Apply read the task, before its guarded write
		if !fired {
			fired = true
			m.tasks[0].Status = "failed"
		}
	}
	s := action.Spec{Name: "x", OnSuccess: action.Outcome{Status: "done", AppendNotes: action.NotesStdout}}
	after, err := Apply(context.Background(), set, b, s, started, okRun("out"), false, "")
	if err != nil || after != "failed" {
		t.Fatalf("after %q err %v", after, err)
	}
	if got := m.get("1"); got.Status != "failed" || !strings.Contains(got.Notes, "out") {
		t.Errorf("the move must lose, the notes must stay: %+v", got)
	}
	// Without notes the lost race is reported.
	m2, set2, b2 := newFixture(t)
	started = m2.get("1")
	fired = false
	m2.beforeWrite = func(m *memBackend) {
		if !fired {
			fired = true
			m.tasks[0].Status = "failed"
		}
	}
	s.OnSuccess.AppendNotes = ""
	if _, err := Apply(context.Background(), set2, b2, s, started, okRun("out"), false, ""); !errors.Is(err, backend.ErrPrecondition) {
		t.Errorf("err %v", err)
	}
}

func TestNotesBlockSources(t *testing.T) {
	code := 3
	rec := actionlog.Record{RunID: "r1", Result: actionlog.ResultError, Error: "exit status 3", Stdout: "out text", Stderr: "err text", ExitCode: &code}
	s := action.Spec{Name: "x"}
	for src, want := range map[string]string{
		action.NotesStdout:  "out text",
		action.NotesError:   "exit status 3",
		action.NotesSummary: "exit 3",
	} {
		if b := notesBlock(s, rec, src); !strings.HasPrefix(b, "[run r1 x error exit 3]") || !strings.Contains(b, want) {
			t.Errorf("%s: %q", src, b)
		}
	}
	rec.Error = ""
	if b := notesBlock(s, rec, action.NotesError); !strings.Contains(b, "err text") {
		t.Errorf("error falls back to stderr: %q", b)
	}
	s.CaptureMax = 1
	rec.Stdout = strings.Repeat("é", 2000)
	if b := notesBlock(s, rec, action.NotesStdout); !strings.HasSuffix(b, "[output cut]") || len(b) > 1200 {
		t.Errorf("cap: %d bytes", len(b))
	}
}

func TestAppendBlock(t *testing.T) {
	if got := AppendBlock("a\n", "b"); got != "a\n\nb" {
		t.Errorf("%q", got)
	}
	if got := AppendBlock(" ", "b"); got != "b" {
		t.Errorf("%q", got)
	}
}
