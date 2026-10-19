package hook

import (
	"slices"
	"strings"
	"testing"

	"tkan/internal/actionlog"
	"tkan/internal/task"
)

func TestTestEventDefaultsAndChecks(t *testing.T) {
	s := spec("n", "", "task.update", "task.delete.fail")
	s.Command = []string{"true"}
	if err := ValidateAll([]Spec{s}); err != nil {
		t.Fatal(err)
	}
	tk := task.Task{ID: "1", Status: "done"}
	ev, err := TestEvent(&s, "", tk, "todo")
	if err != nil || ev.Name != TaskUpdateSuccess || ev.Old.Status != "todo" || ev.New.Status != "done" || !ev.HasOld {
		t.Fatalf("default = %+v, %v", ev, err)
	}
	if ev, _ := TestEvent(&s, "task.delete.fail", tk, ""); ev.subject().ID != "1" || ev.Err == "" {
		t.Fatalf("delete.fail = %+v", ev)
	}
	for _, bad := range []string{"task.moved", "task.update", "task.mvoed"} {
		if _, err := TestEvent(&s, bad, tk, ""); err == nil {
			t.Fatalf("%s: no error", bad)
		}
	}
}

func TestWhenWarnings(t *testing.T) {
	specs := []Spec{spec("a", "priority=high", "task.delete"), spec("b", "priority=high", "task.moved"), spec("c", "", "task.delete.fail")}
	for i := range specs {
		specs[i].Command = []string{"true"}
	}
	if err := ValidateAll(specs); err != nil {
		t.Fatal(err)
	}
	if len(specs[0].WhenWarnings()) != 1 || specs[1].WhenWarnings() != nil || specs[2].WhenWarnings() != nil {
		t.Fatalf("warnings: %q %q %q", specs[0].WhenWarnings(), specs[1].WhenWarnings(), specs[2].WhenWarnings())
	}
}

func TestFindCheckPreviewAndTest(t *testing.T) {
	s := spec("n", "priority=high", "task.moved")
	s.Env = map[string]string{"TOKEN": "s3cret"}
	run, _, lines, r := setup(t, s)
	sp, ok := run.Find("N", "m")
	if !ok {
		t.Fatal("not found")
	}
	if _, ok := run.Find("other", "m"); ok {
		t.Fatal("found an unknown hook")
	}
	low := task.Task{ID: "2", Backend: "m", Status: "done"}
	ev, _ := TestEvent(sp, "", low, "todo")
	if why := run.Check(sp, ev); !strings.Contains(why, "when:") {
		t.Fatalf("check = %q", why)
	}
	high := task.Task{ID: "1", Backend: "m", Status: "done", Priority: "high"}
	ev, _ = TestEvent(sp, "", high, "todo")
	if why := run.Check(sp, ev); why != "" {
		t.Fatalf("check = %q", why)
	}
	p := run.Preview(sp, ev)
	if !slices.Contains(p.Env, "TOKEN="+Masked) || slices.ContainsFunc(p.Env, func(kv string) bool { return strings.Contains(kv, "s3cret") }) {
		t.Fatalf("env = %q", p.Env)
	}
	if !slices.Contains(p.Argv, "task.moved:1:todo:test") {
		t.Fatalf("argv = %q", p.Argv)
	}
	if rec := run.Test(sp, ev); rec.Result != actionlog.ResultOK || rec.Event != TaskMoved {
		t.Fatalf("record = %+v", rec)
	}
	eq(t, lines(), "task.moved:1:todo:test")
	if len(r.entries) != 1 || r.entries[0].ID != "1" {
		t.Fatalf("audit = %+v", r.entries)
	}
	t.Setenv(DepthEnv, "1")
	deep := New([]Spec{*sp}, t.TempDir(), "cli", nil, nil)
	if why := deep.Check(&deep.specs[0], ev); !strings.Contains(why, "depth") {
		t.Fatalf("depth check = %q", why)
	}
}
