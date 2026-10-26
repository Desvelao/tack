package claim

import (
	"errors"
	"testing"
	"time"

	"tack/internal/backend"
	"tack/internal/task"
)

var now = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func TestHolder(t *testing.T) {
	for _, c := range []struct {
		name   string
		t      task.Task
		agent  string
		held   bool
		expiry bool
	}{
		{"free", task.Task{}, "", false, false},
		{"assigned for good", task.Task{Assignee: "ann"}, "ann", true, false},
		{"lease running", task.Task{Assignee: "ann", Lease: "2026-10-18T12:30:00Z"}, "ann", true, true},
		{"lease ran out", task.Task{Assignee: "ann", Lease: "2026-10-18T11:59:59Z"}, "ann", false, true},
		{"lease ends now", task.Task{Assignee: "ann", Lease: "2026-10-18T12:00:00Z"}, "ann", false, true},
		{"lease not a time", task.Task{Assignee: "ann", Lease: "soon"}, "ann", true, false},
		{"lease without assignee", task.Task{Lease: "2026-10-18T12:30:00Z"}, "", false, false},
	} {
		agent, until, held := Holder(c.t, now)
		if agent != c.agent || held != c.held || until.IsZero() == c.expiry {
			t.Errorf("%s: Holder = %q %v %v", c.name, agent, until, held)
		}
		if Free(c.t, now) == held {
			t.Errorf("%s: Free disagrees with Holder", c.name)
		}
	}
}

