package backend

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	t1.Title, t1.Status = "t1", "done"
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
	t1.Title, t1.Status = "t1", "y"
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

type auditRow struct {
	op, backend, id, details string
	failed                   bool
}

type auditSink struct{ rows []auditRow }

func (a *auditSink) Audit(op, backend, id string, err error, details string) {
	a.rows = append(a.rows, auditRow{op, backend, id, details, err != nil})
}

func TestSetAuditsWrites(t *testing.T) {
	s := NewSet([]Named{{Name: "a", Backend: &fake{tasks: []task.Task{{ID: "1", Title: "One", Status: "x"}}}}})
	sink := &auditSink{}
	s.SetAuditor(sink)
	ctx := WithNote(context.Background(), "moved")
	if err := s.Update(ctx, task.Task{ID: "1", Title: "One", Status: "y", Backend: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), task.Task{Title: "Two", Status: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "", "nope"); err == nil {
		t.Fatal("want not found")
	}
	if err := s.Update(context.Background(), task.Task{ID: "1", Backend: "zzz"}); err == nil {
		t.Fatal("want unknown backend")
	}
	want := []auditRow{
		{"update", "a", "1", `moved "One" status=y`, false},
		{"create", "a", "new", `"Two" status=x`, false},
		{"delete", "a", "nope", "", true},
		{"update", "zzz", "1", "", true},
	}
	if len(sink.rows) != len(want) {
		t.Fatalf("%+v", sink.rows)
	}
	for i := range want {
		if sink.rows[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, sink.rows[i], want[i])
		}
	}
}

type reloadable struct {
	fake
	err   error
	calls *int
}

func (r *reloadable) Reload() error { *r.calls++; return r.err }

func TestSetReloadCallsReloadersAndJoinsErrors(t *testing.T) {
	var n1, n2 int
	s := NewSet([]Named{
		{Name: "bad", Backend: &reloadable{err: errors.New("gone"), calls: &n1}},
		{Name: "ok", Backend: &reloadable{calls: &n2}},
		{Name: "plain", Backend: &fake{}},
	})
	err := s.Reload()
	if n1 != 1 || n2 != 1 || err == nil || !strings.Contains(err.Error(), "backend bad: gone") {
		t.Fatalf("calls %d/%d err %v", n1, n2, err)
	}
}

func TestSetUpdateValidates(t *testing.T) {
	a := &fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "x"}}}
	s := NewSet([]Named{{Name: "a", Type: "fake", Backend: a}})
	ctx := context.Background()
	for name, bad := range map[string]task.Task{
		"empty title": {ID: "1", Title: "  ", Status: "y"},
		"bad due":     {ID: "1", Title: "one", Due: "tomorrow"},
		"start>due":   {ID: "1", Title: "one", Start: "2026-02-01", Due: "2026-01-01"},
	} {
		if err := s.Update(ctx, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if a.tasks[0].Title != "one" || a.tasks[0].Status != "x" {
		t.Fatalf("a rejected update reached the backend: %+v", a.tasks)
	}
	if err := s.Update(ctx, task.Task{ID: "1", Title: "one", Status: "y", Due: "2026-01-02"}); err != nil || a.tasks[0].Status != "y" {
		t.Fatalf("valid update: %v %+v", err, a.tasks)
	}
}

// failing lists with an error until ok is set.
type flaky struct {
	fake
	lists int
	fail  bool
}

func (f *flaky) List(ctx context.Context) ([]task.Task, error) {
	f.lists++
	if f.fail {
		return nil, errors.New("down")
	}
	return f.fake.List(ctx)
}

func TestSetCacheSkipsFailuresAndHandsOutCopies(t *testing.T) {
	f := &flaky{fake: fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "x"}}}, fail: true}
	s := NewSet([]Named{{Name: "a", Backend: f}})
	s.CacheLists()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		var perr *PartialError
		if _, err := s.List(ctx); !errors.As(err, &perr) {
			t.Fatalf("want PartialError, got %v", err)
		}
	}
	if f.lists != 2 {
		t.Fatalf("a failed list must not be cached: %d calls, want 2", f.lists)
	}
	f.fail = false
	first, err := s.List(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("%v %+v", err, first)
	}
	first[0].Title = "scribbled" // a caller changing its result must not reach the cache
	again, _ := s.List(ctx)
	if f.lists != 3 || again[0].Title != "one" {
		t.Fatalf("lists %d, cached title %q", f.lists, again[0].Title)
	}
}

