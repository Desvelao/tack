package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tkan/internal/task"
)

type fake struct{ tasks []task.Task }

func (f *fake) List(context.Context) ([]task.Task, error) {
	return append([]task.Task(nil), f.tasks...), nil
}
func (f *fake) Statuses() []string {
	var s []string
	for _, t := range f.tasks {
		s = append(s, t.Status)
	}
	return s
}
func (f *fake) Update(_ context.Context, t task.Task) error {
	for i := range f.tasks {
		if f.tasks[i].ID == t.ID {
			f.tasks[i] = t
			return nil
		}
	}
	return ErrNotFound
}
func (f *fake) Create(_ context.Context, t task.Task) (task.Task, error) {
	t.ID = "new"
	f.tasks = append(f.tasks, t)
	return t, nil
}
func (f *fake) Delete(_ context.Context, id string) error {
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			f.tasks = append(f.tasks[:i], f.tasks[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}
func (f *fake) Close() error { return nil }

func TestSetTagsRoutesAndFinds(t *testing.T) {
	a := &fake{tasks: []task.Task{{ID: "1", Status: "x"}, {ID: "2", Status: "y"}}}
	b := &fake{tasks: []task.Task{{ID: "1", Status: "y"}}}
	s := NewSet([]Named{{Name: "a", Type: "fake", Backend: a}, {Name: "b", Type: "fake", Backend: b}})
	ctx := context.Background()

	all, _ := s.List(ctx)
	if len(all) != 3 || all[0].Backend != "a" || all[2].Backend != "b" {
		t.Fatalf("%+v", all)
	}
	if got := s.Statuses(); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("statuses %v", got)
	}

	var amb *AmbiguousError
	if _, err := s.Find(ctx, "", "1"); !errors.As(err, &amb) || len(amb.Backends) != 2 {
		t.Fatalf("want ambiguity, got %v", err)
	}
	if got, err := s.Find(ctx, "", "2"); err != nil || got.Backend != "a" {
		t.Fatalf("%v %+v", err, got)
	}
	if got, err := s.Find(ctx, "", "B:1"); err != nil || got.Backend != "b" {
		t.Fatalf("NAME:ID: %v %+v", err, got)
	}
	if got, err := s.Find(ctx, "b", "1"); err != nil || got.Backend != "b" {
		t.Fatalf("-b: %v %+v", err, got)
	}
	if _, err := s.Find(ctx, "", "404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Find(ctx, "zzz", "1"); err == nil {
		t.Fatal("unknown backend")
	}

	t1, _ := s.Find(ctx, "b", "1")
	t1.Status = "done"
	if err := s.Update(ctx, t1); err != nil || b.tasks[0].Status != "done" || a.tasks[0].Status != "x" {
		t.Fatalf("update routed wrongly: %v %+v %+v", err, a.tasks, b.tasks)
	}
	created, err := s.Create(ctx, task.Task{Title: "t", Backend: "b"})
	if err != nil || created.Backend != "b" || len(b.tasks) != 2 {
		t.Fatalf("%v %+v", err, created)
	}
	if c, _ := s.Create(ctx, task.Task{Title: "d"}); c.Backend != "a" {
		t.Fatalf("default backend: %+v", c)
	}
	if err := s.Delete(ctx, "b", "1"); err != nil || len(b.tasks) != 1 {
		t.Fatalf("%v", err)
	}
}

type broken struct{ fake }

func (broken) List(context.Context) ([]task.Task, error) { return nil, errors.New("unreachable") }

func TestSetListKeepsHealthyBackendsWhenOneFails(t *testing.T) {
	good := &fake{tasks: []task.Task{{ID: "1", Status: "x"}}}
	s := NewSet([]Named{{Name: "bad", Type: "fake", Backend: &broken{}}, {Name: "good", Type: "fake", Backend: good}})
	ctx := context.Background()

	tasks, err := s.List(ctx)
	var perr *PartialError
	if !errors.As(err, &perr) || len(perr.Names) != 1 || perr.Names[0] != "bad" {
		t.Fatalf("want PartialError for bad, got %v", err)
	}
	if len(tasks) != 1 || tasks[0].Backend != "good" {
		t.Fatalf("healthy tasks lost: %+v", tasks)
	}
	// A bare ID is not resolved while a backend is down: it might exist there too.
	for _, ref := range []string{"1", "9"} {
		_, err := s.Find(ctx, "", ref)
		var perr *PartialError
		if err == nil || errors.Is(err, ErrNotFound) || !errors.As(err, &perr) || !strings.Contains(err.Error(), "-b NAME") {
			t.Fatalf("bare id %q with a failed backend must surface the failure, got %v", ref, err)
		}
	}
	// NAME:ID pins the backend, so the healthy one can be used
	if got, err := s.Find(ctx, "", "good:1"); err != nil || got.Backend != "good" {
		t.Fatalf("NAME:ID with a failed backend: %+v %v", got, err)
	}
	// Naming the backend only consults that one.
	if got, err := s.Find(ctx, "good", "1"); err != nil || got.ID != "1" {
		t.Fatalf("scoped find: %+v %v", got, err)
	}
	if _, err := s.Find(ctx, "bad", "1"); err == nil {
		t.Fatal("scoped find in the failing backend must fail")
	}
}

type watcher struct {
	fake
	changed bool
	err     error
	polled  *int
}

func (w *watcher) Changed() (bool, error) { *w.polled++; return w.changed, w.err }

func TestSetChangedPollsAllWatchersAndJoinsErrors(t *testing.T) {
	var n1, n2 int
	bad := &watcher{err: errors.New("stat failed"), polled: &n1}
	ok := &watcher{changed: true, polled: &n2}
	s := NewSet([]Named{{Name: "bad", Backend: bad}, {Name: "ok", Backend: ok}, {Name: "plain", Backend: &fake{}}})
	changed, err := s.Changed()
	if n1 != 1 || n2 != 1 {
		t.Fatalf("watchers polled %d/%d times, want 1/1", n1, n2)
	}
	if !changed || err == nil || !strings.Contains(err.Error(), "backend bad: stat failed") {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

type counting struct {
	fake
	lists *int
}

func (c *counting) List(ctx context.Context) ([]task.Task, error) {
	*c.lists++
	return c.fake.List(ctx)
}

func TestSetCacheListsUntilAWrite(t *testing.T) {
	n := 0
	b := &counting{fake: fake{tasks: []task.Task{{ID: "1", Status: "x"}}}, lists: &n}
	s := NewSet([]Named{{Name: "a", Backend: b}})
	ctx := context.Background()
	s.List(ctx)
	s.List(ctx)
	if n != 2 {
		t.Fatalf("without caching every List hits the backend, got %d calls", n)
	}
	n = 0
	s.CacheLists()
	s.List(ctx)
	s.List(ctx)
	if _, err := s.Find(ctx, "a", "1"); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cached: %d backend lists, want 1", n)
	}
	t1, _ := s.Find(ctx, "", "1")
	t1.Status = "y"
	if err := s.Update(ctx, t1); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(ctx)
	if n != 2 || got[0].Status != "y" {
		t.Fatalf("a write must invalidate the cache: %d calls, %+v", n, got)
	}
	// callers cannot corrupt the cache
	got[0].Status = "mutated"
	again, _ := s.List(ctx)
	if again[0].Status != "y" {
		t.Fatalf("cache aliased the returned slice: %+v", again)
	}
}

func TestSetCreateValidatesForEveryBackend(t *testing.T) {
	f := &fake{}
	s := NewSet([]Named{{Name: "a", Backend: f}})
	ctx := context.Background()
	for _, bad := range []task.Task{{Title: " "}, {Title: "x", Due: "tomorrow"}, {Title: "x", Start: "2026-06-01", Due: "2026-05-01"}} {
		if _, err := s.Create(ctx, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if len(f.tasks) != 0 {
		t.Fatalf("an invalid task reached the backend: %+v", f.tasks)
	}
	if _, err := s.Create(ctx, task.Task{Title: "ok", Due: "2026-05-01"}); err != nil || len(f.tasks) != 1 {
		t.Fatalf("valid task rejected: %v", err)
	}
}

type slow struct {
	fake
	delay time.Duration
	err   error
}

func (s *slow) List(ctx context.Context) ([]task.Task, error) {
	time.Sleep(s.delay)
	if s.err != nil {
		return nil, s.err
	}
	return s.fake.List(ctx)
}

func TestSetListQueriesBackendsConcurrentlyInConfigOrder(t *testing.T) {
	const d = 300 * time.Millisecond
	items := []Named{
		{Name: "a", Backend: &slow{fake: fake{tasks: []task.Task{{ID: "1"}}}, delay: d}},
		{Name: "b", Backend: &slow{delay: d, err: errors.New("down")}},
		{Name: "c", Backend: &slow{fake: fake{tasks: []task.Task{{ID: "3"}}}, delay: d}},
		{Name: "d", Backend: &slow{delay: d, err: errors.New("also down")}},
	}
	s := NewSet(items)
	start := time.Now()
	tasks, err := s.List(context.Background())
	if took := time.Since(start); took > 3*d {
		t.Fatalf("took %s: backends were queried one after another", took)
	}
	var perr *PartialError
	if !errors.As(err, &perr) || strings.Join(perr.Names, ",") != "b,d" {
		t.Fatalf("partial error %v", err)
	}
	if len(tasks) != 2 || tasks[0].Backend != "a" || tasks[1].Backend != "c" {
		t.Fatalf("order not kept: %+v", tasks)
	}
	// Find without a name searches all backends the same way
	start = time.Now()
	if _, err := s.Find(context.Background(), "", "a:1"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*d {
		t.Fatalf("NAME:ID find took %s: it should only ask one backend", took)
	}
}
