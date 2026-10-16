package csvbackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/task"
)

const sample = `id,title,description,status,priority,notes,type,owner
1,"Fix, comma","Multi
line ""quoted""",pending,high,,fix,ann
2,Second,,in_progress,low,n,feature,bob
3,Third,,done,,,chore,
`

// mk is the spec of a csv entry reading path with default options.
func mk(path string) backend.Spec {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: "+strconv.Quote(path)+"\n"), &n); err != nil {
		panic(err)
	}
	return backend.Spec{Type: "csv", Name: "csv1", Options: *n.Content[0]}
}

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.csv")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// csvOpts builds the spec of a csv entry for path with extra options from YAML text.
func csvOpts(t *testing.T, path, y string) backend.Spec {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: "+strconv.Quote(path)+"\n"+y), &n); err != nil {
		t.Fatal(err)
	}
	return backend.Spec{Type: "csv", Name: "csv1", Options: *n.Content[0]}
}

func TestRoundTripPreservesEverything(t *testing.T) {
	p := write(t, sample)
	b, err := Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tasks, _ := b.List(ctx)
	if len(tasks) != 3 || tasks[0].Title != "Fix, comma" || tasks[0].Extra["owner"] != "ann" {
		t.Fatalf("unexpected parse: %+v", tasks)
	}
	tasks[1].Status = task.Done
	if err := b.Update(ctx, tasks[1]); err != nil {
		t.Fatal(err)
	}
	b2, _ := Open(mk(p))
	got, _ := b2.List(ctx)
	if got[1].Status != task.Done || got[1].Extra["owner"] != "bob" {
		t.Fatalf("update lost: %+v", got[1])
	}
	if got[0].Description != "Multi\nline \"quoted\"" || got[0].Title != "Fix, comma" {
		t.Fatalf("quoting lost: %+v", got[0])
	}
	if got[2].Status != task.Done || got[2].Type != "chore" {
		t.Fatalf("untouched row changed: %+v", got[2])
	}
}

