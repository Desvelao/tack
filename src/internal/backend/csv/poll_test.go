package csvbackend

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"tack/internal/backend"
	"tack/internal/task"
)

const pollCSV = "id,title,status\n1,A,pending\n"

// touch rewrites the file with a different size and a clearly later mtime.
func touch(t *testing.T, p, content string, ahead time.Duration) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(ahead)
	_ = os.Chtimes(p, later, later)
}

func TestPollOptionParsing(t *testing.T) {
	ok := map[string]pollMode{
		"":            {},
		"poll: watch": {},
		"poll: WATCH": {},
		"poll: none":  {Kind: pollNone},
		"poll: 5":     {Kind: pollEvery, Interval: 5 * time.Second},
		`poll: "5"`:   {Kind: pollEvery, Interval: 5 * time.Second},
		"poll: 5s":    {Kind: pollEvery, Interval: 5 * time.Second},
		"poll: 1m":    {Kind: pollEvery, Interval: time.Minute},
	}
	for y, want := range ok {
		b, err := Open(csvOpts(t, write(t, pollCSV), y))
		if err != nil {
			t.Errorf("%q: %v", y, err)
			continue
		}
		if got := b.(*Backend).c.sch.poll; got != want {
			t.Errorf("%q: %+v, want %+v", y, got, want)
		}
	}
	for _, y := range []string{"poll: 0", "poll: -1", "poll: fast", "poll: 0s", "poll: [1]", "poll: 1.5"} {
		if _, err := Open(csvOpts(t, write(t, pollCSV), y)); err == nil || !strings.Contains(err.Error(), "poll must be") {
			t.Errorf("%q: err = %v", y, err)
		}
	}
}

func TestPollNoneIgnoresExternalEditsUntilReload(t *testing.T) {
	p := write(t, pollCSV)
	b, _ := Open(csvOpts(t, p, "poll: none"))
	ctx := context.Background()
	touch(t, p, pollCSV+"2,B,pending\n", time.Hour)
	if ch, _ := b.(backend.Watcher).Changed(); ch {
		t.Fatal("Changed must be false under poll: none")
	}
	if ts, _ := b.List(ctx); len(ts) != 1 {
		t.Fatalf("List saw the external edit: %d tasks", len(ts))
	}
	if err := b.(backend.Reloader).Reload(); err != nil {
		t.Fatal(err)
	}
	if ts, _ := b.List(ctx); len(ts) != 2 {
		t.Fatalf("Reload did not pick up the edit: %d tasks", len(ts))
	}
}

func TestPollNoneWritesStillMergeExternalEdits(t *testing.T) {
	p := write(t, pollCSV)
	b, _ := Open(csvOpts(t, p, "poll: none"))
	ctx := context.Background()
	touch(t, p, pollCSV+"2,B,pending\n", time.Hour)
	if _, err := b.Create(ctx, task.Task{Title: "C", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "2,B,pending") || !strings.Contains(string(data), "3,C,pending") {
		t.Fatalf("external edit lost:\n%s", data)
	}
	if ts, _ := b.List(ctx); len(ts) != 3 {
		t.Fatalf("%d tasks, want 3", len(ts))
	}
}

func TestPollIntervalChecksOncePerPeriod(t *testing.T) {
	p := write(t, pollCSV)
	b, _ := Open(csvOpts(t, p, "poll: 10"))
	be := b.(*Backend)
	clock := time.Now()
	be.SetClock(func() time.Time { return clock })
	w := b.(backend.Watcher)
	ctx := context.Background()

	if ch, _ := w.Changed(); ch { // first check: file unchanged
		t.Fatal("unchanged file reported as changed")
	}
	touch(t, p, pollCSV+"2,B,pending\n", time.Hour)
	clock = clock.Add(5 * time.Second)
	if ch, _ := w.Changed(); ch {
		t.Fatal("checked again before the interval elapsed")
	}
	if ts, _ := b.List(ctx); len(ts) != 1 {
		t.Fatalf("List re-read before a change was reported: %d", len(ts))
	}
	clock = clock.Add(6 * time.Second)
	if ch, _ := w.Changed(); !ch {
		t.Fatal("change not reported after the interval")
	}
	if ts, _ := b.List(ctx); len(ts) != 2 {
		t.Fatalf("List did not re-read after Changed: %d", len(ts))
	}
	clock = clock.Add(11 * time.Second)
	if ch, _ := w.Changed(); ch {
		t.Fatal("no new change, yet reported")
	}
}

func TestPollWatchIsTheDefault(t *testing.T) {
	p := write(t, pollCSV)
	b, _ := Open(mk(p))
	touch(t, p, pollCSV+"2,B,pending\n", time.Hour)
	if ts, _ := b.List(context.Background()); len(ts) != 2 {
		t.Fatalf("watch mode must re-read on List: %d", len(ts))
	}
}

// sameSizeEdit rewrites p with data of identical size and the previous
// modification time, either by rename-replace (new inode) or in place (new ctime).
func sameSizeEdit(t *testing.T, p, data string, rename bool) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if rename {
		tmp := p + ".new"
		if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestChangedSeesSameSizeEditWithRestoredMtime(t *testing.T) {
	for _, rename := range []bool{true, false} {
		for _, age := range []time.Duration{0, -time.Hour} { // fresh (hashed) and old file
			p := write(t, "id,title,status\n1,A,todo\n")
			b, err := Open(csvOpts(t, p, ""))
			if err != nil {
				t.Fatal(err)
			}
			if age != 0 {
				old := time.Now().Add(age)
				if err := os.Chtimes(p, old, old); err != nil {
					t.Fatal(err)
				}
			}
			w := b.(backend.Watcher)
			if _, err := b.List(context.Background()); err != nil {
				t.Fatal(err)
			}
			sameSizeEdit(t, p, "id,title,status\n1,A,done\n", rename)
			if ch, err := w.Changed(); !ch || err != nil {
				t.Fatalf("rename=%v age=%v: Changed = %v, %v; want true", rename, age, ch, err)
			}
			got, err := b.List(context.Background())
			if err != nil || len(got) != 1 || got[0].Status != "done" {
				t.Fatalf("rename=%v age=%v: List = %+v, %v; want status done", rename, age, got, err)
			}
		}
	}
}