func TestSetFailedWriteInvalidatesCache(t *testing.T) {
	n := 0
	b := &counting{fake: fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "x"}}}, lists: &n}
	s := NewSet([]Named{{Name: "a", Backend: b}})
	s.CacheLists()
	ctx := context.Background()
	s.List(ctx)
	if err := s.Update(ctx, task.Task{ID: "nope", Title: "t", Backend: "a"}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	s.List(ctx)
	if n != 2 {
		t.Fatalf("a write, even a failed one, may have changed the store: %d lists, want 2", n)
	}
}

type failing struct{ fake }

func (*failing) List(context.Context) ([]task.Task, error) { return nil, errors.New("down") }

type lockedSink struct {
	mu sync.Mutex
	n  int
}

func (l *lockedSink) Audit(string, string, string, error, string) {
	l.mu.Lock()
	l.n++
	l.mu.Unlock()
}

// Run with -race: lists of several backends, Audit and SetAuditor at the same time.
func TestSetConcurrentUseIsRaceFree(t *testing.T) {
	var items []Named
	for _, name := range []string{"a", "b", "c"} {
		items = append(items, Named{Name: name, Backend: &fake{tasks: []task.Task{{ID: "1", Title: name, Status: "x"}}}})
	}
	items = append(items, Named{Name: "bad", Backend: &failing{}})
	s := NewSet(items)
	s.CacheLists()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch (i + j) % 3 {
				case 0:
					var perr *PartialError
					if _, err := s.List(ctx); !errors.As(err, &perr) {
						t.Errorf("want PartialError, got %v", err)
					}
				case 1:
					s.SetAuditor(&lockedSink{})
				case 2:
					s.Audit("reload", "", "", nil, "x")
				}
			}
		}(i)
	}
	wg.Wait()
}

// patching records which write path the Set took.
type patching struct {
	fake
	patched, updated int
}

func (p *patching) Patch(_ context.Context, base, t task.Task) error {
	p.patched++
	return p.fake.Update(context.Background(), t)
}

func (p *patching) Update(ctx context.Context, t task.Task) error {
	p.updated++
	return p.fake.Update(ctx, t)
}

func TestSetUpdatePatchesOnlyWhenItKnowsTheOriginal(t *testing.T) {
	p := &patching{fake: fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "x"}}}}
	s := NewSet([]Named{{Name: "a", Backend: p}})
	edited := task.Task{ID: "1", Title: "one", Status: "y"}
	if err := s.Update(context.Background(), edited); err != nil || p.updated != 1 || p.patched != 0 {
		t.Fatalf("without the original: %v updated=%d patched=%d", err, p.updated, p.patched)
	}
	ctx := WithOld(context.Background(), task.Task{ID: "1", Title: "one", Status: "x"})
	if err := s.Update(ctx, edited); err != nil || p.updated != 1 || p.patched != 1 {
		t.Fatalf("with the original: %v updated=%d patched=%d", err, p.updated, p.patched)
	}
}

// partlyBroken lists its good tasks together with a LoadError.
type partlyBroken struct{ fake }

func (p *partlyBroken) List(ctx context.Context) ([]task.Task, error) {
	ts, _ := p.fake.List(ctx)
	return ts, &LoadError{Errs: []error{errors.New("2.md: bad frontmatter")}}
}

