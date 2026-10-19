// Package backendtest holds checks every backend that implements
// backend.Patcher must pass.
package backendtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tkan/internal/backend"
	"tkan/internal/claim"
	"tkan/internal/task"
)

// CheckPatch is the two-writer scenario: reader lists the first task, writer
// retitles it, then reader moves it to another status with Patch from the copy
// it read. The title the writer stored must survive. reader and writer are two
// handles on one store; touch makes the store's change stamp differ (a file
// time stamp may not have moved within the same clock tick).
func CheckPatch(t *testing.T, reader, writer backend.Backend, touch func()) {
	t.Helper()
	ctx := context.Background()
	p, ok := reader.(backend.Patcher)
	if !ok {
		t.Fatalf("%T does not implement backend.Patcher", reader)
	}
	tasks, err := reader.List(ctx)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list: %v %v", tasks, err)
	}
	base := tasks[0]

	theirs := base.Clone()
	theirs.Title = "retitled by the other writer"
	if err := writer.Update(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	touch()

	mine := base.Clone()
	mine.Status = "moved-by-reader"
	if err := p.Patch(ctx, base, mine); err != nil {
		t.Fatal(err)
	}
	touch()

	got, err := writer.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range got {
		if x.ID != base.ID {
			continue
		}
		if x.Title != theirs.Title || x.Status != mine.Status {
			t.Fatalf("title %q (want the other writer's %q), status %q (want %q)", x.Title, theirs.Title, x.Status, mine.Status)
		}
		return
	}
	t.Fatalf("task %s vanished", base.ID)
}

