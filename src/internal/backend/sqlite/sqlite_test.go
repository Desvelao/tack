package sqlitebackend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/task"
)

// needCLI skips the test when no SQLite CLI is installed.
func needCLI(t *testing.T) {
	t.Helper()
	if _, err := resolveCLI(nil); err != nil {
		t.Skip(err)
	}
}

// sqlExec runs SQL on db with the real CLI (test fixtures and outside edits).
func sqlExec(t *testing.T, db, sql string) {
	t.Helper()
	cli, err := resolveCLI(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(context.Background(), cli, db, sql, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func spec(t *testing.T, db, y string) backend.Spec {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: "+strconv.Quote(db)+"\n"+y), &n); err != nil {
		t.Fatal(err)
	}
	return backend.Spec{Type: "sqlite", Name: "db1", Options: *n.Content[0]}
}

const fixture = `CREATE TABLE tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, status TEXT, owner TEXT);
INSERT INTO tasks (title, status, owner) VALUES ('A', 'pending', 'ann'), ('B', 'done', NULL);`

func newDB(t *testing.T, sql string) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "t.db")
	if sql != "" {
		sqlExec(t, db, sql)
	}
	return db
}

func open(t *testing.T, db, y string) *Backend {
	t.Helper()
	b, err := Open(spec(t, db, y))
	if err != nil {
		t.Fatal(err)
	}
	return b.(*Backend)
}

