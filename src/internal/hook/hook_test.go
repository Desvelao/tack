package hook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/task"
)

type mem struct{ tasks []task.Task }

func (m *mem) List(context.Context) ([]task.Task, error) {
	return append([]task.Task(nil), m.tasks...), nil
}
func (m *mem) Statuses() []string { return nil }
func (m *mem) Update(_ context.Context, t task.Task) error {
	for i := range m.tasks {
		if m.tasks[i].ID == t.ID {
			m.tasks[i] = t
			return nil
		}
	}
	return backend.ErrNotFound
}
func (m *mem) Create(_ context.Context, t task.Task) (task.Task, error) {
	t.ID = "n1"
	m.tasks = append(m.tasks, t)
	return t, nil
}
func (m *mem) Delete(_ context.Context, id string) error {
	for i := range m.tasks {
		if m.tasks[i].ID == id {
			m.tasks = append(m.tasks[:i], m.tasks[i+1:]...)
			return nil
		}
	}
	return backend.ErrNotFound
}
func (m *mem) Close() error { return nil }

// rec collects audit entries; hooks record from their own goroutines.
type rec struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (r *rec) Record(e audit.Entry) {
	r.mu.Lock()
	r.entries = append(r.entries, e)
	r.mu.Unlock()
}

// setup builds a Runner whose hooks append "event:id:old_status:source" to a file.
func setup(t *testing.T, specs ...Spec) (*Runner, *backend.Set, func() []string, *rec) {
	t.Helper()
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	for i := range specs {
		specs[i].Command = []string{"sh", "-c", `echo "$0" >> ` + out, "{event}:{id}:{old_status}:{source}"}
	}
	if err := ValidateAll(specs); err != nil {
		t.Fatal(err)
	}
	r := &rec{}
	run := New(specs, dir, "cli", (*actionlog.Log)(nil), r)
	set := backend.NewSet([]backend.Named{{Name: "m", Type: "mem", Backend: &mem{tasks: []task.Task{{ID: "1", Title: "a", Status: "todo", Priority: "high"}, {ID: "2", Title: "b", Status: "todo"}}}}})
	set.SetWriteListener(run.OnWrite, run.NeedsOld())
	lines := func() []string {
		run.Wait()
		b, _ := os.ReadFile(out)
		l := strings.Fields(string(b))
		sort.Strings(l)
		return l
	}
	return run, set, lines, r
}

