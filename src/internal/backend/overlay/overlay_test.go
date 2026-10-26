package overlay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tack/internal/backend"
	"tack/internal/claim"
	"tack/internal/task"
)

// fake is a store that keeps everything it is given and counts the writes.
type fake struct {
	mu     sync.Mutex
	tasks  []task.Task
	writes int
}

func (f *fake) List(context.Context) ([]task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]task.Task, len(f.tasks))
	for i, t := range f.tasks {
		out[i] = t.Clone()
	}
	return out, nil
}
func (f *fake) Statuses() []string { return []string{"todo", "doing"} }
func (f *fake) Update(ctx context.Context, t task.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks {
		if f.tasks[i].ID == t.ID {
			if err := backend.CheckExpect(ctx, f.tasks[i]); err != nil {
				return err
			}
			f.tasks[i] = t.Clone()
			f.writes++
			return nil
		}
	}
	return backend.ErrNotFound
}
func (f *fake) Create(_ context.Context, t task.Task) (task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.ID = fmt.Sprint(len(f.tasks) + 1)
	f.tasks = append(f.tasks, t.Clone())
	f.writes++
	return t, nil
}
func (f *fake) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			f.tasks = append(f.tasks[:i], f.tasks[i+1:]...)
			f.writes++
			return nil
		}
	}
	return backend.ErrNotFound
}
func (f *fake) Close() error { return nil }

func open(t *testing.T, fields ...string) (*Backend, *fake, string) {
	t.Helper()
	f := &fake{tasks: []task.Task{
		{ID: "1", Title: "one", Status: "todo", Assignee: "ann"},
		{ID: "2", Title: "two", Status: "todo"},
	}}
	if len(fields) == 0 {
		fields = ClaimFields
	}
	path := filepath.Join(t.TempDir(), "sub", "ov.json")
	b, err := Wrap(f, path, fields)
	if err != nil {
		t.Fatal(err)
	}
	return b, f, path
}

var ctx = context.Background()

// set puts b in a Set named "b", the way the config does, so the claim package
// writes through the real update path (guard, base task, patch).
func set(b *Backend) *backend.Set {
	return backend.NewSet([]backend.Named{{Name: "b", Type: "test", Backend: b}})
}

func TestWrapRejectsBadFields(t *testing.T) {
	for _, fields := range [][]string{{"id"}, {"nope"}, {}} {
		if _, err := Wrap(&fake{}, "x.json", fields); err == nil {
			t.Errorf("%v accepted", fields)
		}
	}
}

