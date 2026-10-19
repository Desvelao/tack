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
	"tkan/internal/backend/backendtest"
	"tkan/internal/task"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func specFor(t *testing.T, p, extra string) backend.Spec {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: \""+p+"\"\n"+extra), &n); err != nil {
		t.Fatal(err)
	}
	return backend.Spec{Type: "yaml", Options: *n.Content[0]}
}

func open(t *testing.T, content, extra string) (backend.Backend, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: \""+p+"\"\n"+extra), &n); err != nil {
		t.Fatal(err)
	}
	b, err := backend.OpenSpec(backend.Spec{Type: "yaml", Options: *n.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	return b, p
}

const sample = `# my backlog
tasks:
  - id: 1
    title: First
    status: todo
    due: 2026-03-04
    labels: [a, b]
    owner: ann
    estimate: 3
    deps: {x: [1, 2]}
  - id: 2
    title: |
      Multi
      line
    status: done
`

func TestRoundTrip(t *testing.T) {
	b, p := open(t, sample, "")
	ctx := context.Background()
	got, err := b.List(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].Due != "2026-03-04" || got[0].Labels != "a,b" || got[0].Extra["estimate"] != "3" || got[0].Extra["owner"] != "ann" {
		t.Fatalf("%+v", got[0])
	}
	if got[1].Title != "Multi\nline\n" {
		t.Fatalf("title %q", got[1].Title)
	}
	got[0].Status = "doing"
	if err := b.Update(ctx, got[0]); err != nil {
		t.Fatal(err)
	}
	c, err := b.Create(ctx, task.Task{Title: "new", Status: "todo", Due: "2026-05-06"})
	if err != nil || c.ID != "3" {
		t.Fatalf("%+v %v", c, err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{"# my backlog", "tasks:", "status: doing", "estimate: 3", `due: "2026-03-04"`, `due: "2026-05-06"`, "id: 3"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// Reading it back gives the same tasks (dates stay strings, extras survive).
	b2, _ := open(t, s, "")
	again, err := b2.List(ctx)
	if err != nil || len(again) != 3 || again[0].Due != "2026-03-04" || again[0].Extra["deps"] != `{"x":[1,2]}` || again[1].Title != "Multi\nline\n" {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestTopLevelListEmptyAndDelete(t *testing.T) {
	b, p := open(t, "", "")
	ctx := context.Background()
	c, err := b.Create(ctx, task.Task{Title: "a", Status: "s"})
	if err != nil || c.ID != "1" {
		t.Fatalf("%+v %v", c, err)
	}
	data, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(data), "- id: 1") {
		t.Fatalf("top-level list expected:\n%s", data)
	}
	if err := b.Delete(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("got %q", data)
	}
	if err := b.Delete(ctx, "1"); err == nil {
		t.Fatal("delete of a missing id succeeded")
	}
}

func TestErrors(t *testing.T) {
	for name, content := range map[string]string{
		"scalar":   "hello",
		"no tasks": "a: 1\n",
		"dup":      "- {id: 1, title: a, status: s}\n- {id: 1, title: b, status: s}\n",
		"item":     "- 5\n",
	} {
		p := filepath.Join(t.TempDir(), "t.yaml")
		_ = os.WriteFile(p, []byte(content), 0o644)
		var n yaml.Node
		_ = yaml.Unmarshal([]byte("source: \""+p+"\""), &n)
		if _, err := backend.OpenSpec(backend.Spec{Type: "yaml", Options: *n.Content[0]}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExampleAndNoExtensionClaim(t *testing.T) {
	if got := backend.TypeForLocation("tkan.yaml"); got != "" {
		t.Fatalf(".yaml must not be claimed, got %q", got)
	}
	p := filepath.Join(t.TempDir(), "t.yaml")
	_ = os.WriteFile(p, []byte("- {key: 1, summary: a, state: Open}\n"), 0o644)
	ex, err := backend.ExampleFor("yaml", p)
	if err != nil || !strings.Contains(ex.Section, "id: key") || len(ex.Statuses) != 1 {
		t.Fatalf("%+v %v", ex, err)
	}
}

func TestStructuredLogicalFieldKeepsItsType(t *testing.T) {
	b, p := open(t, "- id: 1\n  title: {a: 1}\n  status: todo\n  due: [2026, 1, 2]\n- id: 2\n  title: plain\n  status: todo\n", "")
	ctx := context.Background()
	got, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got[0].Status = "doing"
	if err := b.Update(ctx, got[0]); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	var recs []map[string]any
	if err := yaml.Unmarshal(data, &recs); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if _, ok := recs[0]["title"].(map[string]any); !ok {
		t.Errorf("title is %#v, want a mapping:\n%s", recs[0]["title"], data)
	}
	if _, ok := recs[0]["due"].([]any); !ok {
		t.Errorf("due is %#v, want a list:\n%s", recs[0]["due"], data)
	}
	// A string the user typed that looks like a mapping stays a string.
	got, _ = b.List(ctx)
	got[1].Title = `{"b":2}`
	if err := b.Update(ctx, got[1]); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	recs = nil
	_ = yaml.Unmarshal(data, &recs)
	if s, ok := recs[1]["title"].(string); !ok || s != `{"b":2}` {
		t.Errorf("title is %#v, want the string:\n%s", recs[1]["title"], data)
	}
}

func TestPatchKeepsAConcurrentChangeToAnotherField(t *testing.T) {
	reader, p := open(t, sample, "")
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: \""+p+"\"\n"), &n); err != nil {
		t.Fatal(err)
	}
	writer, err := backend.OpenSpec(backend.Spec{Type: "yaml", Options: *n.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	backendtest.CheckPatch(t, reader, writer, func() {
		future := time.Now().Add(time.Duration(time.Now().UnixNano()%1000+2) * time.Second)
		_ = os.Chtimes(p, future, future)
	})
}

func TestConcurrentWriters(t *testing.T) {
	p := writeTemp(t, "[]\n")
	open := func() backend.Backend {
		b, err := backend.OpenSpec(specFor(t, p, ""))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	backendtest.CheckConcurrentWriters(t, open, filepath.Dir(p), 4, 12)
}

func TestIDsNotReused(t *testing.T) {
	p := writeTemp(t, "[]\n")
	open := func() backend.Backend {
		b, err := backend.OpenSpec(specFor(t, p, ""))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	backendtest.CheckIDsNotReused(t, open, p+".nextid")
}
