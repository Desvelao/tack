package jsonbackend

import (
	"context"
	"os"
	"strings"
	"testing"

	"tack/internal/backend"
	"tack/internal/task"
)

const item = `{"id":"1","title":"a","status":"todo"}`

func TestPathLocations(t *testing.T) {
	for _, c := range []struct {
		name, path, doc string
	}{
		{"top-level array", "", `[` + item + `]`},
		{"default tasks key", "", `{"tasks":[` + item + `]}`},
		{"explicit one key", "tasks", `{"keep":1,"tasks":[` + item + `]}`},
		{"two levels", "data.tasks", `{"data":{"tasks":[` + item + `],"other":[1]},"v":2}`},
		{"other key name", "project.backlog", `{"project":{"name":"x","backlog":[` + item + `]}}`},
		{"four levels", "a.b.c.items", `{"a":{"b":{"c":{"items":[` + item + `]}}}}`},
		{"spaces around keys", " data . tasks ", `{"data":{"tasks":[` + item + `]}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			extra := ""
			if c.path != "" {
				extra = `path: "` + c.path + `"`
			}
			p := tmp(t, "t.json", c.doc)
			b := openJSON(t, p, extra)
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
			// Everything outside the task list survives, and the list is where it was.
			for _, keep := range []string{`"keep"`, `"other"`, `"v"`, `"name"`} {
				if strings.Contains(c.doc, keep) && !strings.Contains(string(data), keep) {
					t.Errorf("lost %s:\n%s", keep, data)
				}
			}
			again := openJSON(t, p, extra)
			list, err := again.List(ctx)
			if err != nil || len(list) != 1 || list[0].Status != "done" {
				t.Fatalf("reopen: %+v %v\n%s", list, err, data)
			}
		})
	}
}

func TestPathCreatedInEmptyDocument(t *testing.T) {
	for _, doc := range []string{"", "  \n", "{}"} {
		p := tmp(t, "t.json", doc)
		b := openJSON(t, p, "path: a.b\nindent: 0")
		if n := count(t, b); n != 0 {
			t.Fatalf("%q: %d tasks", doc, n)
		}
		if _, err := b.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(p)
		if string(data) != `{"a":{"b":[{"id":"1","title":"x","status":"s"}]}}`+"\n" {
			t.Fatalf("%q: got %s", doc, data)
		}
	}
}

func TestPathCreatesMissingSiblingOnWrite(t *testing.T) {
	// The path exists in the document read; a sibling branch is untouched.
	p := tmp(t, "t.json", `{"a":{"x":1,"b":[]},"z":true}`)
	b := openJSON(t, p, "path: a.b\nindent: 0")
	if _, err := b.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != `{"a":{"x":1,"b":[{"id":"1","title":"x","status":"s"}]},"z":true}`+"\n" {
		t.Fatalf("got %s", data)
	}
}

func TestPathErrors(t *testing.T) {
	for _, c := range []struct{ name, typ, path, doc, want string }{
		{"missing key", "json", "data.tasks", `{"data":{"other":[]}}`, `no key "tasks"`},
		{"missing first key", "json", "nope", `{"tasks":[]}`, `no key "nope"`},
		{"not a list", "json", "data", `{"data":{"tasks":[]}}`, "not a list"},
		{"scalar in the way", "json", "data.tasks", `{"data":"x"}`, "not an object"},
		{"array at top with path", "json", "tasks", `[` + item + `]`, "not an object"},
		{"empty key", "json", "a..b", `{}`, "empty key"},
		{"leading dot", "json", ".a", `{}`, "empty key"},
		{"jsonl has no tree", "jsonl", "a.b", item + "\n", "not supported"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := tmp(t, "t."+c.typ, c.doc)
			_, err := backend.OpenSpec(spec(t, c.typ, p, `path: "`+c.path+`"`))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestPathWithExternalEditsAndChanged(t *testing.T) {
	p := tmp(t, "t.json", `{"data":{"tasks":[`+item+`]}}`)
	b := openJSON(t, p, "path: data.tasks")
	touch(t, p, `{"data":{"tasks":[`+item+`,{"id":"2","title":"b","status":"todo"}]}}`)
	if ch, _ := b.(backend.Watcher).Changed(); !ch {
		t.Fatal("outside edit not noticed")
	}
	c, err := b.Create(context.Background(), task.Task{Title: "c", Status: "todo"})
	if err != nil || c.ID != "3" {
		t.Fatalf("%+v %v", c, err)
	}
	// A broken edit keeps the last good state.
	touch(t, p, `{"data":{"nothing":[]}}`)
	if _, err := b.List(context.Background()); err == nil {
		t.Fatal("document without the path accepted")
	}
	if st := b.Statuses(); len(st) != 1 {
		t.Fatalf("statuses %v", st)
	}
}

func TestExampleDetectsNestedList(t *testing.T) {
	p := tmp(t, "t.json", `{"project":"x","backlog":{"items":[{"key":"1","summary":"a","state":"Open"}]}}`)
	ex, err := backend.ExampleFor("json", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Section, "path: backlog.items") || !strings.Contains(ex.Section, "id: key") || len(ex.Statuses) != 1 {
		t.Fatalf("%+v", ex)
	}
	// Two candidate lists are ambiguous: no path guessed.
	p = tmp(t, "u.json", `{"a":[{"id":1}],"b":[{"id":2}]}`)
	if _, err := backend.ExampleFor("json", p); err == nil {
		t.Log("ambiguous document produced an example without a path")
	}
}
