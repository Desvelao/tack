package hook

import (
	"errors"
	"testing"

	"tack/internal/backend"
	"tack/internal/task"
)

func newTestWatcher() (*Watcher, *[]string) {
	var got []string
	r := Observing("test", func(ev Event) { got = append(got, ev.Name+" "+ev.subject().ID) })
	return NewWatcher(r, "test"), &got
}

func ts(ids ...string) []task.Task {
	var out []task.Task
	for _, id := range ids {
		out = append(out, task.Task{Backend: "b", ID: id, Title: "t" + id})
	}
	return out
}

func TestWatcherFirstListIsNotNews(t *testing.T) {
	w, got := newTestWatcher()
	w.Observe(ts("1", "2"), nil, nil)
	if len(*got) != 0 {
		t.Fatalf("first list fired %v", *got)
	}
	w.Observe(ts("2", "3"), nil, nil)
	want := []string{"task.disappeared 1", "task.appeared 3"}
	if len(*got) != 2 || (*got)[0] != want[0] || (*got)[1] != want[1] {
		t.Fatalf("got %v, want %v", *got, want)
	}
}

func TestWatcherOwnWritesAreSkipped(t *testing.T) {
	w, got := newTestWatcher()
	w.Observe(ts("1"), nil, nil)
	w.Observe(ts("1", "2"), nil, map[string]bool{backend.Key("b", "2"): true})
	if len(*got) != 0 {
		t.Fatalf("own write fired %v", *got)
	}
}

func TestWatcherPartialBackendIsLeftOut(t *testing.T) {
	w, got := newTestWatcher()
	w.Observe(ts("1"), nil, nil)
	w.Observe(nil, &backend.PartialError{Names: []string{"b"}}, nil) // b failed: its task is not gone
	w.Observe(ts("1"), nil, nil)                                     // b recovered: not new either
	if len(*got) != 0 {
		t.Fatalf("partial list fired %v", *got)
	}
	w.Observe(ts("1", "2"), nil, nil)
	if len(*got) != 1 || (*got)[0] != "task.appeared 2" {
		t.Fatalf("after recovery got %v", *got)
	}
}

func TestWatcherReloadEvents(t *testing.T) {
	w, got := newTestWatcher()
	w.Reload(nil)
	w.Reload(nil)
	boom := errors.New("boom")
	if !w.Reload(boom) || w.Reload(boom) {
		t.Fatal("a repeating failure must be new once")
	}
	w.Reload(nil)
	if !w.Reload(boom) {
		t.Fatal("a failure after a success is new again")
	}
	want := []string{"reload.ok ", "reload.ok ", "reload.fail ", "reload.ok ", "reload.fail "}
	if len(*got) != len(want) {
		t.Fatalf("got %v, want %v", *got, want)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Fatalf("got %v, want %v", *got, want)
		}
	}
}

func TestNilWatcherDoesNothing(t *testing.T) {
	var w *Watcher
	w.Observe(ts("1"), nil, nil)
	w.Tick()
	w.Reload(nil)
}
