package fleet

import (
	"testing"
	"time"

	"tkan/internal/task"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

func TestBuildStatesAndOrder(t *testing.T) {
	tasks := []task.Task{
		{ID: "1", Title: "free"},
		{ID: "2", Title: "run", Assignee: "claude", Lease: ts(10 * time.Minute), ClaimedAt: ts(-5 * time.Minute)},
		{ID: "3", Title: "stale", Assignee: "codex", Lease: ts(-time.Minute)},
		{ID: "4", Title: "mine", Assignee: "ann"},
		{ID: "5", Title: "leased person", Assignee: "bob", Lease: ts(time.Hour)},
		{ID: "6", Title: "listed forever", Assignee: "Gemini"},
	}
	rows := Build(tasks, []string{"Claude", "codex", "gemini", "aider"}, now)
	var got []string
	for _, r := range rows {
		got = append(got, r.Agent+":"+string(r.State))
	}
	// sorted by state, then name (case-insensitive)
	want := []string{"bob:busy", "Claude:busy", "gemini:busy", "codex:expired", "ann:held", "aider:idle"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	var claude Row
	for _, r := range rows {
		if r.Agent == "Claude" {
			claude = r
		}
	}
	if claude.Elapsed(now) != 5*time.Minute || claude.Left(now) != 10*time.Minute {
		t.Fatalf("elapsed %v left %v", claude.Elapsed(now), claude.Left(now))
	}
}

func TestUnknownStartAndNoLease(t *testing.T) {
	rows := Build([]task.Task{{ID: "1", Assignee: "ann"}}, nil, now)
	if len(rows) != 1 || rows[0].Elapsed(now) != 0 || rows[0].Left(now) != 0 {
		t.Fatalf("%+v", rows)
	}
}

func TestAvatar(t *testing.T) {
	busy := Row{Agent: "Claude", State: Busy}
	custom := map[string][]string{"claude": {"a", "b", "c"}}
	if Avatar(busy, custom, 4) != "b" || Avatar(busy, nil, 1) != busyFrames[1] {
		t.Fatal("busy frames")
	}
	if Avatar(Row{Agent: "claude", State: Idle}, custom, 2) != "a" || Avatar(Row{State: Idle}, nil, 0) != idleGlyph {
		t.Fatal("idle")
	}
	if Avatar(Row{State: Held}, custom, 0) != heldGlyph {
		t.Fatal("held")
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", 45 * time.Second: "45s", 12 * time.Minute: "12m",
		65 * time.Minute: "1h05m", 51 * time.Hour: "2d03h",
	} {
		if got := Duration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