func spec(name, when string, on ...string) Spec {
	s := Spec{On: on, When: when}
	s.Name = name
	return s
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]Spec{
		"unknown event":   spec("a", "", "task.creat"),
		"no events":       spec("a", ""),
		"bad when":        spec("a", `"unterminated`, "task.moved"),
		"typo of a field": spec("a", "priorty=high", "task.moved"),
		"when+reload":     spec("a", "priority=high", "reload.fail"),
		"derived field":   spec("a", "ready=yes", "task.moved"),
		"running field":   spec("a", "running=yes", "task.moved"),
		"dedupe on write": func() Spec { s := spec("a", "", "task.moved"); s.Dedupe = true; return s }(),
		"notify":          func() Spec { s := spec("a", "", "task.moved"); s.Notify = "bell"; return s }(),
		"claim":           func() Spec { s := spec("a", "", "task.moved"); s.Claim = true; return s }(),
	}
	for name, s := range bad {
		s.Command = []string{"true"}
		if err := ValidateAll([]Spec{s}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	inter := spec("a", "", "task.moved")
	inter.Command, inter.Mode = []string{"true"}, "interactive"
	if ValidateAll([]Spec{inter}) == nil {
		t.Error("interactive accepted")
	}
	a, b := spec("a", "", "task.moved"), spec("A", "", "task.moved")
	a.Command, b.Command = []string{"true"}, []string{"true"}
	if ValidateAll([]Spec{a, b}) == nil {
		t.Error("duplicate accepted")
	}
	ph := spec("a", "", "task.moved")
	ph.Command = []string{"true", "{event}", "{old_status}", "{title}"}
	if err := ValidateAll([]Spec{ph}); err != nil {
		t.Error(err)
	}
	ph.Command = []string{"true", "{nope}"}
	if ValidateAll([]Spec{ph}) == nil {
		t.Error("unknown placeholder accepted")
	}
}

func TestWriteEvents(t *testing.T) {
	_, set, lines, _ := setup(t,
		spec("c", "", "task.create.success"),
		spec("u", "", "task.update"),
		spec("d", "", "task.delete"),
		spec("m", "", "task.moved"),
	)
	ctx := context.Background()
	if _, err := set.Create(ctx, task.Task{Title: "x", Status: "todo"}); err != nil {
		t.Fatal(err)
	}
	if err := set.Update(ctx, task.Task{ID: "1", Title: "a", Status: "done", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := set.Update(ctx, task.Task{ID: "2", Title: "renamed", Status: "todo", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := set.Delete(ctx, "m", "2"); err != nil {
		t.Fatal(err)
	}
	eq(t, lines(),
		"task.create.success:n1::cli",
		"task.update.success:1:todo:cli", "task.update.success:2:todo:cli",
		"task.moved:1:todo:cli",
		"task.delete.success:2:todo:cli")
}

func TestFailEventsAndWhen(t *testing.T) {
	_, set, lines, _ := setup(t,
		spec("f", "", "task.update.fail"),
		spec("h", "priority=high", "task.update.success"),
	)
	ctx := context.Background()
	if err := set.Update(ctx, task.Task{ID: "zz", Title: "z", Backend: "m"}); !errors.Is(err, backend.ErrNotFound) {
		t.Fatal(err)
	}
	if err := set.Update(ctx, task.Task{ID: "1", Title: "a", Status: "x", Priority: "high", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := set.Update(ctx, task.Task{ID: "2", Title: "b", Status: "x", Priority: "low", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	eq(t, lines(), "task.update.fail:zz::cli", "task.update.success:1:todo:cli")
}

func TestOldFromContextSkipsLookup(t *testing.T) {
	_, set, lines, _ := setup(t, spec("m", "", "task.moved"))
	ctx := backend.WithOld(context.Background(), task.Task{ID: "1", Status: "was"})
	if err := set.Update(ctx, task.Task{ID: "1", Title: "a", Status: "now", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	eq(t, lines(), "task.moved:1:was:cli")
}

func TestDiff(t *testing.T) {
	run, set, lines, _ := setup(t, spec("a", "", "task.appeared"), spec("g", "", "task.disappeared"))
	a := task.Task{ID: "1", Backend: "m"}
	b := task.Task{ID: "2", Backend: "m"}
	c := task.Task{ID: "3", Backend: "m"}
	d := task.Task{ID: "4", Backend: "other"}
	run.Diff([]task.Task{a, b, d}, []task.Task{b, c}, nil, map[string]bool{"other": true})
	eq(t, lines(), "task.appeared:3::poll", "task.disappeared:1::poll")

	// own writes are not news
	if _, err := set.Create(context.Background(), task.Task{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	own := set.TakeWritten()
	if !own[backend.Key("m", "n1")] || len(set.TakeWritten()) != 0 {
		t.Fatalf("own %v", own)
	}
}

func TestDiffOwnWritesAndReusedIDs(t *testing.T) {
	run, _, lines, _ := setup(t, spec("a", "", "task.appeared"), spec("g", "", "task.disappeared"))
	a := task.Task{ID: "1", Title: "a", Backend: "m"}
	b := task.Task{ID: "2", Title: "b", Backend: "m"}

	// an own update, then an outside delete in the same window: still gone
	run.Diff([]task.Task{a, b}, []task.Task{b}, map[string]bool{backend.Key("m", "1"): true}, nil)
	eq(t, lines(), "task.disappeared:1::poll")

	// an own delete, then an outside re-create: it appeared
	run.Diff([]task.Task{b}, []task.Task{a, b}, map[string]bool{backend.Key("m", "1"): false}, nil)
	eq(t, lines(), "task.disappeared:1::poll", "task.appeared:1::poll")

	// own writes that match the list stay quiet
	run.Diff([]task.Task{b}, []task.Task{a}, map[string]bool{backend.Key("m", "1"): true, backend.Key("m", "2"): false}, nil)
	eq(t, lines(), "task.disappeared:1::poll", "task.appeared:1::poll")
}

func TestDiffFingerprint(t *testing.T) {
	run, _, lines, _ := setup(t, spec("a", "", "task.appeared"), spec("g", "", "task.disappeared"))
	a := task.Task{ID: "1", Title: "a", Status: "x", Backend: "m"}

	moved := a
	moved.Status = "y"
	run.Diff([]task.Task{a}, []task.Task{moved}, nil, nil)
	eq(t, lines())

	reused := a
	reused.Title = "other"
	run.Diff([]task.Task{a}, []task.Task{reused}, nil, nil)
	eq(t, lines(), "task.disappeared:1:x:poll", "task.appeared:1::poll")

	// same title, another created date: a new task too
	c1, c2 := a, a
	c1.Extra, c2.Extra = map[string]string{"created": "2026-01-01"}, map[string]string{"created": "2026-02-01"}
	run.Diff([]task.Task{c1}, []task.Task{c2}, nil, nil)
	eq(t, lines(), "task.disappeared:1:x:poll", "task.appeared:1::poll", "task.disappeared:1:x:poll", "task.appeared:1::poll")

	// an own title edit is not a reuse
	run.Diff([]task.Task{a}, []task.Task{reused}, map[string]bool{backend.Key("m", "1"): true}, nil)
	eq(t, lines(), "task.disappeared:1:x:poll", "task.appeared:1::poll", "task.disappeared:1:x:poll", "task.appeared:1::poll")
}

func TestDedupe(t *testing.T) {
	dd := spec("a", "", "task.appeared")
	dd.Dedupe = true
	run, _, lines, r := setup(t, dd, spec("plain", "", "task.disappeared"))
	other := New(append([]Spec(nil), run.specs...), run.baseDir, "tui", nil, r)
	a := task.Task{ID: "1", Title: "a", Backend: "m"}

	// two boards notice the same change: the deduping hook runs once, the other twice
	for _, x := range []*Runner{run, other} {
		x.Diff([]task.Task{a}, []task.Task{}, nil, nil)
		x.Diff([]task.Task{}, []task.Task{a}, nil, nil)
	}
	other.Wait()
	eq(t, lines(), "task.appeared:1::poll", "task.disappeared:1::poll", "task.disappeared:1::poll")
	if !slices.ContainsFunc(r.entries, func(e audit.Entry) bool { return e.Details == "a: skipped (duplicate)" }) {
		t.Fatalf("duplicate not audited: %+v", r.entries)
	}

	// an expired marker is pruned and the event fires again
	dir := filepath.Join(run.baseDir, DedupeDir)
	old := time.Now().Add(-2 * dedupeTTL)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("markers %v %v", entries, err)
	}
	if err := os.Chtimes(filepath.Join(dir, entries[0].Name()), old, old); err != nil {
		t.Fatal(err)
	}
	run.pruned = time.Time{}
	run.Diff([]task.Task{}, []task.Task{a}, nil, nil)
	eq(t, lines(), "task.appeared:1::poll", "task.appeared:1::poll", "task.disappeared:1::poll", "task.disappeared:1::poll")
}

// An event this process drops (a limit) must not keep the marker, or no other process could run it.
func TestDedupeMarkerIsRemovedWhenTheEventIsDropped(t *testing.T) {
	dd := spec("a", "", "task.appeared")
	dd.Dedupe, dd.Unique = true, UniqueTask
	run, _, lines, r := setup(t, dd)
	other := New(append([]Spec(nil), run.specs...), run.baseDir, "tui", nil, r)
	a := task.Task{ID: "1", Title: "a", Backend: "m"}

	run.inflight[inflightKey{&run.specs[0], backend.Key("m", "1")}] = 1 // already running here
	run.Diff([]task.Task{}, []task.Task{a}, nil, nil)
	eq(t, lines())
	if entries, _ := os.ReadDir(filepath.Join(run.baseDir, DedupeDir)); len(entries) != 0 {
		t.Fatalf("marker of a dropped event kept: %v", entries)
	}
	other.Diff([]task.Task{}, []task.Task{a}, nil, nil)
	other.Wait()
	eq(t, lines(), "task.appeared:1::poll")
}

func TestReloadAndActionEvents(t *testing.T) {
	run, _, lines, _ := setup(t,
		spec("r", "", "reload.fail"),
		spec("s", "", "action.succeeded"),
		spec("f", "", "action.finished"),
	)
	run.Fire(Event{Name: ReloadFail, Source: "tui", Err: "boom"})
	run.Fire(Event{Name: ReloadOK, Source: "tui"})
	task1 := task.Task{ID: "7", Backend: "m"}
	run.FireAction(task1, "deploy", "cli", &actionlog.Record{Result: actionlog.ResultOK})
	run.FireAction(task1, "deploy", "cli", &actionlog.Record{Result: actionlog.ResultError, Error: "x"})
	run.FireAction(task1, "deploy", "cli", nil)
	eq(t, lines(), "reload.fail:::tui", "action.succeeded:7::cli", "action.succeeded:7::cli", "action.failed:7::cli")
}

func TestDepthGuardAndFailureDoesNotFailWrite(t *testing.T) {
	_, set, _, r := setup(t, spec("x", "", "task.update"))
	t.Setenv(DepthEnv, "1")
	spec := spec("bad", "", "task.update")
	spec.Command = []string{"sh", "-c", "exit 3"}
	if err := ValidateAll([]Spec{spec}); err != nil {
		t.Fatal(err)
	}
	// Inside a hook: nothing fires.
	if New([]Spec{spec}, t.TempDir(), "cli", nil, r).Active() {
		t.Fatal("active at max depth")
	}
	// A failing command is recorded, the write succeeds.
	t.Setenv(DepthEnv, "")
	run := New([]Spec{spec}, t.TempDir(), "cli", nil, r)
	set.SetWriteListener(run.OnWrite, false)
	if err := set.Update(context.Background(), task.Task{ID: "1", Title: "a", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	run.Wait()
	if len(r.entries) == 0 || r.entries[len(r.entries)-1].Result != audit.ResultError || r.entries[len(r.entries)-1].Op != audit.HookOp(TaskUpdateSuccess) {
		t.Fatalf("%+v", r.entries)
	}
}

func TestCancelKillsRunningHooksAndTheyAreRecorded(t *testing.T) {
	t.Setenv(DepthEnv, "")
	s := spec("slow", "", "task.moved")
	s.Command = []string{"sh", "-c", "sleep 60 & wait"}
	s.Timeout = "5m"
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	r := &rec{}
	run := New([]Spec{s}, t.TempDir(), "cli", nil, r)
	run.Fire(Event{Name: TaskMoved, New: task.Task{ID: "1", Backend: "m"}})
	if run.Running() != 1 {
		t.Fatalf("running %d", run.Running())
	}
	start := time.Now()
	run.Cancel()
	run.Wait()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Cancel took %s", d)
	}
	if run.Running() != 0 {
		t.Fatalf("still running: %d", run.Running())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) != 1 || r.entries[0].Result != audit.ResultError || r.entries[0].Op != audit.HookOp(TaskMoved) {
		t.Fatalf("%+v", r.entries)
	}
}

// A card with agent logs in its notes must not stop a hook from starting.
func TestHugeOldTaskDoesNotBreakTheLaunch(t *testing.T) {
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	s := spec("env", "", "task.moved")
	s.Command = []string{"sh", "-c", `echo "${TACK_OLD_TASK_JSON_OMITTED:-has-json}|${TACK_TASK_JSON_OMITTED:-has-json}" >> ` + out}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	run := New([]Spec{s}, dir, "cli", nil, &rec{})
	big := task.Task{ID: "1", Title: "t", Status: "todo", Backend: "m", Notes: strings.Repeat("agent log line\n", 20<<10)}
	now := big
	now.Status = "doing"
	run.Fire(Event{Name: TaskMoved, Old: big, HasOld: true, New: now})
	run.Wait()
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}
	if got := strings.TrimSpace(string(b)); !strings.Contains(got, "bytes, too large") || strings.Contains(got, "has-json") {
		t.Fatalf("both JSON variables should be omitted: %q", got)
	}
}

// Extra columns of a csv or frontmatter keys are not known when the config is
// read, so `when:` accepts them and matches the tasks that carry them.
func TestWhenCanUseExtraFields(t *testing.T) {
	run, _, lines, _ := setup(t, spec("acme", "customer:acme", "task.moved"), spec("sized", "story_points>=5", "task.moved"))
	acme := task.Task{ID: "1", Title: "a", Backend: "m", Extra: map[string]string{"customer": "ACME Corp", "story_points": "8"}}
	other := task.Task{ID: "2", Title: "b", Backend: "m", Extra: map[string]string{"customer": "globex", "story_points": "3"}}
	plain := task.Task{ID: "3", Title: "c", Backend: "m"}
	for _, tk := range []task.Task{acme, other, plain} {
		run.Fire(Event{Name: TaskMoved, New: tk, Old: tk, HasOld: true})
	}
	eq(t, lines(), "task.moved:1::cli", "task.moved:1::cli")
}

// A title-only edit is an update but not a move.
func TestUpdateWithoutAStatusChangeIsNotMoved(t *testing.T) {
	_, set, lines, _ := setup(t, spec("u", "", "task.update.success"), spec("m", "", "task.moved"))
	if err := set.Update(context.Background(), task.Task{ID: "1", Title: "renamed", Status: "todo", Priority: "high", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	eq(t, lines(), "task.update.success:1:todo:cli")
}

func TestStdinEventValidation(t *testing.T) {
	for _, in := range []string{"event", "event-markdown", "json", "markdown", "none"} {
		s := spec("a", "", "task.moved")
		s.Command, s.Stdin = []string{"true"}, in
		if err := ValidateAll([]Spec{s}); err != nil {
			t.Errorf("stdin %s: %v", in, err)
		}
	}
	s := spec("a", "", "task.moved")
	s.Command, s.Stdin = []string{"true"}, "xml"
	if ValidateAll([]Spec{s}) == nil {
		t.Error("stdin xml accepted")
	}
	s.Stdin, s.Mode = "event", "interactive"
	if ValidateAll([]Spec{s}) == nil {
		t.Error("interactive with stdin event accepted")
	}
}

// stdinOf runs a hook with the given stdin choice on the events fired by fn and returns what it read.
func stdinOf(t *testing.T, stdin string, fn func(*Runner, *backend.Set)) []string {
	t.Helper()
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	s := spec("e", "", "task.create.success", "task.update", "task.moved", "task.delete", "task.delete.fail", "reload.fail", "action.failed")
	s.Command, s.Stdin = []string{"sh", "-c", `cat > ` + filepath.Join(dir, "out.$$")}, stdin
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	run := New([]Spec{s}, dir, "cli", (*actionlog.Log)(nil), &rec{})
	set := backend.NewSet([]backend.Named{{Name: "m", Type: "mem", Backend: &mem{tasks: []task.Task{{ID: "1", Title: "a", Status: "todo"}}}}})
	set.SetWriteListener(run.OnWrite, run.NeedsOld())
	fn(run, set)
	run.Wait()
	files, _ := filepath.Glob(filepath.Join(dir, "out.*"))
	var out []string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out = append(out, string(b))
	}
	return out
}

func TestStdinEventJSON(t *testing.T) {
	ctx := context.Background()
	got := stdinOf(t, "event", func(run *Runner, set *backend.Set) {
		if err := set.Update(ctx, task.Task{ID: "1", Title: "a", Status: "done", Backend: "m"}); err != nil {
			t.Fatal(err)
		}
	})
	// task.moved and task.update.success
	if len(got) != 2 {
		t.Fatalf("runs = %q", got)
	}
	for _, g := range got {
		var d map[string]any
		if err := json.Unmarshal([]byte(g), &d); err != nil || !strings.HasSuffix(g, "\n") || strings.Count(g, "\n") != 1 {
			t.Fatalf("not one JSON line: %q (%v)", g, err)
		}
		if d["source"] != "cli" || d["task"].(map[string]any)["status"] != "done" || d["old"].(map[string]any)["status"] != "todo" {
			t.Errorf("payload = %s", g)
		}
		if _, ok := d["error"]; ok {
			t.Errorf("empty error present: %s", g)
		}
		if _, ok := d["action"]; ok {
			t.Errorf("empty action present: %s", g)
		}
	}

	// create: no old task; reload.fail: error; action event: action name.
	got = stdinOf(t, "event", func(run *Runner, set *backend.Set) {
		if _, err := set.Create(ctx, task.Task{Title: "x", Status: "todo"}); err != nil {
			t.Fatal(err)
		}
	})
	if len(got) != 1 || strings.Contains(got[0], `"old"`) || !strings.Contains(got[0], `"event":"task.create.success"`) || !strings.Contains(got[0], `"title":"x"`) {
		t.Errorf("create = %q", got)
	}
	got = stdinOf(t, "event", func(run *Runner, set *backend.Set) {
		run.Fire(Event{Name: ReloadFail, Source: "tui", Err: "boom"})
	})
	if len(got) != 1 || !strings.Contains(got[0], `"error":"boom"`) || !strings.Contains(got[0], `"event":"reload.fail"`) || strings.Contains(got[0], `"old"`) {
		t.Errorf("reload.fail = %q", got)
	}
	got = stdinOf(t, "event", func(run *Runner, set *backend.Set) {
		run.FireAction(task.Task{ID: "7", Backend: "m"}, "deploy", "cli", &actionlog.Record{Result: actionlog.ResultError, Error: "x"})
	})
	if len(got) != 1 || !strings.Contains(got[0], `"action":"deploy"`) || !strings.Contains(got[0], `"event":"action.failed"`) {
		t.Errorf("action = %q", got)
	}
	got = stdinOf(t, "event", func(run *Runner, set *backend.Set) {
		if err := set.Delete(ctx, "m", "1"); err != nil {
			t.Fatal(err)
		}
	})
	if len(got) != 1 || !strings.Contains(got[0], `"event":"task.delete.success"`) || !strings.Contains(got[0], `"old":{`) || !strings.Contains(got[0], `"task":{`) {
		t.Errorf("delete = %q", got)
	}
}

func TestStdinEventMarkdown(t *testing.T) {
	ctx := context.Background()
	got := stdinOf(t, "event-markdown", func(run *Runner, set *backend.Set) {
		if err := set.Update(ctx, task.Task{ID: "1", Title: "a", Status: "done", Backend: "m", Description: "do it"}); err != nil {
			t.Fatal(err)
		}
	})
	if len(got) != 2 {
		t.Fatalf("runs = %q", got)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "- event: task.") || !strings.Contains(g, "- source: cli\n- old status: todo\n\n# a\n\n") || !strings.Contains(g, "## Description\n\ndo it") || strings.Contains(g, "- error:") || strings.Contains(g, "- action:") {
			t.Errorf("markdown = %q", g)
		}
	}
	got = stdinOf(t, "event-markdown", func(run *Runner, set *backend.Set) {
		run.Fire(Event{Name: ReloadFail, Source: "tui", Err: "boom"})
		run.FireAction(task.Task{ID: "7", Title: "t", Backend: "m"}, "deploy", "cli", &actionlog.Record{Result: actionlog.ResultError, Error: "x"})
	})
	joined := strings.Join(got, "")
	if len(got) != 2 || !strings.Contains(joined, "- error: boom\n") || !strings.Contains(joined, "- action: deploy\n") || strings.Contains(joined, "old status") {
		t.Errorf("markdown = %q", got)
	}
}

// fire runs one task.update through a runner built with the given depth and
// chain env, limits and hooks, and returns what the hooks logged.
func fireWith(t *testing.T, depth, chain string, max int, specs ...Spec) []string {
	t.Helper()
	run, set, lines, _ := setup(t, specs...)
	t.Setenv(DepthEnv, depth)
	t.Setenv(ChainEnv, chain)
	run2 := New(run.specs, run.baseDir, "cli", nil, nil)
	run2.SetMaxDepth(max)
	set.SetWriteListener(run2.OnWrite, false)
	if err := set.Update(context.Background(), task.Task{ID: "1", Title: "a", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	run2.Wait()
	return lines()
}

func TestMaxDepthLimits(t *testing.T) {
	one := func() Spec { return spec("a", "", "task.update") }
	eq(t, fireWith(t, "1", "", 1, one()))                                    // default: inert
	eq(t, fireWith(t, "1", "other", 2, one()), "task.update.success:1::cli") // depth 2 level fires
	eq(t, fireWith(t, "2", "other", 2, one()))                               // past the limit
	eq(t, fireWith(t, "2", "other", 3, one()), "task.update.success:1::cli")
	eq(t, fireWith(t, "3", "other", 3, one()))
	// A per-hook limit can only lower the global one.
	low := one()
	low.MaxDepth = 1
	eq(t, fireWith(t, "1", "other", 3, low))
	high := one()
	high.MaxDepth = 3
	eq(t, fireWith(t, "1", "other", 2, high), "task.update.success:1::cli")
	eq(t, fireWith(t, "2", "other", 2, high))
}

func TestChainPreventsSelfRefire(t *testing.T) {
	a, b := spec("a", "", "task.update"), spec("b", "", "task.update")
	got := fireWith(t, "1", "a", 3, a, b) // only b is outside the chain
	eq(t, got, "task.update.success:1::cli")
	eq(t, fireWith(t, "2", "a>b", 3, a, b))
	t.Setenv(DepthEnv, "")
	t.Setenv(ChainEnv, "")
	if r := New([]Spec{a}, t.TempDir(), "cli", nil, nil); !r.Active() {
		t.Fatal("not active at depth 0")
	}
	t.Setenv(DepthEnv, "1")
	t.Setenv(ChainEnv, "A") // names compare case-insensitively
	r := New([]Spec{a}, t.TempDir(), "cli", nil, nil)
	r.SetMaxDepth(3)
	if r.Active() || r.NeedsOld() {
		t.Fatal("a hook in the chain is active")
	}
}

func TestChildGetsDepthAndChain(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	s := spec("b", "", "task.update")
	s.Command = []string{"sh", "-c", `echo "$TACK_HOOK_DEPTH $TACK_HOOK_CHAIN" > ` + out}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DepthEnv, "1")
	t.Setenv(ChainEnv, "a")
	r := New([]Spec{s}, dir, "cli", nil, nil)
	r.SetMaxDepth(2)
	r.Fire(Event{Name: TaskUpdateSuccess, New: task.Task{ID: "1"}})
	r.Wait()
	b, _ := os.ReadFile(out)
	if strings.TrimSpace(string(b)) != "2 a>b" {
		t.Fatalf("env %q", b)
	}
}

func TestMaxDepthRange(t *testing.T) {
	for _, n := range []int{-1, HardMaxDepth + 1} {
		s := spec("a", "", "task.update")
		s.Command = []string{"x"}
		s.MaxDepth = n
		if err := ValidateAll([]Spec{s}); err == nil {
			t.Errorf("max_depth %d accepted", n)
		}
	}
}

// slowRunner builds a Runner whose hook appends its event id to a file,
// then sleeps; ids() lists the ids started so far.
func slowRunner(t *testing.T, s Spec, sleep string) (*Runner, *rec, func() []string) {
	t.Helper()
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	s.Command = []string{"sh", "-c", `echo "$0" >> ` + out + `; sleep ` + sleep, "{id}"}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	r := &rec{}
	run := New([]Spec{s}, dir, "cli", nil, r)
	ids := func() []string {
		b, _ := os.ReadFile(out)
		return strings.Fields(string(b))
	}
	return run, r, ids
}

func sorted(s []string) []string { s = append([]string(nil), s...); sort.Strings(s); return s }

func fireIDs(run *Runner, name string, ids ...string) {
	for _, id := range ids {
		run.Fire(Event{Name: name, New: task.Task{ID: id, Backend: "m"}})
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestQueueDropSkipsWhileRunning(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.MaxParallel = 1
	run, r, ids := slowRunner(t, s, "0.3")
	fireIDs(run, TaskMoved, "1", "2", "3")
	run.Wait()
	eq(t, sorted(ids()), "1")
	if run.Dropped("slow") != 2 {
		t.Fatalf("dropped %d", run.Dropped("slow"))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	skipped := 0
	for _, e := range r.entries {
		if strings.Contains(e.Details, "slow: skipped (running)") && e.Result == audit.ResultSkipped {
			skipped++
		}
	}
	if skipped != 2 {
		t.Fatalf("%+v", r.entries)
	}
}

func skippedWith(r *rec, reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.entries {
		if strings.Contains(e.Details, "skipped ("+reason+")") && e.Result == audit.ResultSkipped {
			n++
		}
	}
	return n
}

func TestUniqueTaskSkipsSameTaskOnly(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Unique = UniqueTask
	run, r, ids := slowRunner(t, s, "0.3")
	fireIDs(run, TaskMoved, "1", "1", "2", "1")
	run.Wait()
	eq(t, sorted(ids()), "1", "2")
	if n := skippedWith(r, "already running"); n != 2 {
		t.Fatalf("skipped %d: %+v", n, r.entries)
	}
	// the first run ended: the task may run again
	fireIDs(run, TaskMoved, "1")
	run.Wait()
	eq(t, sorted(ids()), "1", "1", "2")
}

func TestUniqueTaskDropsQueuedDuplicate(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Unique, s.MaxParallel, s.Queue = UniqueTask, 1, QueueAll
	run, r, ids := slowRunner(t, s, "0.2")
	fireIDs(run, TaskMoved, "1", "2")
	run.Wait()
	eq(t, sorted(ids()), "1", "2")
	if n := skippedWith(r, "running"); n != 0 {
		t.Fatalf("queued event is not skipped: %+v", r.entries)
	}
}

func TestGlobalMaxParallelCapsAllHooks(t *testing.T) {
	a, b := spec("a", "", "task.moved"), spec("b", "", "task.moved")
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	for _, s := range []*Spec{&a, &b} {
		s.Command = []string{"sh", "-c", `echo "$0" >> ` + out + `; sleep 0.3`, s.Name + "{id}"}
	}
	specs := []Spec{a, b}
	if err := ValidateAll(specs); err != nil {
		t.Fatal(err)
	}
	r := &rec{}
	run := New(specs, dir, "cli", nil, r)
	run.SetMaxParallel(2)
	fireIDs(run, TaskMoved, "1", "2")
	if run.Running() != 2 {
		t.Fatalf("running %d, want 2 (global cap)", run.Running())
	}
	run.Wait()
	b2, _ := os.ReadFile(out)
	if got := len(strings.Fields(string(b2))); got != 2 {
		t.Fatalf("started %d, want 2: %s", got, b2)
	}
	if n := skippedWith(r, "global limit"); n != 2 {
		t.Fatalf("skipped %d: %+v", n, r.entries)
	}
}

func TestMaxParallelAllowsSeveral(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.MaxParallel = 2
	run, _, ids := slowRunner(t, s, "0.3")
	fireIDs(run, TaskMoved, "1", "2", "3")
	waitFor(t, "two starts", func() bool { return len(ids()) == 2 })
	if run.Running() != 2 {
		t.Fatalf("running %d", run.Running())
	}
	run.Wait()
	eq(t, sorted(ids()), "1", "2")
}

func TestNoLimitForTaskEventsByDefault(t *testing.T) {
	run, _, ids := slowRunner(t, spec("slow", "", "task.moved"), "0.2")
	fireIDs(run, TaskMoved, "1", "2", "3")
	run.Wait()
	eq(t, sorted(ids()), "1", "2", "3")
}

func TestReloadDefaultsToOneAtATime(t *testing.T) {
	run, r, ids := slowRunner(t, spec("slow", "", "reload.ok"), "0.3")
	fireIDs(run, ReloadOK, "1", "2", "3")
	run.Wait()
	eq(t, sorted(ids()), "1")
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) != 1 { // drops of reload.* are not audited
		t.Fatalf("%+v", r.entries)
	}
	s := spec("x", "", "reload.ok", "task.moved")
	s.Normalize()
	if s.parallel() != 0 {
		t.Fatal("mixed events should be unlimited")
	}
}

func TestQueueLatestKeepsNewest(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Queue, s.MaxParallel = "latest", 1
	run, _, ids := slowRunner(t, s, "0.3")
	fireIDs(run, TaskMoved, "1", "2", "3", "4")
	if run.Running() != 2 { // one running, one queued
		t.Fatalf("running %d", run.Running())
	}
	run.Wait()
	eq(t, sorted(ids()), "1", "4")
}

func TestQueueAllRunsEveryEventInOrder(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Queue, s.MaxParallel = "all", 1
	run, _, ids := slowRunner(t, s, "0.1")
	fireIDs(run, TaskMoved, "1", "2", "3")
	run.Wait()
	if got := strings.Join(ids(), ","); got != "1,2,3" {
		t.Fatalf("got %s", got)
	}
}

func TestQueueAllOverflowIsDroppedAndCounted(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Queue, s.MaxParallel = "all", 1
	run, r, _ := slowRunner(t, s, "30")
	for i := 0; i < MaxQueue+1+5; i++ {
		fireIDs(run, TaskMoved, "x")
	}
	if run.Dropped("slow") != 5 || run.Running() != 1+MaxQueue {
		t.Fatalf("dropped %d running %d", run.Dropped("slow"), run.Running())
	}
	r.mu.Lock()
	n := len(r.entries)
	r.mu.Unlock()
	if n != 5 {
		t.Fatalf("%d entries", n)
	}
	run.Cancel()
	run.Wait()
	if run.Running() != 0 {
		t.Fatalf("running %d", run.Running())
	}
}

func TestMinIntervalSpacesStarts(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.MinInterval = "300ms"
	run, _, ids := slowRunner(t, s, "0")
	fireIDs(run, TaskMoved, "1", "2")
	time.Sleep(100 * time.Millisecond)
	run.Wait()
	eq(t, sorted(ids()), "1")

	s.Queue = "latest"
	run, _, ids = slowRunner(t, s, "0")
	start := time.Now()
	fireIDs(run, TaskMoved, "1", "2", "3")
	run.Wait() // covers the queued event waiting for the interval
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("second start after %s", d)
	}
	eq(t, sorted(ids()), "1", "3")
}

func TestCancelDropsQueuedRuns(t *testing.T) {
	s := spec("slow", "", "task.moved")
	s.Queue, s.MaxParallel = "all", 1
	run, _, ids := slowRunner(t, s, "30")
	fireIDs(run, TaskMoved, "1", "2", "3")
	waitFor(t, "first start", func() bool { return len(ids()) == 1 })
	run.Cancel()
	done := make(chan struct{})
	go func() { run.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Cancel")
	}
	eq(t, sorted(ids()), "1")
	if run.Running() != 0 {
		t.Fatalf("running %d", run.Running())
	}
}

func TestLimitValidation(t *testing.T) {
	for name, mod := range map[string]func(*Spec){
		"negative max_parallel": func(s *Spec) { s.MaxParallel = -1 },
		"bad min_interval":      func(s *Spec) { s.MinInterval = "soon" },
		"negative interval":     func(s *Spec) { s.MinInterval = "-1s" },
		"bad queue":             func(s *Spec) { s.Queue = "fifo" },
	} {
		s := spec("a", "", "task.moved")
		s.Command = []string{"true"}
		mod(&s)
		if ValidateAll([]Spec{s}) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	s := spec("a", "", "task.moved")
	s.Command, s.MaxParallel, s.MinInterval, s.Queue = []string{"true"}, 3, "2s", " ALL "
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
}

func TestHookRunStoresTheEventInTheActionLog(t *testing.T) {
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	s := spec("notify", "", TaskMoved)
	s.Command = []string{"true"}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	l, err := actionlog.New(actionlog.Spec{File: "a.jsonl"}, dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	run := New([]Spec{s}, dir, "cli", l, &rec{})
	run.Fire(Event{Name: TaskMoved, New: task.Task{ID: "1", Backend: "m"}})
	run.Wait()
	recs, _ := l.Records(true)
	if len(recs) != 1 || recs[0].Event != TaskMoved || recs[0].Source != "hook" || recs[0].Action != "notify" {
		t.Fatalf("%+v", recs)
	}
}

// A backend's hook fires for its own tasks only and replaces the top-level
// hook of the same name there; reload.* events are refused in a backend's list.
func TestBackendHooks(t *testing.T) {
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	mk := func(name, label string) Spec {
		s := spec(name, "", TaskMoved)
		s.Command = []string{"sh", "-c", `echo "$0" >> ` + out, label + ":{backend}"}
		return s
	}
	top := []Spec{mk("a", "top-a"), mk("b", "top-b")}
	own := []Spec{mk("a", "m-a"), mk("c", "m-c")}
	if err := ValidateAll(top); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFor(own, "m"); err != nil {
		t.Fatal(err)
	}
	run := New(append(top, own...), dir, "cli", nil, nil)
	run.Fire(Event{Name: TaskMoved, New: task.Task{ID: "1", Backend: "m"}})
	run.Fire(Event{Name: TaskMoved, New: task.Task{ID: "1", Backend: "z"}})
	run.Wait()
	b, _ := os.ReadFile(out)
	got := strings.Fields(string(b))
	sort.Strings(got)
	eq(t, got, "m-a:m", "m-c:m", "top-b:m", "top-a:z", "top-b:z")

	reload := []Spec{spec("r", "", ReloadOK)}
	reload[0].Command = []string{"true"}
	if err := ValidateFor(reload, "m"); err == nil || !strings.Contains(err.Error(), "whole board") {
		t.Fatalf("reload.ok in a backend's hooks: %v", err)
	}
}

func columnSpec() board.Spec {
	return board.Spec{Unmapped: board.Append, Columns: []board.ColumnSpec{
		{Name: "Todo", Values: []string{"todo", "backlog"}},
		{Name: "Doing", Values: []string{"doing"}, Limit: 1},
		{Name: "Done", Values: []string{"done"}},
	}}
}

func TestEnteredAndLeftColumn(t *testing.T) {
	run, set, lines, _ := setup(t, spec("in", "", "task.entered.doing"), spec("out", "", "task.left.Todo"))
	run.SetBoard(columnSpec())
	ctx := backend.WithOld(context.Background(), task.Task{ID: "1", Status: "backlog"})
	if err := set.Update(ctx, task.Task{ID: "1", Title: "a", Status: "doing", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	eq(t, lines(), "task.entered.Doing:1:backlog:cli", "task.left.Todo:1:backlog:cli")

	// two statuses of one column: not a move between columns
	ctx = backend.WithOld(context.Background(), task.Task{ID: "2", Status: "todo"})
	if err := set.Update(ctx, task.Task{ID: "2", Title: "b", Status: "backlog", Backend: "m"}); err != nil {
		t.Fatal(err)
	}
	eq(t, lines(), "task.entered.Doing:1:backlog:cli", "task.left.Todo:1:backlog:cli")

	// a change seen on a reload
	a := task.Task{ID: "3", Title: "c", Status: "todo", Backend: "m"}
	b := a
	b.Status = "doing"
	run.Diff([]task.Task{a}, []task.Task{b}, nil, nil)
	eq(t, lines(), "task.entered.Doing:1:backlog:cli", "task.left.Todo:1:backlog:cli", "task.entered.Doing:3:todo:poll", "task.left.Todo:3:todo:poll")
}

func TestColumnPlaceholders(t *testing.T) {
	s := spec("p", "", "task.moved")
	s.Command = []string{"true", "{column}", "{old_column}", "{new_status}"}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	run := New([]Spec{s}, t.TempDir(), "cli", (*actionlog.Log)(nil), &rec{})
	run.SetBoard(columnSpec())
	_, p := run.plan(&run.specs[0], Event{Name: TaskMoved, HasOld: true, Old: task.Task{Status: "backlog"}, New: task.Task{Status: "doing"}}, task.Task{Status: "doing"})
	if got := strings.Join(p.Argv, " "); got != "true Doing Todo doing" {
		t.Fatalf("argv %q", got)
	}
}

func TestTickDueOncePerDay(t *testing.T) {
	run, _, lines, _ := setup(t, spec("d", "", "task.due"))
	run.SetBoard(columnSpec())
	tasks := []task.Task{
		{ID: "1", Backend: "m", Status: "todo", Due: "2026-10-18"},
		{ID: "2", Backend: "m", Status: "todo", Due: "2026-10-19"},
		{ID: "3", Backend: "m", Status: "todo", Due: "2026-10-20"},
		{ID: "4", Backend: "m", Status: "done", Due: "2026-10-01"},
		{ID: "5", Backend: "m", Status: "todo"},
	}
	day := time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local)
	run.Tick(tasks, day)
	run.Tick(tasks, day.Add(time.Hour))
	eq(t, lines(), "task.due:1::poll", "task.due:2::poll")
	run.Tick(tasks, day.Add(24*time.Hour))
	eq(t, lines(), "task.due:1::poll", "task.due:1::poll", "task.due:2::poll", "task.due:2::poll", "task.due:3::poll")
}

func TestTickWIPExceededOnTransition(t *testing.T) {
	run, _, lines, _ := setup(t, spec("w", "", "column.wip_exceeded"))
	run.SetBoard(columnSpec())
	one := []task.Task{{ID: "1", Backend: "m", Status: "doing"}}
	two := append(slices.Clone(one), task.Task{ID: "2", Backend: "m", Status: "doing"})
	now := time.Now()
	run.Tick(one, now)
	run.Tick(two, now)
	run.Tick(two, now)
	eq(t, lines(), "column.wip_exceeded:::poll")
	run.Tick(one, now)
	run.Tick(two, now)
	eq(t, lines(), "column.wip_exceeded:::poll", "column.wip_exceeded:::poll")
}

func TestColumnEventNamesValidate(t *testing.T) {
	s := spec("a", "", "task.entered.Review")
	s.Command = []string{"true"}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	w := spec("b", "priority=high", "column.wip_exceeded")
	w.Command = []string{"true"}
	if err := ValidateAll([]Spec{w}); err == nil {
		t.Error("when: accepted on an event without a task")
	}
}

func TestDiffMovedAndChanged(t *testing.T) {
	run, _, lines, _ := setup(t, spec("m", "", "task.moved"), spec("c", "", "task.changed"))
	a := task.Task{ID: "1", Title: "a", Status: "x", Backend: "m"}

	moved := a
	moved.Status = "y"
	run.Diff([]task.Task{a}, []task.Task{moved}, nil, nil)
	eq(t, lines(), "task.moved:1:x:poll", "task.changed:1:x:poll")

	labelled := a
	labelled.Labels = "bug"
	run.Diff([]task.Task{a}, []task.Task{labelled}, nil, nil)
	eq(t, lines(), "task.moved:1:x:poll", "task.changed:1:x:poll", "task.changed:1:x:poll")

	// a lease renewal, an own write and an incomplete backend are not news
	leased := a
	leased.Lease = "2030-01-01T00:00:00Z"
	run.Diff([]task.Task{a}, []task.Task{leased}, nil, nil)
	run.Diff([]task.Task{a}, []task.Task{labelled}, map[string]bool{backend.Key("m", "1"): true}, nil)
	run.Diff([]task.Task{a}, []task.Task{labelled}, nil, map[string]bool{"m": true})
	eq(t, lines(), "task.moved:1:x:poll", "task.changed:1:x:poll", "task.changed:1:x:poll")
}

// Cancel is final: what is fired afterwards does not run (see its doc comment).
func TestFiringAfterCancelDoesNotRun(t *testing.T) {
	s := spec("slow", "", "task.moved")
	run, r, _ := slowRunner(t, s, "0")
	run.Cancel()
	fireIDs(run, TaskMoved, "late")
	run.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.Result == audit.ResultOK {
			t.Errorf("a hook fired after Cancel ran fine: %+v", e)
		}
	}
}

// A hook run gets a run id like an action run: in its environment, in its
// record, and with the action log's path.
func TestHookRunHasARunID(t *testing.T) {
	t.Setenv(DepthEnv, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	s := spec("ids", "", "task.moved")
	s.Command = []string{"sh", "-c", `echo "$TACK_RUN_ID $TACK_ACTION_LOG" >> ` + out}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	alog, err := actionlog.New(actionlog.Spec{File: "actions.jsonl"}, dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	run := New([]Spec{s}, dir, "cli", alog, &rec{})
	rec1 := run.Test(&run.specs[0], Event{Name: TaskMoved, New: task.Task{ID: "1", Backend: "m"}})
	rec2 := run.Test(&run.specs[0], Event{Name: TaskMoved, New: task.Task{ID: "2", Backend: "m"}})
	if rec1.RunID == "" || rec2.RunID == "" || rec1.RunID == rec2.RunID {
		t.Fatalf("run ids %q %q", rec1.RunID, rec2.RunID)
	}
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || lines[0] != rec1.RunID+" "+alog.Path() {
		t.Errorf("environment: %q (want %q first)", lines, rec1.RunID+" "+alog.Path())
	}
}