func TestListUpdateDeleteRoundTrip(t *testing.T) {
	needCLI(t)
	db := newDB(t, fixture)
	b := open(t, db, "")
	ctx := context.Background()
	tasks, err := b.List(ctx)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("%v %+v", err, tasks)
	}
	if tasks[0].ID != "1" || tasks[0].Title != "A" || tasks[0].Extra["owner"] != "ann" || tasks[1].Extra["owner"] != "" {
		t.Fatalf("unexpected parse: %+v", tasks)
	}
	if got := b.Statuses(); len(got) != 2 || got[0] != "pending" || got[1] != "done" {
		t.Fatalf("statuses = %v", got)
	}
	tasks[0].Status = task.Done
	tasks[0].Extra = nil // a caller copy without the extra column must not blank it
	if err := b.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	tasks, _ = b.List(ctx)
	if tasks[0].Status != "done" || tasks[0].Extra["owner"] != "ann" {
		t.Fatalf("after update: %+v", tasks[0])
	}
	if err := b.Delete(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if tasks, _ = b.List(ctx); len(tasks) != 1 || tasks[0].ID != "2" {
		t.Fatalf("after delete: %+v", tasks)
	}
	if err := b.Delete(ctx, "99"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	if err := b.Update(ctx, task.Task{ID: "99", Title: "x"}); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestCreateAssignsIDs(t *testing.T) {
	needCLI(t)
	b := open(t, newDB(t, fixture), "")
	ctx := context.Background()
	n, err := b.Create(ctx, task.Task{Title: "C", Status: "pending", Extra: map[string]string{"owner": "bob"}})
	if err != nil || n.ID != "3" {
		t.Fatalf("%v %+v", err, n)
	}
	tasks, _ := b.List(ctx)
	if len(tasks) != 3 || tasks[2].Extra["owner"] != "bob" {
		t.Fatalf("%+v", tasks)
	}
	if _, err := b.Create(ctx, task.Task{Title: " "}); err == nil {
		t.Fatal("empty title must be rejected")
	}
}

func TestCreateWithTextIDColumnUsesMaxPlusOne(t *testing.T) {
	needCLI(t)
	db := newDB(t, `CREATE TABLE tasks (id TEXT PRIMARY KEY, title TEXT, status TEXT);
INSERT INTO tasks VALUES ('7','A','pending'), ('x','B','done');`)
	b := open(t, db, "")
	n, err := b.Create(context.Background(), task.Task{Title: "C", Status: "pending"})
	if err != nil || n.ID != "8" {
		t.Fatalf("%v %+v", err, n)
	}
}

func TestMissingDatabaseIsCreatedByFirstCreate(t *testing.T) {
	needCLI(t)
	db := newDB(t, "")
	b := open(t, db, "")
	ctx := context.Background()
	if tasks, err := b.List(ctx); err != nil || len(tasks) != 0 {
		t.Fatalf("%v %+v", err, tasks)
	}
	if err := b.Delete(ctx, "1"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	n, err := b.Create(ctx, task.Task{Title: "first", Status: "pending", Priority: "high", Labels: "a,b"})
	if err != nil || n.ID != "1" {
		t.Fatalf("%v %+v", err, n)
	}
	tasks, err := b.List(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].Priority != "high" || tasks[0].Labels != "a,b" {
		t.Fatalf("%v %+v", err, tasks)
	}
	// An existing database without the table gets it too.
	b2 := open(t, newDB(t, "CREATE TABLE other (x);"), "")
	if _, err := b2.Create(ctx, task.Task{Title: "t", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
}

func TestFieldAndTableMapping(t *testing.T) {
	needCLI(t)
	db := newDB(t, `CREATE TABLE todo (key INTEGER PRIMARY KEY, summary TEXT, state TEXT, Prio TEXT);
INSERT INTO todo (summary, state, Prio) VALUES ('A', 'open', 'high');`)
	b := open(t, db, "table: todo\nfields: {id: key, title: summary, status: state, priority: prio}")
	ctx := context.Background()
	tasks, err := b.List(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].Title != "A" || tasks[0].Status != "open" || tasks[0].Priority != "high" {
		t.Fatalf("%v %+v", err, tasks)
	}
	tasks[0].Title = "A2"
	if err := b.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Create(ctx, task.Task{Title: "B", Status: "open"}); err != nil || n.ID != "2" {
		t.Fatalf("%v %+v", err, n)
	}
	if _, err := Open(spec(t, db, "table: todo")); err == nil || !strings.Contains(err.Error(), `no column "id"`) {
		t.Fatalf("unmapped required field: %v", err)
	}
	if _, err := Open(spec(t, db, "table: todo\nfields: {id: key, title: summary, status: state, notes: nope}")); err == nil || !strings.Contains(err.Error(), `field "notes"`) {
		t.Fatalf("configured but missing optional field: %v", err)
	}
}

func TestOptionErrors(t *testing.T) {
	needCLI(t)
	db := newDB(t, "")
	for y, want := range map[string]string{
		"fields: {nope: x}":        "unknown field",
		"fields: {title: ''}":      "empty column name",
		"poll: fast":               "poll must be",
		"table: \"a\\nb\"":         "control character",
		"cli: [sqlite3, 5]":        "cli must be",
		"cli: definitely-not-here": "cli [definitely-not-here]",
	} {
		if _, err := Open(spec(t, db, y)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", y, err, want)
		}
	}
	if _, err := Open(backend.Spec{Type: "sqlite"}); err == nil || !strings.Contains(err.Error(), "source is required") {
		t.Errorf("no source: %v", err)
	}
}

// Hostile values must come back byte for byte and must not execute anything.
func TestHostileValuesAreStoredLiterally(t *testing.T) {
	needCLI(t)
	dir := t.TempDir()
	pwn := filepath.Join(dir, "pwned")
	db := filepath.Join(dir, "t.db")
	b := open(t, db, "")
	ctx := context.Background()
	vals := []string{
		"it's \"quoted\"", "semi; DROP TABLE tasks; --", "line1\n.shell touch " + pwn + "\nline3",
		".quit", "'; .shell touch " + pwn + "; --", "ünï ✓ 日本", "back\\slash", "  padded  ", "tab\there", "cr\r\nlf",
	}
	for _, v := range vals {
		n, err := b.Create(ctx, task.Task{Title: "t", Status: "pending", Description: v, Notes: v})
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		n.Title = v
		if err := b.Update(ctx, n); err != nil {
			t.Fatalf("update %q: %v", v, err)
		}
	}
	tasks, err := b.List(ctx)
	if err != nil || len(tasks) != len(vals) {
		t.Fatalf("%v %d", err, len(tasks))
	}
	for i, v := range vals {
		if tasks[i].Title != v || tasks[i].Description != v || tasks[i].Notes != v {
			t.Errorf("value %d: got %q, want %q", i, tasks[i].Title, v)
		}
	}
	if _, err := os.Stat(pwn); err == nil {
		t.Fatal("a value was executed as a CLI command")
	}
	if _, err := b.Create(ctx, task.Task{Title: "nul\x00", Status: "p"}); err == nil {
		t.Fatal("NUL must be rejected")
	}
}

func TestChangedPollModes(t *testing.T) {
	needCLI(t)
	db := newDB(t, fixture)
	ctx := context.Background()

	b := open(t, db, "")
	if ch, err := b.Changed(); err != nil || ch {
		t.Fatalf("fresh: %v %v", ch, err)
	}
	if _, err := b.Create(ctx, task.Task{Title: "own", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if ch, _ := b.Changed(); ch {
		t.Fatal("our own write must not look external")
	}
	if tasks, _ := b.List(ctx); len(tasks) != 3 {
		t.Fatalf("List after own write: %d", len(tasks))
	}
	sqlExec(t, db, "INSERT INTO tasks (title, status) VALUES ('ext', 'pending');")
	if ch, _ := b.Changed(); !ch {
		t.Fatal("external write not noticed")
	}
	if tasks, _ := b.List(ctx); len(tasks) != 4 {
		t.Fatalf("List after external write: %d", len(tasks))
	}

	n := open(t, db, "poll: none")
	sqlExec(t, db, "INSERT INTO tasks (title, status) VALUES ('later', 'pending');")
	if ch, _ := n.Changed(); ch {
		t.Fatal("poll: none must never report changes")
	}
	if tasks, _ := n.List(ctx); len(tasks) != 4 {
		t.Fatalf("poll: none serves the cache: %d", len(tasks))
	}
	if err := n.Reload(); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := n.List(ctx); len(tasks) != 5 {
		t.Fatalf("after Reload: %d", len(tasks))
	}

	e := open(t, db, "poll: 5")
	clock := time.Now()
	e.now = func() time.Time { return clock }
	e.Changed() // first check
	sqlExec(t, db, "INSERT INTO tasks (title, status) VALUES ('again', 'pending');")
	if ch, _ := e.Changed(); ch {
		t.Fatal("poll: 5 must not look before the interval elapsed")
	}
	clock = clock.Add(6 * time.Second)
	if ch, _ := e.Changed(); !ch {
		t.Fatal("poll: 5 must notice after the interval")
	}
}

func TestConcurrentCreatesGetDistinctIDs(t *testing.T) {
	needCLI(t)
	db := newDB(t, fixture)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 2; i++ {
		b := open(t, db, "")
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				n, err := b.Create(context.Background(), task.Task{Title: "c", Status: "pending"})
				if err != nil {
					t.Error(err)
					return
				}
				ids <- n.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
	if len(seen) != 8 {
		t.Fatalf("got %d ids", len(seen))
	}
}

// fakeBin writes an executable shell script named name into a new PATH dir.
func fakeBin(t *testing.T, name, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestCLISelection(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	// Nothing installed: the error names every candidate and the option.
	t.Setenv("PATH", t.TempDir())
	_, err := resolveCLI(nil)
	if err == nil || !strings.Contains(err.Error(), "sqlite3: not found") || !strings.Contains(err.Error(), "sqlite: not found") || !strings.Contains(err.Error(), "options.cli") {
		t.Fatalf("err = %v", err)
	}

	// A CLI without -json is rejected with a clear message.
	fakeBin(t, "sqlite3", `echo 1`)
	if _, err := resolveCLI(nil); err == nil || !strings.Contains(err.Error(), "-json") {
		t.Fatalf("old cli: %v", err)
	}

	// The first candidate that works wins; a broken one is skipped.
	fakeBin(t, "sqlite", `while read -r _; do :; done; echo '[{"x":1}]'`)
	cli, err := resolveCLI(nil)
	if err != nil || len(cli) != 1 || cli[0] != "sqlite" {
		t.Fatalf("fallback: %v %v", cli, err)
	}

	// options.cli (string or list) overrides the candidates.
	cli, err = resolveCLI("sqlite")
	if err != nil || cli[0] != "sqlite" {
		t.Fatalf("string: %v %v", cli, err)
	}
	cli, err = resolveCLI([]any{"sqlite", "-batch"})
	if err != nil || len(cli) != 2 {
		t.Fatalf("list: %v %v", cli, err)
	}
}

func TestExampleDetectsLayout(t *testing.T) {
	needCLI(t)
	db := newDB(t, `CREATE TABLE tasks (key INTEGER PRIMARY KEY, summary TEXT, state TEXT, owner TEXT, sprint TEXT);
INSERT INTO tasks (summary, state, owner, sprint) VALUES ('a','doing','x','1'), ('b','todo','y','1'), ('c','doing','x','2');`)
	ex, err := example(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"source: {{source}}", "table: tasks", "id: key", "title: summary", "status: state", "assignee: owner", "kept untouched: sprint"} {
		if !strings.Contains(ex.Section, want) {
			t.Errorf("section lacks %q:\n%s", want, ex.Section)
		}
	}
	if strings.Join(ex.Statuses, ",") != "doing,todo" || len(ex.Extras) != 1 || ex.Extras[0] != "sprint" {
		t.Errorf("statuses %v extras %v", ex.Statuses, ex.Extras)
	}
	// The generated section must open the same database.
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(strings.ReplaceAll(ex.Section, "{{source}}", strconv.Quote(db))), &n); err != nil {
		t.Fatal(err)
	}
	b, err := Open(backend.Spec{Type: "sqlite", Options: *n.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := b.List(context.Background()); err != nil || len(tasks) != 3 || tasks[0].Title != "a" {
		t.Fatalf("%v %+v", err, tasks)
	}

	for _, loc := range []string{"", filepath.Join(t.TempDir(), "none.db")} {
		ex, err := example(loc)
		if err != nil || !strings.Contains(ex.Section, "table: tasks") || len(ex.Notes) == 0 {
			t.Errorf("%q: %v %+v", loc, err, ex)
		}
	}
}

func TestRegistered(t *testing.T) {
	if got := backend.TypeForLocation("x/tasks.db"); got != "sqlite" {
		t.Fatalf("TypeForLocation = %q", got)
	}
	if _, err := backend.ExampleFor("sqlite", ""); err != nil {
		t.Fatal(err)
	}
}