func TestClaimStaysLocal(t *testing.T) {
	b, f, path := open(t)
	s := set(b)
	ts, _ := s.List(ctx)
	if ts[0].Assignee != "ann" || len(ts[0].Overlaid) != 0 {
		t.Fatalf("a store value shows, unmarked, while the overlay has none: %+v", ts[0])
	}
	got, err := claim.Take(ctx, s, ts[1], claim.Request{Agent: "claude", Lease: time.Hour}, time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "claude" || f.writes != 0 || f.tasks[1].Assignee != "" {
		t.Fatalf("claim reached the store: writes=%d %+v", f.writes, f.tasks[1])
	}
	ts, _ = s.List(ctx)
	if ts[1].Assignee != "claude" || ts[1].Lease == "" || ts[1].ClaimedAt == "" {
		t.Fatalf("not merged: %+v", ts[1])
	}
	if want := []string{"assignee", "lease", "claimed_at"}; fmt.Sprint(ts[1].Overlaid) != fmt.Sprint(want) {
		t.Fatalf("overlaid %v, want %v", ts[1].Overlaid, want)
	}
	if !ts[1].IsOverlaid("lease") || ts[1].IsOverlaid("title") {
		t.Fatal("IsOverlaid")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// a second claimer loses, the holder releases
	if _, err := claim.Take(ctx, s, ts[1], claim.Request{Agent: "codex", Lease: time.Hour}, time.Date(2026, 10, 19, 12, 1, 0, 0, time.UTC)); !errors.Is(err, backend.ErrPrecondition) {
		t.Fatalf("second claim: %v", err)
	}
	if _, _, err := claim.Give(ctx, s, ts[1], "claude", false, ""); err != nil {
		t.Fatal(err)
	}
	ts, _ = s.List(ctx)
	if ts[1].Assignee != "" || len(ts[1].Overlaid) != 0 || f.writes != 0 {
		t.Fatalf("after release: writes=%d %+v", f.writes, ts[1])
	}
}

// A claim or release that also moves the task: the guard is on an overlaid
// field, which the store never held, so only the overlay may check it.
func TestClaimAndReleaseWithStatus(t *testing.T) {
	b, f, _ := open(t)
	s := set(b)
	ts, _ := s.List(ctx)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	got, err := claim.Take(ctx, s, ts[1], claim.Request{Agent: "claude", Lease: time.Hour, Status: "doing"}, now)
	if err != nil {
		t.Fatalf("claim -status: %v", err)
	}
	if got.Status != "doing" || f.tasks[1].Status != "doing" || f.tasks[1].Assignee != "" {
		t.Fatalf("store: %+v", f.tasks[1])
	}
	ts, _ = s.List(ctx)
	if ts[1].Assignee != "claude" {
		t.Fatalf("claim not kept: %+v", ts[1])
	}
	if _, _, err := claim.Give(ctx, s, ts[1], "claude", false, "todo"); err != nil {
		t.Fatalf("release -status: %v", err)
	}
	ts, _ = s.List(ctx)
	if ts[1].Status != "todo" || f.tasks[1].Status != "todo" || ts[1].Assignee != "" || len(ts[1].Overlaid) != 0 {
		t.Fatalf("after release: %+v", ts[1])
	}
	// the guard still holds
	ts[1].Status = "doing"
	other := claim.Request{Agent: "codex", Lease: time.Hour}
	if _, err := claim.Take(ctx, s, ts[1], other, now); err != nil {
		t.Fatal(err)
	}
	if _, err := claim.Take(ctx, s, ts[1], claim.Request{Agent: "claude", Status: "todo"}, now); !errors.Is(err, backend.ErrPrecondition) {
		t.Fatalf("stale claim: %v", err)
	}
}

func TestOtherFieldsGoToTheStore(t *testing.T) {
	b, f, _ := open(t)
	ts, _ := b.List(ctx)
	moved := ts[1].Clone()
	moved.Status, moved.Assignee = "doing", "claude"
	if err := b.Patch(ctx, ts[1], moved); err != nil {
		t.Fatal(err)
	}
	if f.tasks[1].Status != "doing" || f.tasks[1].Assignee != "" {
		t.Fatalf("store: %+v", f.tasks[1])
	}
	ts, _ = b.List(ctx)
	if ts[1].Status != "doing" || ts[1].Assignee != "claude" {
		t.Fatalf("merged: %+v", ts[1])
	}
	// Update without a base keeps the store's own value of overlaid fields
	again := ts[0].Clone()
	again.Title = "uno"
	if err := b.Update(ctx, again); err != nil {
		t.Fatal(err)
	}
	if f.tasks[0].Title != "uno" || f.tasks[0].Assignee != "ann" {
		t.Fatalf("store: %+v", f.tasks[0])
	}
}

func TestGenericField(t *testing.T) {
	b, f, _ := open(t, "priority")
	ts, _ := b.List(ctx)
	p := ts[0].Clone()
	p.Priority = "high"
	if err := b.Patch(ctx, ts[0], p); err != nil {
		t.Fatal(err)
	}
	ts, _ = b.List(ctx)
	if ts[0].Priority != "high" || !ts[0].IsOverlaid("priority") || f.writes != 0 {
		t.Fatalf("%+v writes=%d", ts[0], f.writes)
	}
	if b.Stores("priority") != true {
		t.Fatal("Stores")
	}
}

func TestGuardRaceExactlyOneWins(t *testing.T) {
	b, _, _ := open(t)
	s := set(b)
	ts, _ := s.List(ctx)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = claim.Take(ctx, s, ts[1], claim.Request{Agent: fmt.Sprintf("a%d", i), Lease: time.Hour}, time.Now())
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, backend.ErrPrecondition):
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d winners", wins)
	}
}

