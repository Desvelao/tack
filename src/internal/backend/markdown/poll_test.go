package markdownbackend

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

const (
	docOne = "---\ntitle: a\nstatus: todo\n---\n"
	docTwo = "---\ntitle: b\nstatus: todo\n---\n"
)

func one() map[string]string { return map[string]string{"1.md": docOne} }

func count(t *testing.T, b backend.Backend) int {
	t.Helper()
	got, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(got)
}

// put writes dir/name with a modification time in the future.
func put(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, f, f); err != nil {
		t.Fatal(err)
	}
}

func TestPollOptionParsing(t *testing.T) {
	for poll, ok := range map[string]bool{"watch": true, "none": true, "5": true, "5s": true, "250ms": true, "0": false, "-1": false, "soon": false} {
		dir := t.TempDir()
		var n yaml.Node
		_ = yaml.Unmarshal([]byte("source: \""+dir+"\"\npoll: "+poll+"\n"), &n)
		_, err := backend.OpenSpec(backend.Spec{Type: "markdown", Options: *n.Content[0]})
		if ok && err != nil {
			t.Errorf("poll %q: %v", poll, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "poll")) {
			t.Errorf("poll %q: want a poll error, got %v", poll, err)
		}
	}
}

func TestPollNoneIgnoresExternalEditsUntilReload(t *testing.T) {
	b, dir := setup(t, one(), "poll: none")
	put(t, dir, "2.md", docTwo)
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

func TestPollNoneWritesStillMergeExternalEdits(t *testing.T) {
	b, dir := setup(t, one(), "poll: none")
	put(t, dir, "2.md", docTwo)
	c, err := b.Create(context.Background(), task.Task{Title: "c", Status: "todo"})
	if err != nil || c.ID != "3" {
		t.Fatalf("create = %+v, %v; the outside file must be merged first", c, err)
	}
	if n := count(t, b); n != 3 {
		t.Fatalf("%d tasks, want 3", n)
	}
}

func TestPollIntervalChecksOncePerPeriod(t *testing.T) {
	b, dir := setup(t, one(), "poll: 10s")
	now := time.Now()
	b.(*Backend).now = func() time.Time { return now }
	w := b.(backend.Watcher)
	if ch, _ := w.Changed(); ch {
		t.Fatal("unchanged directory reported")
	}
	put(t, dir, "2.md", docTwo)
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

func TestPollWatchIsTheDefaultAndSeesAddEditRemove(t *testing.T) {
	b, dir := setup(t, one(), "")
	w := b.(backend.Watcher)
	step := func(what string, mutate func(), wantTasks int) {
		t.Helper()
		mutate()
		if ch, _ := w.Changed(); !ch {
			t.Fatalf("%s: not reported", what)
		}
		if n := count(t, b); n != wantTasks {
			t.Fatalf("%s: %d tasks, want %d", what, n, wantTasks)
		}
	}
	step("add", func() { put(t, dir, "2.md", docTwo) }, 2)
	step("edit", func() { put(t, dir, "2.md", "---\ntitle: bb\nstatus: done\n---\n") }, 2)
	got, _ := b.List(context.Background())
	if got[1].Title != "bb" || got[1].Status != "done" {
		t.Fatalf("edit not read: %+v", got[1])
	}
	step("remove", func() {
		if err := os.Remove(filepath.Join(dir, "2.md")); err != nil {
			t.Fatal(err)
		}
	}, 1)
}

func TestPollIgnoresFilesThatAreNotTasks(t *testing.T) {
	b, dir := setup(t, one(), "")
	w := b.(backend.Watcher)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".hidden.md"), []byte("x"), 0o644)
	_ = os.Mkdir(filepath.Join(dir, "sub.md"), 0o755)
	if ch, err := w.Changed(); ch || err != nil {
		t.Fatalf("Changed = %v, %v for files that are not tasks", ch, err)
	}
}
