package actionlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tack/internal/action"
	"tack/internal/task"
)

var tk = task.Task{ID: "7", Title: "Fix it", Backend: "csv1", Status: "pending"}

func job(t *testing.T, env map[string]string, cmd ...string) *action.Job {
	t.Helper()
	s := action.Spec{Name: "demo", Command: cmd, Env: env, Stdin: action.StdinMarkdown}
	s.Normalize()
	j := s.Start(context.Background(), tk, t.TempDir())
	<-j.Done()
	return j
}

func TestNewValidation(t *testing.T) {
	if l, err := New(Spec{}, "/cfg", "s"); l != nil || err != nil || l.Enabled() {
		t.Fatalf("disabled: %v %v", l, err)
	}
	if _, err := New(Spec{File: "a.jsonl", Dir: "d"}, "/cfg", "s"); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both: %v", err)
	}
	if _, err := New(Spec{File: "a.csv"}, "/cfg", "s"); err == nil || !strings.Contains(err.Error(), ".jsonl") {
		t.Fatalf("ext: %v", err)
	}
	l, err := New(Spec{File: "logs/a.jsonl"}, "/cfg", "s")
	if err != nil || l.Path() != "/cfg/logs/a.jsonl" {
		t.Fatalf("%v %v", l, err)
	}
	var nilLog *Log
	if nilLog.Append(Record{}) != nil || nilLog.Path() != "" {
		t.Fatal("nil log must be inert")
	}
	if recs, err := nilLog.Records(true); recs != nil || err != nil {
		t.Fatal("nil log has no records")
	}
}

func TestRecordKeepsCommandStdinOutputButNeverEnvValues(t *testing.T) {
	j := job(t, map[string]string{"SECRET_TOKEN": "hunter2"}, "sh", "-c", `echo "out $1"; echo warn >&2; exit 3`, "sh", "{title}")
	r := FromJob(j, "tui")
	if r.Result != ResultError || r.ExitCode == nil || *r.ExitCode != 3 || r.Action != "demo" || r.Source != "tui" || r.ID != "7" || r.Backend != "csv1" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Stdout, "out Fix it") || !strings.Contains(r.Stderr, "warn") || !strings.HasPrefix(r.Stdin, "# Fix it") {
		t.Fatalf("io: %+v", r)
	}
	if strings.Join(r.Command, " ") != "sh -c echo \"out $1\"; echo warn >&2; exit 3 sh Fix it" {
		t.Fatalf("command %q", r.Command)
	}
	if r.Time.IsZero() || r.Ended.Before(r.Time) || r.DurationMS < 0 {
		t.Fatalf("times: %+v", r)
	}
	l, _ := New(Spec{File: "a.jsonl"}, t.TempDir(), "sess")
	if err := l.Append(r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(l.Path())
	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "SECRET_TOKEN") || strings.Contains(string(data), "TACK_TASK") {
		t.Fatalf("environment leaked into the log:\n%s", data)
	}
}

func TestSummary(t *testing.T) {
	ok := FromJob(job(t, nil, "sh", "-c", "echo hello; echo more"), "cli")
	if s := ok.Summary(); s != "action demo <1s exit 0 out 11 B err 0 B" {
		t.Fatalf("%q", s)
	}
	bad := FromJob(job(t, nil, "sh", "-c", "echo boom >&2; exit 2"), "cli")
	if s := bad.Summary(); s != "action demo <1s exit 2 out 0 B err 5 B" {
		t.Fatalf("%q", s)
	}
	s := action.Spec{Name: "term", Mode: action.ModeInteractive, Command: []string{"x"}}
	now := time.Now()
	r := Interactive(s, tk, action.Plan{Argv: []string{"x"}}, now, now.Add(75*time.Second), nil, "tui")
	if got := r.Summary(); got != "action term 1m15s (terminal)" || r.ExitCode != nil {
		t.Fatalf("%q", got)
	}
}