func TestSetKeepsTheTasksThatLoadedNextToALoadError(t *testing.T) {
	s := NewSet([]Named{{Name: "md", Backend: &partlyBroken{fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "x"}}}}}, {Name: "ok", Backend: &fake{tasks: []task.Task{{ID: "9", Title: "nine", Status: "x"}}}}})
	s.CacheLists()
	ctx := context.Background()
	got, err := s.List(ctx)
	var perr *PartialError
	if !errors.As(err, &perr) || len(perr.Names) != 1 || perr.Names[0] != "md" || !strings.Contains(err.Error(), "2.md") {
		t.Fatalf("want a PartialError for md, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the healthy tasks must stay: %+v", got)
	}
	// A bare ID still resolves: every backend answered, one only had a damaged file.
	if tk, err := s.Find(ctx, "", "9"); err != nil || tk.Backend != "ok" {
		t.Fatalf("find: %v %+v", err, tk)
	}
	if tk, err := s.Find(ctx, "md", "1"); err != nil || tk.Title != "one" {
		t.Fatalf("find in md: %v %+v", err, tk)
	}
}

func TestSetTracksOnlyWritesThatSucceeded(t *testing.T) {
	f := &fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "x"}}}
	s := NewSet([]Named{{Name: "a", Backend: f}})
	var events []WriteEvent
	s.SetWriteListener(func(ev WriteEvent) { events = append(events, ev) }, true)
	ctx := context.Background()

	if err := s.Update(ctx, task.Task{ID: "nope", Title: "t", Backend: "a"}); err == nil {
		t.Fatal("update of a missing task succeeded")
	}
	if w := s.TakeWritten(); len(w) != 0 {
		t.Fatalf("a failed write was recorded as ours: %v", w)
	}
	if len(events) != 1 || events[0].Err == nil || events[0].Op != OpUpdate {
		t.Fatalf("events %+v", events)
	}

	// needOld: the previous task is looked up when the caller did not pass it.
	if err := s.Update(ctx, task.Task{ID: "1", Title: "one", Status: "y", Backend: "a"}); err != nil {
		t.Fatal(err)
	}
	if ev := events[1]; !ev.HasOld || ev.Old.Status != "x" || ev.New.Status != "y" || ev.Err != nil {
		t.Fatalf("event %+v", ev)
	}
	created, err := s.Create(ctx, task.Task{Title: "two", Status: "x"})
	if err != nil {
		t.Fatal(err)
	}
	w := s.TakeWritten()
	if !w[Key("a", "1")] || !w[Key("a", created.ID)] || len(w) != 2 {
		t.Fatalf("written %v", w)
	}
	if again := s.TakeWritten(); len(again) != 0 {
		t.Fatalf("TakeWritten must clear: %v", again)
	}
	if err := s.Delete(ctx, "a", "1"); err != nil {
		t.Fatal(err)
	}
	if ev := events[len(events)-1]; ev.Op != OpDelete || !ev.HasOld || ev.Old.Title != "one" {
		t.Fatalf("delete event %+v", ev)
	}
	if present, ok := s.TakeWritten()[Key("a", "1")]; !ok || present {
		t.Fatalf("a delete must be recorded as gone: ok=%v present=%v", ok, present)
	}
	s.SetWriteListener(nil, false)
	if w := s.TakeWritten(); w != nil {
		t.Fatalf("without a listener nothing is tracked: %v", w)
	}
}

// stubBackend is a lock-less backend (no Patcher), like custom.
type stubBackend struct {
	Backend
	tasks   []task.Task
	updated int
}

func (s *stubBackend) List(context.Context) ([]task.Task, error) { return s.tasks, nil }
func (s *stubBackend) Update(context.Context, task.Task) error   { s.updated++; return nil }

func TestSetUpdateChecksTheGuardForABackendWithoutPatch(t *testing.T) {
	st := &stubBackend{tasks: []task.Task{{ID: "1", Title: "A", Status: "doing"}}}
	s := NewSet([]Named{{Name: "x", Backend: st}})
	ctx := WithExpect(context.Background(), "status", "todo")
	err := s.Update(ctx, task.Task{ID: "1", Title: "A", Status: "done", Backend: "x"})
	if !errors.Is(err, ErrPrecondition) || st.updated != 0 {
		t.Fatalf("err %v, updated %d", err, st.updated)
	}
	ctx = WithExpect(context.Background(), "status", "doing")
	if err := s.Update(ctx, task.Task{ID: "1", Title: "A", Status: "done", Backend: "x"}); err != nil || st.updated != 1 {
		t.Fatalf("err %v, updated %d", err, st.updated)
	}
}
