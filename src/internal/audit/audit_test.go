package audit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryLog(t *testing.T) {
	l, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	l.Record(Entry{Op: OpCreate, ID: "1", Details: "x"})
	es, err := l.Entries()
	if err != nil || len(es) != 1 || es[0].Result != ResultOK || es[0].Session != l.Session() || es[0].Source != "cli" {
		t.Fatalf("%v %+v", err, es)
	}
}

func TestUnsupportedExtension(t *testing.T) {
	if _, err := New("audit.log"); err == nil {
		t.Fatal("want error")
	}
}

func roundTrip(t *testing.T, name string) {
	p := filepath.Join(t.TempDir(), name)
	a, _ := New(p)
	a.session = "20260101T100000-1"
	a.SetSource("tui")
	a.Record(Entry{Op: OpDelete, Backend: "b", ID: "3", Details: `has "quotes", commas`})
	a.Record(Entry{Op: OpReload, Result: ResultError, Details: "boom\nline2"})
	b, _ := New(p)
	b.session = "20260102T100000-2"
	b.Record(Entry{Op: OpUpdate, ID: "1"})
	es, err := b.Entries()
	if err != nil || len(es) != 3 {
		t.Fatalf("%v %d", err, len(es))
	}
	if es[0].Details != `has "quotes", commas` || es[0].Source != "tui" || es[1].Details != "boom\nline2" || es[1].Result != ResultError {
		t.Fatalf("%+v", es)
	}
	s := b.SessionsOf(es)
	if s.Kind(a.session) != "previous" || s.Label(b.session) != "this session" || s.Label(a.session) != "previous session" {
		t.Fatalf("%+v", s)
	}
}

func TestJSONLRoundTrip(t *testing.T) { roundTrip(t, "a.jsonl") }
func TestCSVRoundTrip(t *testing.T)   { roundTrip(t, "a.CSV") }

func TestCorruptLinesSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	os.WriteFile(p, []byte("not json\n{\"time\":\"2026-01-01T00:00:00Z\",\"session\":\"s\",\"op\":\"create\",\"result\":\"ok\"}\n{broken\n"), 0o644)
	l, _ := New(p)
	es, err := l.Entries()
	if err != nil || len(es) != 1 {
		t.Fatalf("%v %v", err, es)
	}
}

func TestOtherRunsAppearWhileOpen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	tui, _ := New(p)
	cli, _ := New(p)
	cli.session += "x"
	if es, _ := tui.Entries(); len(es) != 0 {
		t.Fatal("want none")
	}
	cli.Record(Entry{Op: OpCreate})
	if es, _ := tui.Entries(); len(es) != 1 {
		t.Fatalf("got %d", len(es))
	}
}

func TestConcurrentAppends(t *testing.T) {
	for _, name := range []string{"c.jsonl", "c.csv"} {
		p := filepath.Join(t.TempDir(), name)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			l, _ := New(p)
			l.session = "s" + string(rune('a'+i))
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 25; j++ {
					l.Record(Entry{Op: OpUpdate, Details: strings.Repeat("d", 50)})
				}
			}()
		}
		wg.Wait()
		r, _ := New(p)
		if es, _ := r.Entries(); len(es) != 100 {
			t.Fatalf("%s: %d entries, want 100", name, len(es))
		}
	}
}

func TestWriteFailureIsReportedOnce(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "no", "dir", "a.jsonl"))
	l.Record(Entry{Op: OpCreate})
	if l.Err() == nil || l.Err() != nil {
		t.Fatal("want the error once")
	}
	if es, _ := l.Entries(); len(es) != 1 {
		t.Fatal("entry must stay in memory")
	}
}

func TestFilters(t *testing.T) {
	l, _ := New("")
	day := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	l.Record(Entry{Time: day, Op: OpDelete, ID: "3", Details: "removed Login page"})
	l.Record(Entry{Time: day.AddDate(0, 0, 20), Op: OpUpdate, ID: "4", Details: "status=done"})
	l.Record(Entry{Time: day.AddDate(0, 0, 20), Op: OpReload, Result: ResultError, Details: "bad csv"})
	es, _ := l.Entries()
	s := l.SessionsOf(es)
	n := func(src string) int {
		q, err := Parse(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		return len(s.Filter(es, q))
	}
	for src, want := range map[string]int{
		"": 3, "op=delete": 1, "op:update OR op:reload": 2, "date>=2026-10-10": 2, "date<=2026-10-05": 1,
		"login": 1, "done": 1, "2026-10-05": 1, "result=error": 1, "session=current": 3, "-login": 2,
	} {
		if got := n(src); got != want {
			t.Errorf("%q: %d, want %d", src, got, want)
		}
	}
	if _, err := Parse("nope=1"); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestHookOpFilters(t *testing.T) {
	l, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	l.Record(Entry{Op: OpRun, ID: "1"})
	l.Record(Entry{Op: HookOp("task.moved"), ID: "1"})
	l.Record(Entry{Op: HookOp("reload.fail")})
	es, _ := l.Entries()
	sess := l.SessionsOf(es)
	for q, want := range map[string]int{"op=run": 1, "op:hook": 2, "op=hook:task.moved": 1, "op:task.moved": 1} {
		parsed, err := Parse(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := len(sess.Filter(es, parsed)); got != want {
			t.Errorf("%s: %d entries, want %d", q, got, want)
		}
	}
}

func TestFileMode0600AndRotation(t *testing.T) {
	for _, name := range []string{"r.jsonl", "r.csv"} {
		p := filepath.Join(t.TempDir(), name)
		l, _ := New(p)
		l.SetMaxSize(1)
		for i := 0; i < 40; i++ {
			l.Record(Entry{Op: OpUpdate, Details: strings.Repeat("d", 100)})
		}
		if err := l.Err(); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", name, st.Mode().Perm())
		}
		if st.Size() > 2<<10 {
			t.Errorf("%s: %d bytes, rotation did not cap it", name, st.Size())
		}
		if _, err := os.Stat(p + ".1"); err != nil {
			t.Errorf("%s: no rotated file: %v", name, err)
		}
		if name == "r.csv" {
			b, _ := os.ReadFile(p)
			if !strings.HasPrefix(string(b), "time,session") {
				t.Errorf("rotated csv lacks its header: %q", b[:20])
			}
		}
	}
}