func TestClaim(t *testing.T) {
	r := Request{Agent: "bob", Lease: 30 * time.Minute, Status: "doing"}
	for _, c := range []struct {
		name  string
		t     task.Task
		guard Guard
		taken bool
	}{
		{"free", task.Task{ID: "1", Status: "todo"}, Guard{"assignee", ""}, false},
		{"own claim is renewed", task.Task{ID: "1", Assignee: "bob", Lease: "2026-10-18T12:10:00Z"}, Guard{"assignee", "bob"}, false},
		{"expired lease is taken over", task.Task{ID: "1", Assignee: "ann", Lease: "2026-10-18T11:00:00Z"}, Guard{"lease", "2026-10-18T11:00:00Z"}, false},
		{"held by another", task.Task{ID: "1", Assignee: "ann", Lease: "2026-10-18T12:10:00Z"}, Guard{}, true},
		{"assigned by hand", task.Task{ID: "1", Assignee: "ann"}, Guard{}, true},
	} {
		out, g, err := Claim(c.t, r, now)
		if c.taken {
			if !errors.Is(err, backend.ErrPrecondition) {
				t.Errorf("%s: err %v, want ErrPrecondition", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if g != c.guard {
			t.Errorf("%s: guard %+v, want %+v", c.name, g, c.guard)
		}
		if out.Assignee != "bob" || out.Lease != "2026-10-18T12:30:00Z" || out.Status != "doing" {
			t.Errorf("%s: claimed task %+v", c.name, out)
		}
	}
	out, _, err := Claim(task.Task{Status: "todo", Lease: "stale"}, Request{Agent: " bob "}, now)
	if err != nil || out.Assignee != "bob" || out.Lease != "" || out.Status != "todo" {
		t.Errorf("claim without lease or status: %+v %v", out, err)
	}
	if _, _, err := Claim(task.Task{}, Request{Agent: " "}, now); err == nil {
		t.Error("claim without an agent must fail")
	}
}

func TestRelease(t *testing.T) {
	held := task.Task{ID: "1", Status: "doing", Assignee: "bob", Lease: "2026-10-18T12:30:00Z"}
	out, g, changed, err := Release(held, "bob", false, "todo")
	if err != nil || !changed || g != (Guard{"assignee", "bob"}) || out.Assignee != "" || out.Lease != "" || out.Status != "todo" {
		t.Errorf("own release: %+v %+v %v %v", out, g, changed, err)
	}
	if _, _, _, err := Release(held, "ann", false, ""); !errors.Is(err, backend.ErrPrecondition) {
		t.Errorf("release of another's claim: %v, want ErrPrecondition", err)
	}
	if out, g, changed, err := Release(held, "", true, ""); err != nil || !changed || g.Value != "bob" || out.Assignee != "" || out.Status != "doing" {
		t.Errorf("forced release: %+v %+v %v %v", out, g, changed, err)
	}
	if _, _, changed, err := Release(task.Task{ID: "1", Status: "todo"}, "bob", false, ""); err != nil || changed {
		t.Errorf("release of a free task: changed %v err %v, want a no-op", changed, err)
	}
	if _, _, _, err := Release(task.Task{ID: "1", Status: "todo"}, "bob", false, "done"); !errors.Is(err, backend.ErrPrecondition) {
		t.Errorf("move of a free task by release: %v, want ErrPrecondition", err)
	}
}

func TestReleaseClearsOrphanLeftovers(t *testing.T) {
	for name, orphan := range map[string]task.Task{
		"lease only":      {ID: "1", Status: "doing", Lease: "2026-10-18T12:30:00Z"},
		"claimed_at only": {ID: "1", Status: "doing", ClaimedAt: "2026-10-18T12:00:00Z"},
	} {
		out, g, changed, err := Release(orphan, "bob", false, "")
		if err != nil || !changed || g != (Guard{"assignee", ""}) || out.Lease != "" || out.ClaimedAt != "" {
			t.Errorf("%s: %+v %+v %v %v", name, out, g, changed, err)
		}
	}
}

func TestClaimedAtIsSetKeptOnRenewalAndCleared(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	got, _, err := Claim(task.Task{ID: "1"}, Request{Agent: "ann", Lease: time.Hour}, t0)
	if err != nil || got.ClaimedAt != "2026-10-19T12:00:00Z" {
		t.Fatalf("claim: %q %v", got.ClaimedAt, err)
	}
	renewed, _, err := Claim(got, Request{Agent: "ann", Lease: time.Hour}, t0.Add(10*time.Minute))
	if err != nil || renewed.ClaimedAt != got.ClaimedAt {
		t.Fatalf("renewal moved the start: %q %v", renewed.ClaimedAt, err)
	}
	taken, _, err := Claim(got, Request{Agent: "bob"}, t0.Add(2*time.Hour)) // ann's lease ran out
	if err != nil || taken.ClaimedAt != "2026-10-19T14:00:00Z" {
		t.Fatalf("takeover: %q %v", taken.ClaimedAt, err)
	}
	out, _, _, err := Release(taken, "bob", false, "")
	if err != nil || out.ClaimedAt != "" || out.Assignee != "" {
		t.Fatalf("release: %+v %v", out, err)
	}
}

func TestAgentNamesIgnoreCase(t *testing.T) {
	held := task.Task{ID: "1", Status: "doing", Assignee: "claude", Lease: "2026-10-18T12:10:00Z", ClaimedAt: "2026-10-18T11:00:00Z"}
	out, g, err := Claim(held, Request{Agent: "Claude", Lease: 30 * time.Minute}, now)
	if err != nil || g != (Guard{"assignee", "claude"}) || out.Assignee != "claude" || out.ClaimedAt != held.ClaimedAt {
		t.Errorf("renewal as Claude: %+v %+v %v, want the stored spelling and start kept", out, g, err)
	}
	if _, _, err := Claim(held, Request{Agent: "ann"}, now); !errors.Is(err, backend.ErrPrecondition) {
		t.Errorf("claim by another agent: %v, want ErrPrecondition", err)
	}
	if out, _, changed, err := Release(held, "CLAUDE", false, ""); err != nil || !changed || out.Assignee != "" {
		t.Errorf("release as CLAUDE: %+v %v %v", out, changed, err)
	}
	if _, _, _, err := Release(held, "ann", false, ""); !errors.Is(err, backend.ErrPrecondition) {
		t.Errorf("release by another agent: %v, want ErrPrecondition", err)
	}
}
