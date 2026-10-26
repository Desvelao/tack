package jsonbackend

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"tack/internal/backend"
	"tack/internal/task"
)

type clocked interface{ SetClock(func() time.Time) }

// formats runs a test for both file types with a document of one task.
var formats = []struct {
	typ, name, one, two string
}{
	{"json", "t.json", `[{"id":"1","title":"a","status":"todo"}]`, `[{"id":"1","title":"a","status":"todo"},{"id":"2","title":"b","status":"todo"}]`},
	{"jsonl", "t.jsonl", `{"id":"1","title":"a","status":"todo"}` + "\n", `{"id":"1","title":"a","status":"todo"}` + "\n" + `{"id":"2","title":"b","status":"todo"}` + "\n"},
}

// touch rewrites p with different content and a modification time in the
// future, so the change is seen even on coarse file system clocks.
func touch(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, f, f); err != nil {
		t.Fatal(err)
	}
}

func openFmt(t *testing.T, i int, extra string) (backend.Backend, string) {
	t.Helper()
	f := formats[i]
	p := tmp(t, f.name, f.one)
	b, err := backend.OpenSpec(spec(t, f.typ, p, extra))
	if err != nil {
		t.Fatal(err)
	}
	return b, p
}

func count(t *testing.T, b backend.Backend) int {
	t.Helper()
	got, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(got)
}

func TestPollOptionParsing(t *testing.T) {
	for _, f := range formats {
		for poll, ok := range map[string]bool{"watch": true, "none": true, "5": true, "5s": true, "250ms": true, "0": false, "-1": false, "soon": false} {
			p := tmp(t, f.name, f.one)
			_, err := backend.OpenSpec(spec(t, f.typ, p, "poll: "+poll))
			if ok && err != nil {
				t.Errorf("%s poll %q: %v", f.typ, poll, err)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "poll")) {
				t.Errorf("%s poll %q: want a poll error, got %v", f.typ, poll, err)
			}
		}
	}
}

func TestPollNoneIgnoresExternalEditsUntilReload(t *testing.T) {
	for i, f := range formats {
		b, p := openFmt(t, i, "poll: none")
		touch(t, p, f.two)
		if ch, err := b.(backend.Watcher).Changed(); ch || err != nil {
			t.Fatalf("%s: Changed = %v, %v; want false", f.typ, ch, err)
		}
		if n := count(t, b); n != 1 {
			t.Fatalf("%s: cached copy expected, got %d tasks", f.typ, n)
		}
		if err := b.(backend.Reloader).Reload(); err != nil {
			t.Fatal(err)
		}
		if n := count(t, b); n != 2 {
			t.Fatalf("%s: after Reload got %d tasks, want 2", f.typ, n)
		}
	}
}

func TestPollNoneWritesStillMergeExternalEdits(t *testing.T) {
	for i, f := range formats {
		b, p := openFmt(t, i, "poll: none")
		touch(t, p, f.two)
		c, err := b.Create(context.Background(), task.Task{Title: "c", Status: "todo"})
		if err != nil || c.ID != "3" {
			t.Fatalf("%s: create = %+v, %v; the outside task must be merged first", f.typ, c, err)
		}
		if n := count(t, b); n != 3 {
			t.Fatalf("%s: %d tasks, want 3", f.typ, n)
		}
	}
}

func TestPollIntervalChecksOncePerPeriod(t *testing.T) {
	for i, f := range formats {
		b, p := openFmt(t, i, "poll: 10s")
		now := time.Now()
		b.(clocked).SetClock(func() time.Time { return now })
		w := b.(backend.Watcher)
		if ch, _ := w.Changed(); ch {
			t.Fatalf("%s: unchanged file reported", f.typ)
		}
		touch(t, p, f.two)
		now = now.Add(5 * time.Second)
		if ch, _ := w.Changed(); ch {
			t.Fatalf("%s: checked again before the interval", f.typ)
		}
		now = now.Add(6 * time.Second)
		if ch, _ := w.Changed(); !ch {
			t.Fatalf("%s: change not noticed after the interval", f.typ)
		}
		if n := count(t, b); n != 2 {
			t.Fatalf("%s: List after Changed got %d tasks, want 2", f.typ, n)
		}
	}
}

func TestPollWatchIsTheDefault(t *testing.T) {
	for i, f := range formats {
		b, p := openFmt(t, i, "")
		touch(t, p, f.two)
		if ch, _ := b.(backend.Watcher).Changed(); !ch {
			t.Fatalf("%s: change not reported by default", f.typ)
		}
		if n := count(t, b); n != 2 {
			t.Fatalf("%s: got %d tasks, want 2", f.typ, n)
		}
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
			b, p := openFmt(t, 0, "")
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
			sameSizeEdit(t, p, `[{"id":"1","title":"a","status":"done"}]`, rename)
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
