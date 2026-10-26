package claim

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"tack/internal/backend"
	"tack/internal/task"
)

// fakeStore keeps tasks in memory and honours a WithExpect guard under its
// lock, like a backend does.
type fakeStore struct {
	mu      sync.Mutex
	tasks   map[string]task.Task
	fields  map[string]bool // fields every backend stores
	updates int
}

func newFake(ts ...task.Task) *fakeStore {
	f := &fakeStore{tasks: map[string]task.Task{}, fields: map[string]bool{"assignee": true, "lease": true}}
	for _, t := range ts {
		f.tasks[t.ID] = t
	}
	return f
}

func (f *fakeStore) Update(ctx context.Context, t task.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if err := backend.CheckExpect(ctx, f.tasks[t.ID]); err != nil {
		return err
	}
	f.tasks[t.ID] = t
	return nil
}

func (f *fakeStore) Stores(_, field string) bool { return f.fields[field] }

func (f *fakeStore) get(id string) task.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[id]
}

func TestTakeRacingClaimsOneWins(t *testing.T) {
	start := task.Task{ID: "1", Status: "todo"}
	f := newFake(start)
	agents := []string{"ann", "bob", "cy", "di"}
	errs := make([]error, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = Take(context.Background(), f, start, Request{Agent: a, Status: "doing"}, now)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, backend.ErrPrecondition):
			t.Errorf("loser got %v, want ErrPrecondition", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d claims won, want 1", wins)
	}
	if got := f.get("1"); got.Assignee == "" || got.Status != "doing" {
		t.Errorf("stored task = %+v", got)
	}
}

func TestTakeOverOfExpiredLeaseLosesToRenewal(t *testing.T) {
	old := task.Task{ID: "1", Assignee: "ann", Lease: "2026-10-18T11:00:00Z"}
	f := newFake(old)
	// Ann renews after bob read the expired task.
	renewed, err := Take(context.Background(), f, old, Request{Agent: "ann", Lease: time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Take(context.Background(), f, old, Request{Agent: "bob", Lease: time.Hour}, now)
	if !errors.Is(err, backend.ErrPrecondition) {
		t.Fatalf("takeover err = %v, want ErrPrecondition", err)
	}
	if got := f.get("1"); got.Assignee != "ann" || got.Lease != renewed.Lease {
		t.Errorf("stored task = %+v, want ann's renewal", got)
	}
	// With the lease still expired in the store, the takeover goes through.
	f = newFake(old)
	if _, err := Take(context.Background(), f, old, Request{Agent: "bob"}, now); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if got := f.get("1"); got.Assignee != "bob" {
		t.Errorf("assignee = %q, want bob", got.Assignee)
	}
}

func TestTakeHeldByAnotherDoesNotWrite(t *testing.T) {
	held := task.Task{ID: "1", Assignee: "ann", Lease: "2026-10-18T12:10:00Z"}
	f := newFake(held)
	if _, err := Take(context.Background(), f, held, Request{Agent: "bob"}, now); !errors.Is(err, backend.ErrPrecondition) {
		t.Fatalf("err = %v, want ErrPrecondition", err)
	}
	if f.updates != 0 {
		t.Errorf("%d updates, want none", f.updates)
	}
}

func TestCheckRefusesBackendWithoutFields(t *testing.T) {
	tk := task.Task{ID: "1", Backend: "csv"}
	f := newFake(tk)
	if err := Check(f, tk, Request{Agent: "ann", Lease: time.Hour}); err != nil {
		t.Fatalf("both fields: %v", err)
	}
	f.fields["lease"] = false
	if err := Check(f, tk, Request{Agent: "ann"}); err != nil {
		t.Errorf("no lease asked: %v", err)
	}
	if err := Check(f, tk, Request{Agent: "ann", Lease: time.Hour}); err == nil {
		t.Error("lease asked but not stored: want an error")
	}
	f.fields["assignee"] = false
	if err := Check(f, tk, Request{Agent: "ann"}); err == nil {
		t.Error("no assignee field: want an error")
	}
}

func TestGiveUnchangedSkipsUpdate(t *testing.T) {
	free := task.Task{ID: "1", Status: "todo"}
	f := newFake(free)
	out, changed, err := Give(context.Background(), f, free, "ann", false, "")
	if err != nil || changed {
		t.Fatalf("Give = %v %v, want unchanged and no error", changed, err)
	}
	if out.ID != "1" || f.updates != 0 {
		t.Errorf("out %+v, updates %d", out, f.updates)
	}
}

func TestGiveClearsClaimAndLosesToChangedClaim(t *testing.T) {
	held := task.Task{ID: "1", Assignee: "ann", Lease: "2026-10-18T12:10:00Z", ClaimedAt: "2026-10-18T11:50:00Z"}
	f := newFake(held)
	out, changed, err := Give(context.Background(), f, held, "ann", false, "todo")
	if err != nil || !changed || out.Assignee != "" {
		t.Fatalf("Give = %+v %v %v", out, changed, err)
	}
	// Somebody else claimed meanwhile: a forced release read earlier must lose.
	f = newFake(task.Task{ID: "1", Assignee: "bob"})
	_, changed, err = Give(context.Background(), f, held, "ann", true, "")
	if !errors.Is(err, backend.ErrPrecondition) || changed {
		t.Errorf("Give = %v %v, want ErrPrecondition", changed, err)
	}
	if got := f.get("1"); got.Assignee != "bob" {
		t.Errorf("stored assignee = %q, want bob", got.Assignee)
	}
}