func TestCreateDelete(t *testing.T) {
	p := write(t, sample)
	b, _ := Open(mk(p))
	ctx := context.Background()
	c, err := b.Create(ctx, task.Task{Title: "New", Status: "pending"})
	if err != nil || c.ID != "4" || c.Status != "pending" {
		t.Fatalf("create: %+v %v", c, err)
	}
	if err := b.Delete(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "99"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	b2, _ := Open(mk(p))
	got, _ := b2.List(ctx)
	if len(got) != 3 || got[2].Title != "New" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestExternalEditIsMergedNotClobbered(t *testing.T) {
	p := write(t, sample)
	b, _ := Open(mk(p))
	ctx := context.Background()
	other, _ := Open(mk(p))
	if _, err := other.Create(ctx, task.Task{Title: "From agent", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(p, future, future)
	if ch, _ := b.(backend.Watcher).Changed(); !ch {
		t.Fatal("Changed should be true")
	}
	tasks, _ := b.List(ctx)
	tasks[0].Status = task.Done
	if err := b.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	got, _ := b.List(ctx)
	if len(got) != 4 || got[3].Title != "From agent" {
		t.Fatalf("agent row lost: %+v", got)
	}
}

func TestStatusesAreRawValuesInOrder(t *testing.T) {
	p := write(t, "id,title,status\n1,a,blocked\n2,b,open\n3,c,blocked\n4,d,\n")
	b, _ := Open(mk(p))
	if got := b.Statuses(); !reflect.DeepEqual(got, []string{"blocked", "open", ""}) {
		t.Fatalf("statuses: %q", got)
	}
}

func TestMissingRequiredColumn(t *testing.T) {
	p := write(t, "id,title\n1,a\n")
	if _, err := Open(mk(p)); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("expected missing status error, got %v", err)
	}
}

func TestRegistryByTypeAndExtension(t *testing.T) {
	if backend.TypeForLocation("X.CSV") != "csv" || backend.TypeForLocation("x.unknown") != "" {
		t.Fatal("csv should be guessed from the .csv extension only")
	}
	if _, err := backend.OpenSpec(backend.Spec{Type: "nope"}); err == nil {
		t.Fatal("unknown backend type should fail")
	}
	if _, err := backend.OpenSpec(mk(filepath.Join(t.TempDir(), "missing.csv"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
	if _, err := backend.OpenSpec(backend.Spec{Type: "CSV"}); err == nil || !strings.Contains(err.Error(), "source is required") {
		t.Fatalf("missing source: %v", err)
	}
}

func TestSourceResolvesAgainstBaseDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rel.csv"), []byte("id,title,status\n1,a,x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := csvOpts(t, "rel.csv", "")
	spec.BaseDir = dir
	if _, err := Open(spec); err != nil {
		t.Fatal(err)
	}
}

func TestCRLFPreserved(t *testing.T) {
	in := "id,title,status\r\n1,\"a\nb\",pending\r\n2,c,done\r\n"
	p := write(t, in)
	b, _ := Open(mk(p))
	ctx := context.Background()
	tasks, _ := b.List(ctx)
	tasks[0].Status = task.Done
	if err := b.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "id,title,status\r\n1,\"a\nb\",done\r\n2,c,done\r\n"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCustomFieldNames(t *testing.T) {
	p := write(t, "key,summary,state,body,owner\nA-1,Ship it,open,details,ann\nA-2,Test it,review,,bob\n")
	opts := csvOpts(t, p, "fields: {id: key, title: Summary, status: STATE, description: body}\n")
	b, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tasks, _ := b.List(ctx)
	if tasks[0].ID != "A-1" || tasks[0].Title != "Ship it" || tasks[0].Status != "open" || tasks[0].Description != "details" {
		t.Fatalf("mapping: %+v", tasks[0])
	}
	if tasks[0].Extra["owner"] != "ann" {
		t.Fatalf("extra: %+v", tasks[0].Extra)
	}
	tasks[0].Status = "review"
	if err := b.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "key,summary,state,body,owner\nA-1,Ship it,review,details,ann\nA-2,Test it,review,,bob\n"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Non-numeric ids: Create still works (max numeric id + 1, here 1).
	c, err := b.Create(ctx, task.Task{Title: "New", Status: "open"})
	if err != nil || c.ID != "1" {
		t.Fatalf("create: %+v %v", c, err)
	}
}

func TestPositionsWithHeader(t *testing.T) {
	p := write(t, "a,b,c\n1,T,done\n")
	b, err := Open(csvOpts(t, p, "fields: {id: 1, title: 2, status: 3}\n"))
	if err != nil {
		t.Fatal(err)
	}
	tasks, _ := b.List(context.Background())
	if tasks[0].Status != "done" || tasks[0].Title != "T" {
		t.Fatalf("%+v", tasks[0])
	}
}

func TestHeaderlessRoundTrip(t *testing.T) {
	p := write(t, "1,Do it,todo,note x\n2,\"Else, really\",doing,\n")
	opts := csvOpts(t, p, "header: false\nfields: {id: 1, title: 2, status: 3}\n")
	b, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tasks, _ := b.List(ctx)
	if len(tasks) != 2 || tasks[1].Title != "Else, really" || tasks[0].Extra["col4"] != "note x" {
		t.Fatalf("%+v", tasks)
	}
	tasks[0].Status = "doing"
	if err := b.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(ctx, task.Task{Title: "Third", Status: "todo"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "1,Do it,doing,note x\n2,\"Else, really\",doing,\n3,Third,todo,\n"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHeaderlessNeedsPositions(t *testing.T) {
	p := write(t, "1,a,b\n")
	if _, err := Open(csvOpts(t, p, "header: false\n")); err == nil || !strings.Contains(err.Error(), "position") {
		t.Fatalf("got %v", err)
	}
}

func TestSchemaErrors(t *testing.T) {
	p := write(t, sample)
	if _, err := Open(csvOpts(t, p, "fields: {colour: x}\n")); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := Open(csvOpts(t, p, "fields: {id: title}\n")); err == nil || !strings.Contains(err.Error(), "mapped to both") {
		t.Fatalf("duplicate mapping: %v", err)
	}
	if _, err := Open(csvOpts(t, p, "fields: {status: nope}\n")); err == nil || !strings.Contains(err.Error(), "missing required column") {
		t.Fatalf("missing column: %v", err)
	}
}

func TestOptionalColumnAppendedWhenFirstUsed(t *testing.T) {
	p := write(t, "id,title,status\n1,a,todo\n")
	b, _ := Open(csvOpts(t, p, "fields: {priority: prio}\n"))
	ctx := context.Background()
	tasks, _ := b.List(ctx)
	tasks[0].Priority = "high"
	if err := b.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "id,title,status,prio\n1,a,todo,high\n" {
		t.Fatalf("got %q", got)
	}
}

func TestRaggedRowsKeepExtraCells(t *testing.T) {
	p := write(t, "id,title,status\n1,A,pending,extra1,extra2\n2,B,done\n")
	b, err := Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, _ := b.List(ctx)
	got[1].Title = "B2"
	if err := b.Update(ctx, got[1]); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	want := "id,title,status,,\n1,A,pending,extra1,extra2\n2,B2,done,,\n"
	if string(data) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", data, want)
	}
}

func TestDuplicateAndBlankHeadersDoNotCollide(t *testing.T) {
	p := write(t, "id,title,status,tag,tag,,col3\n1,A,pending,x,y,z,w\n")
	b, err := Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, _ := b.List(ctx)
	if len(got) != 1 || len(got[0].Extra) != 4 {
		t.Fatalf("extras collided: %+v", got)
	}
	got[0].Title = "A2"
	if err := b.Update(ctx, got[0]); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	want := "id,title,status,tag,tag,,col3\n1,A2,pending,x,y,z,w\n"
	if string(data) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", data, want)
	}
}

func TestWriteThroughSymlinkKeepsLink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.csv")
	link := filepath.Join(dir, "link.csv")
	if err := os.WriteFile(real, []byte("id,title,status\n1,A,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	b, err := Open(mk(link))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(context.Background(), task.Task{Title: "B", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced: %v %v", st, err)
	}
	if data, _ := os.ReadFile(real); !strings.Contains(string(data), "B") {
		t.Fatalf("real file not updated:\n%s", data)
	}
}

func TestExternalWriteAfterOwnWriteIsDetected(t *testing.T) {
	p := write(t, "id,title,status\n1,A,pending\n")
	b, _ := Open(mk(p))
	ctx := context.Background()
	if _, err := b.Create(ctx, task.Task{Title: "B", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if ch, _ := b.(backend.Watcher).Changed(); ch {
		t.Fatal("own write reported as external change")
	}
	later := time.Now().Add(time.Hour)
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, later, later)
	if ch, _ := b.(backend.Watcher).Changed(); !ch {
		t.Fatal("external write not detected")
	}
}

func TestBOMAndMissingFinalNewlinePreserved(t *testing.T) {
	cases := map[string]string{
		"bom":        "\xef\xbb\xbfid,title,status\n1,A,pending\n",
		"no newline": "id,title,status\n1,A,pending",
		"both crlf":  "\xef\xbb\xbfid,title,status\r\n1,A,pending",
	}
	for name, in := range cases {
		p := write(t, in)
		b, err := Open(mk(p))
		if err != nil {
			t.Fatal(name, err)
		}
		ctx := context.Background()
		tasks, _ := b.List(ctx)
		tasks[0].Title = "B"
		if err := b.Update(ctx, tasks[0]); err != nil {
			t.Fatal(name, err)
		}
		got, _ := os.ReadFile(p)
		want := strings.Replace(in, ",A,", ",B,", 1)
		if string(got) != want {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
}

func TestConcurrentCreatesFromTwoBackendsDoNotLoseRows(t *testing.T) {
	p := write(t, "id,title,status\n")
	var bs []backend.Backend
	for i := 0; i < 2; i++ {
		b, err := Open(mk(p))
		if err != nil {
			t.Fatal(err)
		}
		bs = append(bs, b)
	}
	const each = 25
	var wg sync.WaitGroup
	for _, b := range bs {
		wg.Add(1)
		go func(b backend.Backend) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := b.Create(context.Background(), task.Task{Title: "t", Status: "pending"}); err != nil {
					t.Error(err)
					return
				}
			}
		}(b)
	}
	wg.Wait()
	check, _ := Open(mk(p))
	tasks, _ := check.List(context.Background())
	seen := map[string]bool{}
	for _, tk := range tasks {
		if seen[tk.ID] {
			t.Errorf("duplicate id %s", tk.ID)
		}
		seen[tk.ID] = true
	}
	if len(tasks) != 2*each {
		t.Fatalf("got %d rows, want %d", len(tasks), 2*each)
	}
}

func TestCreateValidatesAndUpdateKeepsOmittedExtra(t *testing.T) {
	p := write(t, sample)
	b, _ := Open(mk(p))
	ctx := context.Background()
	if _, err := b.Create(ctx, task.Task{Title: " ", Status: "pending"}); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := b.Create(ctx, task.Task{Title: "x", Due: "soon"}); err == nil {
		t.Fatal("bad due accepted")
	}
	tasks, _ := b.List(ctx)
	if tasks[0].Extra["owner"] != "ann" {
		t.Fatalf("setup: %+v", tasks[0].Extra)
	}
	upd := task.Task{ID: tasks[0].ID, Title: "renamed", Status: tasks[0].Status} // nil Extra
	if err := b.Update(ctx, upd); err != nil {
		t.Fatal(err)
	}
	after, _ := b.List(ctx)
	if after[0].Title != "renamed" || after[0].Extra["owner"] != "ann" {
		t.Fatalf("extra erased: %+v", after[0])
	}
}

func TestTrailingBlankLineAndEmptyFile(t *testing.T) {
	p := write(t, "id,title,status\n1,A,pending\n\n")
	b, err := Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if tasks, _ := b.List(ctx); len(tasks) != 1 {
		t.Fatalf("blank line became a row: %+v", tasks)
	}
	// A file holding only the header accepts its first row.
	p = write(t, "id,title,status\n")
	b, err = Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(ctx, task.Task{Title: "First", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "id,title,status\n1,First,pending\n" {
		t.Fatalf("got %q", got)
	}
}

func TestUpdateMissingIDIsNotFound(t *testing.T) {
	b, _ := Open(mk(write(t, sample)))
	if err := b.Update(context.Background(), task.Task{ID: "99", Title: "x"}); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// failingWrites opens sample in a directory where the data file can be locked
// (the sidecar .lock exists) but no temp file can be created, so mutations get
// as far as the write and fail there. The returned func restores the directory.
func failingWrites(t *testing.T) (backend.Backend, string, func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	p := write(t, sample)
	dir := filepath.Dir(p)
	if err := os.WriteFile(p+".lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	restore := func() { os.Chmod(dir, 0o755) }
	t.Cleanup(restore)
	return b, p, restore
}

func TestFailedWriteLeavesOriginalAndNoTempFiles(t *testing.T) {
	b, p, restore := failingWrites(t)
	_, err := b.Create(context.Background(), task.Task{Title: "x", Status: "pending"})
	if err == nil {
		t.Fatal("write into a read-only directory succeeded")
	}
	// the failure must come from the write (temp file), not from taking the lock
	if strings.Contains(err.Error(), "lock") {
		t.Fatalf("test did not reach the write path: %v", err)
	}
	restore()
	if got, _ := os.ReadFile(p); string(got) != sample {
		t.Fatalf("original changed:\n%s", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestFailedWriteDoesNotLeaveUnsavedStateInMemory(t *testing.T) {
	b, p, restore := failingWrites(t)
	ctx := context.Background()
	before, _ := b.List(ctx)

	if _, err := b.Create(ctx, task.Task{Title: "ghost", Status: "pending"}); err == nil {
		t.Fatal("create should fail")
	}
	changed := before[1]
	changed.Title = "renamed"
	if err := b.Update(ctx, changed); err == nil {
		t.Fatal("update should fail")
	}
	if err := b.Delete(ctx, before[2].ID); err == nil {
		t.Fatal("delete should fail")
	}
	after, _ := b.List(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a failed write changed what List shows:\nbefore %+v\nafter  %+v", before, after)
	}
	if got := b.Statuses(); len(got) != 3 {
		t.Fatalf("statuses changed: %v", got)
	}

	// once writes work again, only the new change is saved: no ghost, no rename, no delete
	restore()
	if _, err := b.Create(ctx, task.Task{Title: "real", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "ghost") || strings.Contains(string(data), "renamed") || !strings.Contains(string(data), "real") || !strings.Contains(string(data), "Third") {
		t.Fatalf("saved file carries stale in-memory changes:\n%s", data)
	}
}

func TestDuplicateAndEmptyIDsAreRejectedWithRowNumbers(t *testing.T) {
	cases := map[string]struct{ csv, want string }{
		"duplicate":         {"id,title,status\n1,A,pending\n2,B,pending\n1,C,pending\n", `line 4 repeats id "1" of line 2`},
		"empty id":          {"id,title,status\n1,A,pending\n,B,pending\n", "line 3 has an empty id"},
		"blank lines count": {"id,title,status\n1,A,pending\n\n2,B,pending\n2,C,pending\n", `line 5 repeats id "2" of line 4`},
	}
	for name, c := range cases {
		if _, err := Open(mk(write(t, c.csv))); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	// headerless files count from row 1
	spec := csvOpts(t, write(t, "1,A,pending\n1,B,pending\n"), "header: false\nfields: {id: 1, title: 2, status: 3}\n")
	if _, err := Open(spec); err == nil || !strings.Contains(err.Error(), `line 2 repeats id "1" of line 1`) {
		t.Errorf("headerless: %v", err)
	}
}

func TestExternalEditIntroducingADuplicateKeepsTheLastGoodState(t *testing.T) {
	p := write(t, "id,title,status\n1,A,pending\n2,B,pending\n")
	b, err := Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// someone edits the file and breaks the ids (and reorders the columns)
	if err := os.WriteFile(p, []byte("status,id,title\npending,7,X\npending,7,Y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, later, later)
	if _, err := b.List(ctx); err == nil {
		t.Log("List serves the cached rows until a mutation re-reads the file")
	}
	err = b.Update(ctx, task.Task{ID: "1", Title: "A2", Status: "pending"})
	if err == nil || !strings.Contains(err.Error(), "repeats id") {
		t.Fatalf("mutation on a broken file: %v", err)
	}
	// the file was not overwritten by stale state
	if got, _ := os.ReadFile(p); string(got) != "status,id,title\npending,7,X\npending,7,Y\n" {
		t.Fatalf("broken file was rewritten:\n%s", got)
	}
	// once fixed, everything works again
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n2,B,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, later.Add(time.Hour), later.Add(time.Hour))
	if err := b.Update(ctx, task.Task{ID: "1", Title: "A2", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "1,A2,pending") {
		t.Fatalf("update after fixing the file:\n%s", got)
	}
}

func TestCreateUsesMaxNumericID(t *testing.T) {
	p := write(t, "id,title,status\nT-1,A,pending\n5,B,pending\n")
	b, _ := Open(mk(p))
	created, err := b.Create(context.Background(), task.Task{Title: "C", Status: "pending"})
	if err != nil || created.ID != "6" {
		t.Fatalf("got %+v %v", created, err)
	}
}

func TestLockTimesOutWithAClearErrorInsteadOfHanging(t *testing.T) {
	p := write(t, sample)
	be, err := Open(mk(p))
	if err != nil {
		t.Fatal(err)
	}
	b := be.(*Backend)
	b.lockTimeout = 150 * time.Millisecond

	release, err := lockFile(p+".lock", time.Second) // another tkan holds the lock
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = b.Create(context.Background(), task.Task{Title: "blocked", Status: "pending"})
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "locked by another tkan") {
		t.Fatalf("got %v", err)
	}
	if took < 100*time.Millisecond || took > 2*time.Second {
		t.Fatalf("waited %s, want about the 150ms timeout", took)
	}
	if got, _ := os.ReadFile(p); string(got) != sample {
		t.Fatal("a timed-out write changed the file")
	}

	// once the holder lets go, the same backend works (the failed call left nothing behind)
	release()
	if _, err := b.Create(context.Background(), task.Task{Title: "after", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "after") || strings.Contains(string(got), "blocked") {
		t.Fatalf("file:\n%s", got)
	}
}

func TestLockWaitsForAShortHolder(t *testing.T) {
	p := write(t, sample)
	be, _ := Open(mk(p))
	release, err := lockFile(p+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); release() }()
	if _, err := be.Create(context.Background(), task.Task{Title: "patient", Status: "pending"}); err != nil {
		t.Fatalf("a holder that lets go within the timeout must not fail the write: %v", err)
	}
}