// CheckConcurrentWriters runs n handles on one store (each in its own
// goroutine, as separate tkan processes would) that each create `each` tasks,
// retitle every one of them and delete every third, then checks: unique IDs, no
// lost task, every retitle kept, no temporary files left in dir, and no
// leftover lock file blocking the next writer. open returns a new handle on
// the same store; dir is the directory holding the store's files.
func CheckConcurrentWriters(t *testing.T, open func() backend.Backend, dir string, n, each int) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	kept := map[string]string{} // id -> title the writer set last
	for w := 0; w < n; w++ {
		b := open()
		wg.Add(1)
		go func(w int, b backend.Backend) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c, err := b.Create(ctx, task.Task{Title: fmt.Sprintf("w%d-%d", w, i), Status: "todo"})
				if err != nil {
					t.Errorf("writer %d create: %v", w, err)
					return
				}
				c.Title += "-edited"
				if err := b.Update(ctx, c); err != nil {
					t.Errorf("writer %d update %s: %v", w, c.ID, err)
					return
				}
				if i%3 == 0 {
					if err := b.Delete(ctx, c.ID); err != nil {
						t.Errorf("writer %d delete %s: %v", w, c.ID, err)
						return
					}
					continue
				}
				mu.Lock()
				kept[c.ID] = c.Title
				mu.Unlock()
			}
		}(w, b)
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	got, err := open().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, x := range got {
		if seen[x.ID] {
			t.Errorf("duplicate id %s", x.ID)
		}
		seen[x.ID] = true
		if want, ok := kept[x.ID]; !ok {
			t.Errorf("task %s (%q) should have been deleted or never existed", x.ID, x.Title)
		} else if x.Title != want {
			t.Errorf("task %s: title %q, want %q (an update was lost)", x.ID, x.Title, want)
		}
	}
	for id := range kept {
		if !seen[id] {
			t.Errorf("task %s was lost", id)
		}
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, ".tkan-*.tmp")); len(tmp) > 0 {
		t.Errorf("leftover temporary files: %v", tmp)
	}
	// Lock files stay (they are the lock), but without a holder they block nobody.
	done := make(chan error, 1)
	go func() {
		_, err := open().Create(ctx, task.Task{Title: "after", Status: "todo"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("write after the others finished: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("a leftover lock file blocks the next writer")
	}
}

// CheckCompareAndSet is the claim race: every handle (separate handles on one
// store, each in its own goroutine, as separate tkan processes would be) tries
// to move the first task from its current status to a status of its own, with
// backend.WithExpect. Exactly one must win, the others get ErrPrecondition,
// and the store holds the winner's status. It runs once with Update and, when
// the backend implements backend.Patcher, once with Patch.
func CheckCompareAndSet(t *testing.T, handles []backend.Backend) {
	t.Helper()
	ctx := context.Background()
	tasks, err := handles[0].List(ctx)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list: %v %v", tasks, err)
	}
	base := tasks[0]
	for _, mode := range []string{"update", "patch"} {
		if _, ok := handles[0].(backend.Patcher); mode == "patch" && !ok {
			continue
		}
		reset := base.Clone()
		if err := handles[0].Update(ctx, reset); err != nil {
			t.Fatal(err)
		}
		errs := make([]error, len(handles))
		var wg sync.WaitGroup
		for i, h := range handles {
			wg.Add(1)
			go func(i int, h backend.Backend) {
				defer wg.Done()
				mine := base.Clone()
				mine.Status = fmt.Sprintf("claimed-%s-%d", mode, i)
				c := backend.WithExpect(ctx, "status", base.Status)
				if p, ok := h.(backend.Patcher); ok && mode == "patch" {
					errs[i] = p.Patch(c, base, mine)
				} else {
					errs[i] = h.Update(c, mine)
				}
			}(i, h)
		}
		wg.Wait()
		winner := -1
		for i, err := range errs {
			switch {
			case err == nil && winner < 0:
				winner = i
			case err == nil:
				t.Fatalf("%s: handles %d and %d both won the claim", mode, winner, i)
			case !errors.Is(err, backend.ErrPrecondition):
				t.Fatalf("%s: handle %d: %v, want ErrPrecondition", mode, i, err)
			}
		}
		if winner < 0 {
			t.Fatalf("%s: nobody won the claim: %v", mode, errs)
		}
		got, err := handles[0].List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("claimed-%s-%d", mode, winner); got[0].Status != want {
			t.Fatalf("%s: status %q, want the winner's %q", mode, got[0].Status, want)
		}
	}
}

// handleStore lets claim.Take write through one handle, the way backend.Set
// does: Patch over the task as read when the backend can, else Update.
type handleStore struct {
	h    backend.Backend
	base task.Task
}

func (s handleStore) Update(ctx context.Context, t task.Task) error {
	if p, ok := s.h.(backend.Patcher); ok {
		return p.Patch(ctx, s.base, t)
	}
	return s.h.Update(ctx, t)
}

func (s handleStore) Stores(_, field string) bool {
	if fs, ok := s.h.(backend.FieldStorer); ok {
		return fs.Stores(field)
	}
	return true
}

// CheckClaimRace races every handle (as separate tkan processes would) to
// claim the first task with a lease, twice: while it is free, then after the
// winner's lease ran out (a takeover, guarded on the lease). Each time exactly
// one must win, the others get ErrPrecondition, and the store holds the
// winner as assignee with its lease. The store must keep assignee and lease.
func CheckClaimRace(t *testing.T, handles []backend.Backend) {
	t.Helper()
	ctx := context.Background()
	tasks, err := handles[0].List(ctx)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list: %v %v", tasks, err)
	}
	free := tasks[0].Clone()
	free.Assignee, free.Lease = "", ""
	if err := handles[0].Update(ctx, free); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for round, at := range []time.Time{start, start.Add(time.Hour)} {
		cur, err := handles[0].List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		base := cur[0]
		errs := make([]error, len(handles))
		var wg sync.WaitGroup
		for i, h := range handles {
			wg.Add(1)
			go func(i int, h backend.Backend) {
				defer wg.Done()
				r := claim.Request{Agent: fmt.Sprintf("agent-%d-%d", round, i), Lease: 30 * time.Minute}
				_, errs[i] = claim.Take(ctx, handleStore{h, base}, base, r, at)
			}(i, h)
		}
		wg.Wait()
		winner := -1
		for i, err := range errs {
			switch {
			case err == nil && winner < 0:
				winner = i
			case err == nil:
				t.Fatalf("round %d: handles %d and %d both claimed the task", round, winner, i)
			case !errors.Is(err, backend.ErrPrecondition):
				t.Fatalf("round %d: handle %d: %v, want ErrPrecondition", round, i, err)
			}
		}
		if winner < 0 {
			t.Fatalf("round %d: nobody won the claim: %v", round, errs)
		}
		got, err := handles[0].List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantLease := at.Add(30 * time.Minute).Format(time.RFC3339)
		if want := fmt.Sprintf("agent-%d-%d", round, winner); got[0].Assignee != want || got[0].Lease != wantLease {
			t.Fatalf("round %d: stored %q until %q, want the winner %q until %q", round, got[0].Assignee, got[0].Lease, want, wantLease)
		}
	}
}

// CheckIDsNotReused checks the high-water mark: after the highest id is
// deleted, a create (from the same handle or another one) gives a new id, and
// with the mark file at markPath gone a create falls back to max+1 instead of
// failing. open returns a new handle on the store (which must start empty).
func CheckIDsNotReused(t *testing.T, open func() backend.Backend, markPath string) {
	t.Helper()
	ctx := context.Background()
	a, b := open(), open()
	create := func(h backend.Backend) string {
		t.Helper()
		c, err := h.Create(ctx, task.Task{Title: "x", Status: "todo"})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	create(a)
	create(a)
	if id := create(a); id != "3" {
		t.Fatalf("third id = %s, want 3", id)
	}
	if err := a.Delete(ctx, "3"); err != nil {
		t.Fatal(err)
	}
	if id := create(a); id != "4" {
		t.Errorf("id after deleting the highest = %s, want 4 (reused)", id)
	}
	if err := b.Delete(ctx, "4"); err != nil {
		t.Fatal(err)
	}
	if id := create(b); id != "5" {
		t.Errorf("id from another handle = %s, want 5 (reused)", id)
	}
	if err := os.Remove(markPath); err != nil {
		t.Fatalf("no high-water mark file: %v", err)
	}
	if err := a.Delete(ctx, "5"); err != nil {
		t.Fatal(err)
	}
	if id := create(a); id != "3" {
		t.Errorf("id without the mark file = %s, want max+1 = 3", id)
	}
}