func TestCreateDeleteAndMissingOrCorruptFile(t *testing.T) {
	b, f, path := open(t)
	made, err := b.Create(ctx, task.Task{Title: "new", Status: "todo", Assignee: "claude"})
	if err != nil || made.Assignee != "claude" || !made.IsOverlaid("assignee") || f.tasks[2].Assignee != "" {
		t.Fatalf("create: %+v %v / %+v", made, err, f.tasks[2])
	}
	if err := b.Delete(ctx, made.ID); err != nil {
		t.Fatal(err)
	}
	ts, _ := b.List(ctx)
	if len(ts) != 2 {
		t.Fatal(ts)
	}
	if data, _ := os.ReadFile(path); string(data) != "{}\n" {
		t.Fatalf("entry left behind: %s", data)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err = b.List(ctx)
	var le *backend.LoadError
	if len(ts) != 2 || !errors.As(err, &le) {
		t.Fatalf("corrupt file: %v %v", ts, err)
	}
	if err := b.Update(ctx, ts[0]); err == nil {
		t.Fatal("a corrupt overlay file must not be overwritten")
	}
	if data, _ := os.ReadFile(path); string(data) != "{nope" {
		t.Fatal("corrupt file rewritten")
	}
}

func TestCreateRollsBackWhenOverlayWriteFails(t *testing.T) {
	b, f, path := open(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(ctx, task.Task{Title: "new", Status: "todo", Assignee: "claude"}); err == nil {
		t.Fatal("create must fail")
	}
	if len(f.tasks) != 2 {
		t.Fatalf("created task left behind: %+v", f.tasks)
	}
}

func TestCreateDoesNotInheritAStaleEntry(t *testing.T) {
	b, _, path := open(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "{\"3\": {\"assignee\": \"old\"}}"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	made, err := b.Create(ctx, task.Task{Title: "new", Status: "todo"})
	if err != nil || made.ID != "3" || made.Assignee != "" {
		t.Fatalf("create: %+v %v", made, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "{}\n" {
		t.Fatalf("stale entry kept: %s", data)
	}
}

func TestChangedSeesOtherWriters(t *testing.T) {
	b, _, path := open(t)
	if _, err := b.List(ctx); err != nil {
		t.Fatal(err)
	}
	if c, err := b.Changed(); err != nil || c {
		t.Fatalf("changed right after a read: %v %v", c, err)
	}
	other, _ := Wrap(&fake{tasks: []task.Task{{ID: "2", Title: "two", Status: "todo"}}}, path, ClaimFields)
	if _, err := claim.Take(ctx, set(other), task.Task{ID: "2", Title: "two", Status: "todo", Backend: "b"}, claim.Request{Agent: "x"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c, err := b.Changed(); err != nil || !c {
		t.Fatalf("not noticed: %v %v", c, err)
	}
}

func TestListPrunesEntriesOfTasksTheStoreLost(t *testing.T) {
	b, f, path := open(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// 2 stays; 7 was deleted outside tack.
	if err := os.WriteFile(path, []byte(`{"2": {"assignee": "ann"}, "7": {"assignee": "ghost", "lease": "2026-01-01T00:00:00Z"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := b.List(ctx)
	if err != nil || len(ts) != 2 || ts[1].Assignee != "ann" {
		t.Fatalf("list: %+v %v", ts, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"2"`) || strings.Contains(string(data), `"7"`) {
		t.Fatalf("stale entry not pruned:\n%s", data)
	}
	// An id the store reuses later starts clean.
	f.tasks = append(f.tasks, task.Task{ID: "7", Title: "reused", Status: "todo"})
	if ts, _ = b.List(ctx); ts[2].Assignee != "" {
		t.Errorf("a reused id inherited %+v", ts[2])
	}
}

// One failed prune (a store that errs once, a lock timeout) must not stop the
// pruning for good.
func TestPruneIsRetriedAfterATransientError(t *testing.T) {
	f := &fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "todo"}}}
	flaky := &flakyStore{fake: f, failCall: 2} // the List inside the first prune
	path := filepath.Join(t.TempDir(), "ov.json")
	if err := os.WriteFile(path, []byte(`{"9": {"assignee": "ghost"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Wrap(flaky, path, ClaimFields)
	if err != nil {
		t.Fatal(err)
	}
	b.List(ctx)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"9"`) {
		t.Fatalf("pruned despite the failure:\n%s", data)
	}
	b.List(ctx)
	if data, _ := os.ReadFile(path); strings.Contains(string(data), `"9"`) {
		t.Fatalf("not retried after a transient error:\n%s", data)
	}
}

type flakyStore struct {
	*fake
	calls, failCall int
}

func (s *flakyStore) List(c context.Context) ([]task.Task, error) {
	s.calls++
	if s.calls == s.failCall {
		return nil, errors.New("transient")
	}
	return s.fake.List(c)
}

// A store that did not load in full proves nothing about a missing id.
func TestListDoesNotPruneOnAPartialLoad(t *testing.T) {
	f := &fake{tasks: []task.Task{{ID: "1", Title: "one", Status: "todo"}}}
	partial := &partialStore{fake: f}
	path := filepath.Join(t.TempDir(), "ov.json")
	if err := os.WriteFile(path, []byte(`{"9": {"assignee": "ann"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Wrap(partial, path, ClaimFields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.List(ctx); err == nil {
		t.Fatal("the partial error was swallowed")
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"9"`) {
		t.Fatalf("pruned on a partial load:\n%s", data)
	}
}

type partialStore struct{ *fake }

func (p *partialStore) List(c context.Context) ([]task.Task, error) {
	ts, _ := p.fake.List(c)
	return ts, &backend.LoadError{Errs: []error{errors.New("file 2 is broken")}}
}

// When the overlay file cannot be written, the store's part of the edit is undone.
func TestFailedOverlayWriteUndoesTheStoreChange(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}
	b, f, path := open(t)
	s := set(b)
	ts, _ := s.List(ctx)
	// a first write creates the overlay file and its lock
	if _, err := claim.Take(ctx, s, ts[1], claim.Request{Agent: "x"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	ts, _ = s.List(ctx)
	edit := ts[0]
	edit.Title = "renamed"
	edit.Lease = "2026-12-01T00:00:00Z" // overlaid: needs the file
	if err := s.Update(ctx, edit); err == nil {
		t.Fatal("update succeeded with an unwritable overlay")
	}
	if f.tasks[0].Title != "one" {
		t.Fatalf("the store kept half of the failed edit: %+v", f.tasks[0])
	}
}
