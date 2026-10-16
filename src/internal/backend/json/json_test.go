package jsonbackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/task"
)

func spec(t *testing.T, typ, path, extra string) backend.Spec {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: \""+path+"\"\n"+extra), &n); err != nil {
		t.Fatal(err)
	}
	return backend.Spec{Type: typ, Options: *n.Content[0]}
}

func tmp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openJSON(t *testing.T, path, extra string) backend.Backend {
	t.Helper()
	b, err := backend.OpenSpec(spec(t, "json", path, extra))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const sample = `[
  {"id": 1, "title": "First", "status": "todo", "labels": ["a", "b"], "owner": "ann", "estimate": 3, "meta": {"k": [1, 2]}},
  {"id": 2, "title": "Multi\nline", "status": "done", "due": "2026-01-02"}
]
`

func TestRoundTripKeepsExtrasAndTypes(t *testing.T) {
	p := tmp(t, "t.json", sample)
	b := openJSON(t, p, "")
	ctx := context.Background()
	got, err := b.List(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %v %v", got, err)
	}
	if got[0].ID != "1" || got[0].Labels != "a,b" || got[0].Extra["owner"] != "ann" || got[0].Extra["estimate"] != "3" {
		t.Fatalf("task 0: %+v", got[0])
	}
	if got[1].Title != "Multi\nline" {
		t.Fatalf("title %q", got[1].Title)
	}
	if err := b.Update(ctx, func() task.Task { x := got[0]; x.Status = "doing"; return x }()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{`"id": 1`, `"estimate": 3`, `"k"`, `"labels": [`, `"status": "doing"`, `"title": "Multi\nline"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"owner": 3`) || !strings.HasSuffix(s, "]\n") {
		t.Errorf("bad output:\n%s", s)
	}
}

func TestCreateDeleteAndWrapperObject(t *testing.T) {
	p := tmp(t, "t.json", `{"version": 2, "tasks": [{"id":"5","title":"x","status":"todo"}]}`)
	b := openJSON(t, p, "")
	ctx := context.Background()
	c, err := b.Create(ctx, task.Task{Title: "new", Status: "todo", Labels: "x,y"})
	if err != nil || c.ID != "6" {
		t.Fatalf("create: %+v %v", c, err)
	}
	if _, err := b.Create(ctx, task.Task{Status: "todo"}); err == nil {
		t.Fatal("empty title accepted")
	}
	if err := b.Delete(ctx, "5"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "99"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), `"version": 2`) || !strings.Contains(string(data), `"tasks"`) {
		t.Fatalf("wrapper lost:\n%s", data)
	}
	if entries, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".tkan-*")); len(entries) != 0 {
		t.Fatalf("temp files left: %v", entries)
	}
}

func TestExternalEditMergedAndBadFileKeepsState(t *testing.T) {
	p := tmp(t, "t.json", `[{"id":"1","title":"a","status":"todo"}]`)
	b := openJSON(t, p, "")
	ctx := context.Background()
	if err := os.WriteFile(p, []byte(`[{"id":"1","title":"a","status":"todo"},{"id":"2","title":"b","status":"todo"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(ctx, task.Task{Title: "c", Status: "todo"}); err != nil {
		t.Fatal(err)
	}
	got, _ := b.List(ctx)
	if len(got) != 3 || got[2].ID != "3" {
		t.Fatalf("merge: %+v", got)
	}
	if err := os.WriteFile(p, []byte(`[{"id":"1","title":"a","status":"todo"},{"id":"1","title":"dup","status":"todo"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.List(ctx); err == nil || !strings.Contains(err.Error(), "repeats id") {
		t.Fatalf("want duplicate error, got %v", err)
	}
	if st := b.Statuses(); len(st) != 1 || st[0] != "todo" {
		t.Fatalf("statuses %v", st)
	}
}

func TestChangedDetectsOutsideWrite(t *testing.T) {
	p := tmp(t, "t.json", `[]`)
	b := openJSON(t, p, "")
	w := b.(backend.Watcher)
	if ch, _ := w.Changed(); ch {
		t.Fatal("changed right after open")
	}
	if _, err := b.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err != nil {
		t.Fatal(err)
	}
	if ch, _ := w.Changed(); ch {
		t.Fatal("own write reported as change")
	}
	if err := os.WriteFile(p, []byte(`[{"id":"9","title":"outside","status":"s"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ch, _ := w.Changed(); !ch {
		t.Fatal("outside write not noticed")
	}
}

func TestFieldsRenameAndErrors(t *testing.T) {
	p := tmp(t, "t.json", `[{"key":"A-1","summary":"hi","state":"open","title":"other"}]`)
	b := openJSON(t, p, "fields: {id: key, title: summary, status: state}")
	got, _ := b.List(context.Background())
	if got[0].ID != "A-1" || got[0].Title != "hi" || got[0].Status != "open" || got[0].Extra["title"] != "other" {
		t.Fatalf("%+v", got[0])
	}
	if _, err := backend.OpenSpec(spec(t, "json", p, "fields: {nope: x}")); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := backend.OpenSpec(spec(t, "json", tmp(t, "b.json", `{"x":1}`), "")); err == nil {
		t.Fatal("object without tasks accepted")
	}
	if _, err := backend.OpenSpec(spec(t, "json", tmp(t, "c.json", `[1]`), "")); err == nil {
		t.Fatal("non-object task accepted")
	}
}

func TestJSONLines(t *testing.T) {
	p := tmp(t, "t.jsonl", "{\"id\":\"1\",\"title\":\"a\",\"status\":\"todo\"}\n\n{\"id\":\"2\",\"title\":\"b\",\"status\":\"done\",\"x\":true}\n")
	b, err := backend.OpenSpec(spec(t, "jsonl", p, ""))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := b.Create(ctx, task.Task{Title: "c", Status: "todo"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], `"x":true`) || !strings.Contains(lines[2], `"id":"3"`) {
		t.Fatalf("lines:\n%s", data)
	}
	if _, err := backend.OpenSpec(spec(t, "jsonl", tmp(t, "bad.jsonl", "{\"id\":\n"), "")); err == nil {
		t.Fatal("bad line accepted")
	}
}

func TestEmptyFileAndRegistry(t *testing.T) {
	p := tmp(t, "e.json", "")
	b := openJSON(t, p, "indent: 0")
	if _, err := b.Create(context.Background(), task.Task{Title: "a", Status: "s"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != `[{"id":"1","title":"a","status":"s"}]`+"\n" {
		t.Fatalf("got %q", data)
	}
	for loc, want := range map[string]string{"a.JSON": "json", "a.jsonl": "jsonl", "a.ndjson": "jsonl"} {
		if got := backend.TypeForLocation(loc); got != want {
			t.Errorf("%s -> %q, want %q", loc, got, want)
		}
	}
}

func TestExample(t *testing.T) {
	p := tmp(t, "t.json", `[{"key":"1","summary":"a","state":"Todo","owner":"x"},{"key":"2","summary":"b","state":"Done"}]`)
	ex, err := backend.ExampleFor("json", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id: key", "title: summary", "status: state", "assignee: owner"} {
		if !strings.Contains(ex.Section, want) {
			t.Errorf("section lacks %q:\n%s", want, ex.Section)
		}
	}
	if len(ex.Statuses) != 2 || ex.Statuses[0] != "Todo" {
		t.Fatalf("statuses %v", ex.Statuses)
	}
}