func TestFileLogRoundTripAndPrivacy(t *testing.T) {
	dir := t.TempDir()
	mine, _ := New(Spec{File: "sub/runs.jsonl"}, dir, "me")
	other, _ := New(Spec{File: "sub/runs.jsonl"}, dir, "earlier")
	r := FromJob(job(t, nil, "echo", "x"), "tui")
	r2 := r
	r2.Time = r.Time.Add(-time.Hour)
	if err := other.Append(r2); err != nil {
		t.Fatal(err)
	}
	if err := mine.Append(r); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(mine.Path()); st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
	recs, err := mine.Records(false)
	if err != nil || len(recs) != 1 || recs[0].Session != "earlier" {
		t.Fatalf("others: %+v %v", recs, err)
	}
	if recs, _ = mine.Records(true); len(recs) != 2 || !recs[0].Time.Before(recs[1].Time) {
		t.Fatalf("all, oldest first: %+v", recs)
	}
	os.WriteFile(mine.Path(), append([]byte("garbage\n"), mustRead(t, mine.Path())...), 0o600)
	if recs, _ = mine.Records(true); len(recs) != 2 {
		t.Fatalf("damaged line must be skipped: %d", len(recs))
	}
	if recs, err := (&Log{file: filepath.Join(dir, "missing.jsonl")}).Records(true); recs != nil || err != nil {
		t.Fatalf("missing file: %v %v", recs, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDirLogOneFilePerRunGroupedByAction(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(Spec{Dir: "runs"}, dir, "s1")
	r := FromJob(job(t, nil, "echo", "x"), "cli")
	r.Action, r.ID = "my action/../x", "a b"
	for i := 0; i < 3; i++ { // same second, same card: files must not overwrite each other
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(dir, "runs", "my_action_.._x")
	files, err := os.ReadDir(sub)
	if err != nil || len(files) != 3 {
		t.Fatalf("%v %v", files, err)
	}
	for _, f := range files {
		if strings.ContainsAny(f.Name(), "/ ") || !strings.Contains(f.Name(), "csv1-a_b") {
			t.Fatalf("file name %q", f.Name())
		}
	}
	if st, _ := os.Stat(sub); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Fatal("path traversal through the action name")
	}
	recs, _ := (&Log{dir: filepath.Join(dir, "runs"), session: "other"}).Records(false)
	if len(recs) != 3 {
		t.Fatalf("read back %d", len(recs))
	}
}

func TestFilter(t *testing.T) {
	a := Record{Time: time.Now(), Action: "claude", Result: ResultOK, ID: "1", Title: "Alpha"}
	b := Record{Time: time.Now(), Action: "notify", Result: ResultError, ID: "2", Title: "Beta", Error: "boom"}
	for q, want := range map[string]int{"": 2, "result=error": 1, "action:claude": 1, "alpha": 1, "boom": 1, "result!=ok": 1} {
		parsed, err := Parse(q)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if got := len(Filter([]Record{a, b}, parsed)); got != want {
			t.Errorf("%q: %d, want %d", q, got, want)
		}
	}
	if _, err := Parse("nofield=1"); err == nil {
		t.Error("unknown field accepted")
	}
}

// The audit file is long-lived and may be shipped elsewhere: what a command
// printed must stay in the action log, in the output and in the error text alike.
func TestSummaryNeverContainsOutput(t *testing.T) {
	for _, script := range []string{
		"echo sk-secret-token; echo more",
		"echo sk-secret-token >&2; exit 3",
		"echo sk-secret-token; echo sk-secret-token >&2",
	} {
		rec := FromJob(job(t, nil, "sh", "-c", script), "cli")
		if s := rec.Summary(); strings.Contains(s, "sk-secret-token") {
			t.Errorf("%q: summary %q leaks output", script, s)
		}
	}
	// A message tack wrote itself (a start failure, a timeout) is kept.
	rec := Record{Action: "x", Mode: action.ModeBackground, Result: ResultError, Error: "timed out after 10s"}
	if s := rec.Summary(); !strings.HasSuffix(s, ": timed out after 10s") {
		t.Errorf("%q", s)
	}
}

func TestHugeRecordDoesNotHideLaterRuns(t *testing.T) {
	l, _ := New(Spec{File: "a.jsonl"}, t.TempDir(), "sess")
	rec := func(id string) Record {
		return Record{Time: time.Now(), Action: "demo", ID: id, Result: ResultOK, Command: []string{"x"}}
	}
	// Appended as written by an older version or another tool: no cap on the line.
	big := rec("big")
	big.Stdout = strings.Repeat("x", 9<<20)
	for _, r := range []Record{rec("1"), big, rec("2")} {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := l.Records(true)
	if err != nil || len(recs) != 3 || recs[2].ID != "2" {
		t.Fatalf("%v: got %d records", err, len(recs))
	}
}

func TestStdinIsCappedInTheRecord(t *testing.T) {
	s := action.Spec{Name: "demo", Command: []string{"x"}}
	now := time.Now()
	r := Interactive(s, tk, action.Plan{Argv: []string{"x"}, Stdin: strings.Repeat("é", maxStdin)}, now, now, nil, "cli")
	if len(r.Stdin) > maxStdin || !r.Truncated || !utf8.ValidString(r.Stdin) {
		t.Fatalf("stdin %d bytes, truncated %v, valid %v", len(r.Stdin), r.Truncated, utf8.ValidString(r.Stdin))
	}
}

func TestNonASCIIActionNamesGetOwnDirectories(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(Spec{Dir: "log"}, dir, "sess")
	for _, name := range []string{"é", "è", "añadir"} {
		r := Record{Time: time.Now(), Action: name, ID: "1", Result: ResultOK, Command: []string{"x"}}
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"é", "è", "añadir"} {
		if _, err := os.Stat(filepath.Join(dir, "log", name)); err != nil {
			t.Errorf("no directory for %q: %v", name, err)
		}
	}
	if recs, _ := l.Records(true); len(recs) != 3 {
		t.Fatalf("%d records", len(recs))
	}
}

func TestEveryRunFieldIsDocumented(t *testing.T) {
	if len(FieldDocs) != len(Fields) {
		t.Fatalf("%d docs for %d fields", len(FieldDocs), len(Fields))
	}
	for i, f := range Fields {
		if FieldDocs[i].Name != f || strings.TrimSpace(FieldDocs[i].Desc) == "" {
			t.Errorf("field %q: doc %+v", f, FieldDocs[i])
		}
	}
}

func TestRecentLimitsAndCountsOlder(t *testing.T) {
	for _, dirMode := range []bool{false, true} {
		dir := t.TempDir()
		l := &Log{file: filepath.Join(dir, "a.jsonl"), session: "me"}
		if dirMode {
			l = &Log{dir: filepath.Join(dir, "runs"), session: "me"}
		}
		base := time.Now().Add(-time.Hour).Truncate(time.Second)
		for i := 0; i < 5; i++ {
			if err := l.Append(Record{Time: base.Add(time.Duration(i) * time.Minute), Action: "a", ID: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
		}
		recs, older, err := l.Recent(true, 2)
		if err != nil || len(recs) != 2 || older != 3 || recs[0].ID != "3" || recs[1].ID != "4" {
			t.Fatalf("dir=%v: %+v older=%d %v", dirMode, recs, older, err)
		}
		if recs, older, _ = l.Recent(true, 0); len(recs) != 5 || older != 0 {
			t.Fatalf("dir=%v all: %d older=%d", dirMode, len(recs), older)
		}
		if recs, older, _ = l.Recent(true, 9); len(recs) != 5 || older != 0 {
			t.Fatalf("dir=%v big n: %d older=%d", dirMode, len(recs), older)
		}
	}
}

func TestEventRoundTripsLooksUpAndFilters(t *testing.T) {
	withEv := Record{Time: time.Now(), Action: "notify", Source: "hook", Event: "task.moved", Result: ResultOK}
	b, err := json.Marshal(withEv)
	if err != nil || !strings.Contains(string(b), `"event":"task.moved"`) {
		t.Fatalf("%v %s", err, b)
	}
	var got Record
	if err := json.Unmarshal(b, &got); err != nil || got.Event != "task.moved" {
		t.Fatalf("%v %+v", err, got)
	}
	plain := Record{Time: time.Now(), Action: "x", Source: "cli", Result: ResultOK}
	if b, _ = json.Marshal(plain); strings.Contains(string(b), "event") {
		t.Fatalf("event written when empty: %s", b)
	}
	got = Record{}
	if err := json.Unmarshal([]byte(`{"time":"2026-01-01T00:00:00Z","action":"old","source":"tui"}`), &got); err != nil || got.Event != "" {
		t.Fatalf("old record: %v %+v", err, got)
	}
	if v, ok := Lookup(withEv)("event"); !ok || v != "task.moved" {
		t.Fatalf("lookup %q %v", v, ok)
	}
	q, err := Parse("source=hook event:task.moved")
	if err != nil {
		t.Fatal(err)
	}
	other := withEv
	other.Event = "task.added"
	if recs := Filter([]Record{withEv, other, plain}, q); len(recs) != 1 || recs[0].Event != "task.moved" {
		t.Fatalf("%+v", recs)
	}
}

// The big record spans several read chunks and the lines around it still come out in order.
func TestRecentReadsAcrossChunks(t *testing.T) {
	l := &Log{file: filepath.Join(t.TempDir(), "a.jsonl"), session: "me"}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 6; i++ {
		r := Record{Time: base.Add(time.Duration(i) * time.Minute), Action: "a", ID: fmt.Sprint(i)}
		if i == 2 {
			r.Stdout = strings.Repeat("x", 3*chunk+17)
		}
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	recs, older, err := l.Recent(true, 0)
	if err != nil || len(recs) != 6 || older != 0 {
		t.Fatalf("all: %d older=%d %v", len(recs), older, err)
	}
	for i, r := range recs {
		if r.ID != fmt.Sprint(i) {
			t.Fatalf("order: %d is %s", i, r.ID)
		}
	}
	recs, older, err = l.Recent(true, 2)
	if err != nil || len(recs) != 2 || older != 4 || recs[0].ID != "4" {
		t.Fatalf("last two: %+v older=%d %v", recs, older, err)
	}
	recs, older, _ = l.Recent(true, 4) // stops inside the big record's file region
	if len(recs) != 4 || older != 2 || recs[0].ID != "2" {
		t.Fatalf("last four: %d older=%d first %s", len(recs), older, recs[0].ID)
	}
}

func TestFileLogRotatesAndRecentReadsBothHalves(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Spec{File: "a.jsonl", MaxSize: 1}, dir, "me")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 12; i++ {
		r := Record{Time: base.Add(time.Duration(i) * time.Minute), Action: "a", ID: fmt.Sprint(i), Stdout: strings.Repeat("o", 300)}
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "a.jsonl.1")); err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	st, _ := os.Stat(filepath.Join(dir, "a.jsonl"))
	if st.Size() > 3<<10 {
		t.Errorf("current file is %d bytes: not capped", st.Size())
	}
	recs, _, err := l.Recent(true, 0)
	if err != nil || len(recs) < 6 || recs[len(recs)-1].ID != "11" {
		t.Fatalf("%d records, last %v, %v", len(recs), recs[len(recs)-1:], err)
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Time.Before(recs[i-1].Time) {
			t.Fatalf("not oldest first at %d", i)
		}
	}
	// The cap on what Recent returns spans the two files, and the older count too.
	recs, older, _ := l.Recent(true, len(recs)-1)
	if len(recs) == 0 || older < 1 {
		t.Errorf("limit across files: %d records, older %d", len(recs), older)
	}
}

func TestDirLogKeepsTheNewestRuns(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Spec{Dir: "runs", Keep: 3}, dir, "me")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 8; i++ {
		if err := l.Append(Record{Time: base.Add(time.Duration(i) * time.Minute), Action: "a", ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	l.mu.Lock()
	l.prune() // appends prune now and then; force the last one
	l.mu.Unlock()
	recs, _, _ := l.Recent(true, 0)
	if len(recs) != 3 || recs[0].ID != "5" || recs[2].ID != "7" {
		t.Fatalf("kept %+v", recs)
	}
}

func TestSizeOptionsAreChecked(t *testing.T) {
	for name, s := range map[string]Spec{
		"negative size":  {File: "a.jsonl", MaxSize: -1},
		"negative keep":  {Dir: "d", Keep: -1},
		"size with dir":  {Dir: "d", MaxSize: 5},
		"keep with file": {File: "a.jsonl", Keep: 5},
	} {
		if _, err := New(s, t.TempDir(), "me"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRunAndReportStatusAreFilterFields(t *testing.T) {
	r := Record{Time: time.Now(), Action: "report", RunID: "r-1", ReportStatus: "done", Result: ResultOK}
	for q, want := range map[string]bool{"run=r-1": true, "run=r-2": false, "report_status=done": true, "report_status=wip": false} {
		p, err := Parse(q)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if got := p.Match(Lookup(r), SearchFields); got != want {
			t.Errorf("%q: %v want %v", q, got, want)
		}
	}
}
