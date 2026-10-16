package markdownbackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/backend/filestore"
	"tkan/internal/task"
)

func setup(t *testing.T, files map[string]string, extra string) (backend.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: \""+dir+"\"\n"+extra), &n); err != nil {
		t.Fatal(err)
	}
	b, err := backend.OpenSpec(backend.Spec{Type: "markdown", Options: *n.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	return b, dir
}

var sample = map[string]string{
	"2.md":     "---\ntitle: Second\nstatus: done\n---\n\nbody two\n",
	"10.md":    "---\ntitle: Tenth\nstatus: todo\nlabels: [a, b]\nowner: ann\nestimate: 3\n---\n\nLine one\n\nLine two\n",
	"x.md":     "just text, no frontmatter\n",
	"skip.txt": "not a task",
}

func TestReadOrderFieldsAndExtras(t *testing.T) {
	b, _ := setup(t, sample, "")
	got, err := b.List(context.Background())
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].ID != "2" || got[1].ID != "10" || got[2].ID != "x" {
		t.Fatalf("order: %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[1].Labels != "a,b" || got[1].Description != "Line one\n\nLine two" || got[1].Extra["owner"] != "ann" || got[1].Extra["estimate"] != "3" {
		t.Fatalf("%+v", got[1])
	}
	if got[2].Title != "x" || got[2].Description != "just text, no frontmatter" {
		t.Fatalf("%+v", got[2])
	}
}

func TestWriteOperations(t *testing.T) {
	b, dir := setup(t, sample, "")
	ctx := context.Background()
	got, _ := b.List(ctx)
	upd := got[1]
	upd.Status = "doing"
	upd.Description = "new body"
	if err := b.Update(ctx, upd); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "10.md"))
	s := string(data)
	for _, want := range []string{"---\n", "status: doing", "estimate: 3", "owner: ann", "\nnew body\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "id:") || strings.Contains(s, "description:") {
		t.Errorf("id/description leaked into frontmatter:\n%s", s)
	}
	c, err := b.Create(ctx, task.Task{Title: "New", Status: "todo", Priority: "high"})
	if err != nil || c.ID != "11" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "11.md")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2.md")); !os.IsNotExist(err) {
		t.Fatal("2.md still there")
	}
	if err := b.Delete(ctx, "../etc"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := b.Update(ctx, task.Task{ID: "nope", Title: "x"}); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	again, _ := b.List(ctx)
	if len(again) != 3 || again[0].Description != "new body" || again[0].Status != "doing" {
		t.Fatalf("%+v", again)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".tkan-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestChangedAndExternalEdits(t *testing.T) {
	b, dir := setup(t, sample, "")
	ctx := context.Background()
	w := b.(backend.Watcher)
	if ch, _ := w.Changed(); ch {
		t.Fatal("changed after open")
	}
	if _, err := b.Create(ctx, task.Task{Title: "n", Status: "s"}); err != nil {
		t.Fatal(err)
	}
	if ch, _ := w.Changed(); ch {
		t.Fatal("own write reported as change")
	}
	if err := os.WriteFile(filepath.Join(dir, "50.md"), []byte("---\ntitle: ext\nstatus: s\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ch, _ := w.Changed(); !ch {
		t.Fatal("new file not noticed")
	}
	c, err := b.Create(ctx, task.Task{Title: "after", Status: "s"})
	if err != nil || c.ID != "51" {
		t.Fatalf("%+v %v (external file must be merged before assigning the id)", c, err)
	}
	if err := os.Remove(filepath.Join(dir, "50.md")); err != nil {
		t.Fatal(err)
	}
	if ch, _ := w.Changed(); !ch {
		t.Fatal("removal not noticed")
	}
}

func TestBadFrontmatterFailsWithFileName(t *testing.T) {
	_, err := backend.OpenSpec(func() backend.Spec {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "1.md"), []byte("---\ntitle: [oops\n---\n"), 0o644)
		var n yaml.Node
		_ = yaml.Unmarshal([]byte("source: \""+dir+"\""), &n)
		return backend.Spec{Type: "markdown", Options: *n.Content[0]}
	}())
	if err == nil || !strings.Contains(err.Error(), "1.md") {
		t.Fatalf("got %v", err)
	}
}

func TestOptionsErrors(t *testing.T) {
	mk := func(src string) error {
		var n yaml.Node
		_ = yaml.Unmarshal([]byte(src), &n)
		_, err := backend.OpenSpec(backend.Spec{Type: "markdown", Options: *n.Content[0]})
		return err
	}
	dir := t.TempDir()
	if mk("poll: none") == nil {
		t.Error("missing source accepted")
	}
	if mk("source: \""+dir+"/nope\"") == nil {
		t.Error("missing directory accepted")
	}
	f := filepath.Join(dir, "f.md")
	_ = os.WriteFile(f, nil, 0o644)
	if mk("source: \""+f+"\"") == nil {
		t.Error("file accepted as directory")
	}
	if mk("source: \""+dir+"\"\nfields: {id: key}") == nil {
		t.Error("mapping id accepted")
	}
}

func TestSplitAndExample(t *testing.T) {
	for in, want := range map[string][2]string{
		"---\na: 1\n---\nbody":     {"a: 1\n", "body"},
		"---\r\na: 1\r\n---\r\nb":  {"a: 1\n", "b"},
		"---\na: 1\n---":           {"a: 1\n", ""},
		"no front":                 {"", "no front"},
		"---\nnever closed\nstuff": {"", "---\nnever closed\nstuff"},
	} {
		f, b := split([]byte(in))
		if f != want[0] || b != want[1] {
			t.Errorf("%q -> %q %q", in, f, b)
		}
	}
	_, dir := setup(t, map[string]string{"1.md": "---\nsummary: a\nstate: Open\nowner: z\n---\n"}, "")
	ex, err := backend.ExampleFor("markdown", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Section, "title: summary") || !strings.Contains(ex.Section, "status: state") || strings.Contains(ex.Section, "id:") {
		t.Fatalf("section:\n%s", ex.Section)
	}
	for _, n := range ex.Notes {
		if strings.Contains(n, `"id"`) {
			t.Errorf("note about id: %s", n)
		}
	}
	_ = filestore.LockTimeout
}
