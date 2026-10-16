package yamlbackend

import (
	"context"
	"os"
	"strings"
	"testing"

	"tkan/internal/backend"
	"tkan/internal/task"
)

const item = "{id: 1, title: a, status: todo}"

func TestPathLocations(t *testing.T) {
	for _, c := range []struct{ name, path, doc string }{
		{"top-level list", "", "- " + item + "\n"},
		{"default tasks key", "", "tasks:\n  - " + item + "\n"},
		{"explicit one key", "tasks", "keep: 1\ntasks:\n  - " + item + "\n"},
		{"two levels", "data.tasks", "data:\n  tasks:\n    - " + item + "\n  other: [1]\nv: 2\n"},
		{"other key name", "project.backlog", "project:\n  name: x\n  backlog:\n    - " + item + "\n"},
		{"four levels", "a.b.c.items", "a:\n  b:\n    c:\n      items:\n        - " + item + "\n"},
		{"flow style", "data.tasks", "{data: {tasks: [" + item + "]}}\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			extra := ""
			if c.path != "" {
				extra = `path: "` + c.path + `"`
			}
			b, p := open(t, c.doc, extra)
			ctx := context.Background()
			got, err := b.List(ctx)
			if err != nil || len(got) != 1 || got[0].Title != "a" {
				t.Fatalf("list: %+v %v", got, err)
			}
			created, err := b.Create(ctx, task.Task{Title: "new", Status: "todo"})
			if err != nil || created.ID != "2" {
				t.Fatalf("create: %+v %v", created, err)
			}
			got[0].Status = "done"
			if err := b.Update(ctx, got[0]); err != nil {
				t.Fatal(err)
			}
			if err := b.Delete(ctx, "2"); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(p)
			for _, keep := range []string{"keep", "other", "v:", "name"} {
				if strings.Contains(c.doc, keep) && !strings.Contains(string(data), keep) {
					t.Errorf("lost %s:\n%s", keep, data)
				}
			}
			again, _ := open(t, string(data), extra)
			list, err := again.List(ctx)
			if err != nil || len(list) != 1 || list[0].Status != "done" {
				t.Fatalf("reopen: %+v %v\n%s", list, err, data)
			}
		})
	}
}

func TestPathKeepsCommentsOutsideTheList(t *testing.T) {
	doc := "# project file\nname: demo # the name\ndata:\n  # tasks follow\n  tasks:\n    - " + item + "\nfooter: end\n"
	b, p := open(t, doc, "path: data.tasks")
	if _, err := b.Create(context.Background(), task.Task{Title: "n", Status: "todo"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	for _, want := range []string{"# project file", "name: demo # the name", "footer: end", "id: 2"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q:\n%s", want, data)
		}
	}
}

func TestPathCreatedInEmptyDocument(t *testing.T) {
	for _, doc := range []string{"", "\n", "{}\n"} {
		b, p := open(t, doc, "path: a.b")
		got, err := b.List(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatalf("%q: %+v %v", doc, got, err)
		}
		if _, err := b.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(p)
		if !strings.HasPrefix(string(data), "a:\n  b:\n    - id: 1\n") {
			t.Fatalf("%q: got\n%s", doc, data)
		}
	}
}

func TestPathErrors(t *testing.T) {
	for _, c := range []struct{ name, path, doc, want string }{
		{"missing key", "data.tasks", "data:\n  other: []\n", `no key "tasks"`},
		{"missing first key", "nope", "tasks: []\n", `no key "nope"`},
		{"not a list", "data", "data:\n  tasks: []\n", "not a list"},
		{"scalar in the way", "data.tasks", "data: x\n", "not a mapping"},
		{"list at top with path", "tasks", "- " + item + "\n", "not a mapping"},
		{"empty key", "a..b", "{}\n", "empty key"},
		{"trailing dot", "a.", "{}\n", "empty key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := writeTemp(t, c.doc)
			_, err := backend.OpenSpec(specFor(t, p, `path: "`+c.path+`"`))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestPathWithExternalEditsAndChanged(t *testing.T) {
	b, p := open(t, "data:\n  tasks:\n    - "+item+"\n", "path: data.tasks")
	touch(t, p, "data:\n  tasks:\n    - "+item+"\n    - {id: 2, title: b, status: todo}\n")
	if ch, _ := b.(backend.Watcher).Changed(); !ch {
		t.Fatal("outside edit not noticed")
	}
	c, err := b.Create(context.Background(), task.Task{Title: "c", Status: "todo"})
	if err != nil || c.ID != "3" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestExampleDetectsNestedList(t *testing.T) {
	p := writeTemp(t, "project: x\nbacklog:\n  items:\n    - {key: 1, summary: a, state: Open}\n")
	ex, err := backend.ExampleFor("yaml", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Section, "path: backlog.items") || !strings.Contains(ex.Section, "id: key") || len(ex.Statuses) != 1 {
		t.Fatalf("%+v", ex)
	}
}
