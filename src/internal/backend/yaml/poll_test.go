package yamlbackend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/task"
)

type clocked interface{ SetClock(func() time.Time) }

const (
	one = "- {id: 1, title: a, status: todo}\n"
	two = one + "- {id: 2, title: b, status: todo}\n"
)

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

func openFmt(t *testing.T, extra string) (backend.Backend, string) {
	t.Helper()
	b, p := open(t, one, extra)
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
	for poll, ok := range map[string]bool{"watch": true, "none": true, "5": true, "5s": true, "250ms": true, "0": false, "-1": false, "soon": false} {
		p := filepath.Join(t.TempDir(), "t.yaml")
		_ = os.WriteFile(p, []byte(one), 0o644)
		var n yaml.Node
		_ = yaml.Unmarshal([]byte("source: \""+p+"\"\npoll: "+poll+"\n"), &n)
		_, err := backend.OpenSpec(backend.Spec{Type: "yaml", Options: *n.Content[0]})
		if ok && err != nil {
			t.Errorf("poll %q: %v", poll, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "poll")) {
			t.Errorf("poll %q: want a poll error, got %v", poll, err)
		}
	}
}

func TestPollNoneIgnoresExternalEditsUntilReload(t *testing.T) {
	{
		b, p := openFmt(t, "poll: none")
		touch(t, p, two)
		if ch, err := b.(backend.Watcher).Changed(); ch || err != nil {
			t.Fatalf("Changed = %v, %v; want false", ch, err)
		}
		if n := count(t, b); n != 1 {
			t.Fatalf("cached copy expected, got %d tasks", n)
		}
		if err := b.(backend.Reloader).Reload(); err != nil {
			t.Fatal(err)
		}
		if n := count(t, b); n != 2 {
			t.Fatalf("after Reload got %d tasks, want 2", n)
		}
	}
}

func TestPollNoneWritesStillMergeExternalEdits(t *testing.T) {
	{
		b, p := openFmt(t, "poll: none")
		touch(t, p, two)
		c, err := b.Create(context.Background(), task.Task{Title: "c", Status: "todo"})
		if err != nil || c.ID != "3" {
			t.Fatalf("create = %+v, %v; the outside task must be merged first", c, err)
		}
		if n := count(t, b); n != 3 {
			t.Fatalf("%d tasks, want 3", n)
		}
	}
}

func TestPollIntervalChecksOncePerPeriod(t *testing.T) {
	{
		b, p := openFmt(t, "poll: 10s")
		now := time.Now()
		b.(clocked).SetClock(func() time.Time { return now })
		w := b.(backend.Watcher)
		if ch, _ := w.Changed(); ch {
			t.Fatal("unchanged file reported")
		}
		touch(t, p, two)
		now = now.Add(5 * time.Second)
		if ch, _ := w.Changed(); ch {
			t.Fatal("checked again before the interval")
		}
		now = now.Add(6 * time.Second)
		if ch, _ := w.Changed(); !ch {
			t.Fatal("change not noticed after the interval")
		}
		if n := count(t, b); n != 2 {
			t.Fatalf("List after Changed got %d tasks, want 2", n)
		}
	}
}

func TestPollWatchIsTheDefault(t *testing.T) {
	{
		b, p := openFmt(t, "")
		touch(t, p, two)
		if ch, _ := b.(backend.Watcher).Changed(); !ch {
			t.Fatal("change not reported by default")
		}
		if n := count(t, b); n != 2 {
			t.Fatalf("got %d tasks, want 2", n)
		}
	}
}
