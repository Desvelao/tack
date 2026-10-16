package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tkan/internal/backend"
	"tkan/internal/board"
	"tkan/internal/task"
)

const baseYAML = "backend:\n  - type: csv\n    options: {source: work.csv}\n"

// setup writes work.csv and tkan.yaml into a temp dir. yml is the whole
// config; when it has no `backend:` list the default csv entry is prepended.
func setup(t *testing.T, csv, yml string) (dir, file, cfg string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "work.csv")
	cfg = filepath.Join(dir, "tkan.yaml")
	if err := os.WriteFile(file, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yml, "backend:") {
		yml = baseYAML + yml
	}
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

func do(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := run(args, &o, &e)
	return code, o.String(), e.String()
}

const issuesCSV = "key,summary,state\nA-1,Ship it,open\nA-2,Review it,in review\nA-3,Odd,parked\n"

const issuesYAML = `
backend:
  - type: csv
    options:
      source: work.csv
      fields: {id: key, title: summary, status: state}
board:
  columns:
    - name: Todo
      values: [open, new]
    - name: Doing
      values: [in review, wip]
      write: wip
    - name: Done
      values: [closed]
`

func TestConfigDrivesColumnsAndMoves(t *testing.T) {
	_, file, cfg := setup(t, issuesCSV, issuesYAML)

	code, out, errs := do(t, "-c", cfg, "list", "-json")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errs)
	}
	var tasks []map[string]any
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatal(err)
	}
	cols := []string{}
	for _, tk := range tasks {
		cols = append(cols, tk["column"].(string))
	}
	if strings.Join(cols, ",") != "Todo,Doing,parked" {
		t.Fatalf("columns: %v", cols)
	}

	// Column name resolves to its write value.
	if code, _, errs = do(t, "-c", cfg, "set-status", "A-1", "doing"); code != 0 {
		t.Fatalf("set-status: %d %s", code, errs)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "A-1,Ship it,wip") {
		t.Fatalf("file:\n%s", data)
	}

	// Raw value of another column also works; canonical spelling is written.
	if code, _, errs = do(t, "-c", cfg, "set-status", "A-1", "NEW"); code != 0 {
		t.Fatalf("set-status raw: %d %s", code, errs)
	}
	data, _ = os.ReadFile(file)
	if !strings.Contains(string(data), "A-1,Ship it,new") {
		t.Fatalf("file:\n%s", data)
	}

	// list -status by column name matches all of its values.
	_, out, _ = do(t, "-c", cfg, "list", "-status", "Doing", "-json")
	if !strings.Contains(out, "A-2") || strings.Contains(out, "A-1") {
		t.Fatalf("filter: %s", out)
	}
}

func TestUnknownStatusIsUsageError(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML)
	code, _, errs := do(t, "-c", cfg, "set-status", "A-1", "nonsense")
	if code != exitUsage || !strings.Contains(errs, "Todo, Doing, Done, parked") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if code, _, _ = do(t, "-c", cfg, "show", "ZZ-9"); code != exitNotFound {
		t.Fatalf("code=%d", code)
	}
}

func TestAddDefaultsToFirstColumnWriteValue(t *testing.T) {
	_, file, cfg := setup(t, issuesCSV, issuesYAML)
	code, out, errs := do(t, "-c", cfg, "add", "-title", "Fresh")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if strings.TrimSpace(out) != "1" { // non-numeric ids: max numeric + 1
		t.Fatalf("id: %q", out)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "1,Fresh,open") {
		t.Fatalf("file:\n%s", data)
	}
}

func TestConfigLocationAndRelativeSource(t *testing.T) {
	dir, _, _ := setup(t, issuesCSV, issuesYAML)
	// -c with a source relative to the config file, in another directory.
	other := filepath.Join(dir, "cfg", "t.yaml")
	_ = os.MkdirAll(filepath.Dir(other), 0o755)
	_ = os.WriteFile(other, []byte(strings.Replace(issuesYAML, "source: work.csv", "source: ../work.csv", 1)), 0o644)
	code, out, errs := do(t, "-c", other, "config")
	if code != 0 || !strings.Contains(out, "name: csv1") || !strings.Contains(out, "type: csv") || !strings.Contains(out, "../work.csv") {
		t.Fatalf("config: %d %q %q", code, out, errs)
	}
	if code, out, _ = do(t, "-c", other, "statuses"); code != 0 || !strings.Contains(out, "Doing") {
		t.Fatalf("statuses: %d %q", code, out)
	}
	// $TKAN_CONFIG and ./tkan.yaml.
	t.Setenv("TKAN_CONFIG", other)
	if code, out, _ = do(t, "statuses"); code != 0 || !strings.Contains(out, "Doing") {
		t.Fatalf("env: %d %q", code, out)
	}
	t.Setenv("TKAN_CONFIG", "")
	t.Chdir(dir)
	if code, out, _ = do(t, "statuses"); code != 0 || !strings.Contains(out, "Doing") {
		t.Fatalf("cwd default: %d %q", code, out)
	}
	t.Chdir(t.TempDir())
	if code, _, errs = do(t, "statuses"); code != exitError || !strings.Contains(errs, "tkan init") {
		t.Fatalf("no config: %d %q", code, errs)
	}
}

func TestOldFlagsAndLegacyConfigAreGone(t *testing.T) {
	_, file, cfg := setup(t, "id,title,status\n1,a,pending\n", "")
	if code, _, _ := do(t, "-f", file, "list"); code != exitUsage {
		t.Fatalf("-f should be rejected, code=%d", code)
	}
	if code, _, _ := do(t, file, "list"); code != exitUsage {
		t.Fatalf("positional FILE should be rejected, code=%d", code)
	}
	_ = os.WriteFile(cfg, []byte("backend: csv\nsource: work.csv\n"), 0o644)
	if code, _, errs := do(t, "-c", cfg, "list"); code != exitError || !strings.Contains(errs, "docs/config.md") {
		t.Fatalf("legacy config: %d %q", code, errs)
	}
}

func TestDefaultColumnsForPlainConfig(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status\n1,a,pending\n2,b,blocked\n", "")
	code, out, _ := do(t, "-c", cfg, "statuses")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"pending", "in_progress", "done", "failed", "blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if code, _, _ := do(t, "-c", cfg, "set-status", "1", "done"); code != 0 {
		t.Fatal("default set-status failed")
	}
}

func TestMultipleBackendsRouteAndDisambiguate(t *testing.T) {
	dir, _, cfg := setup(t, "id,title,status\n1,InWork,pending\n2,OnlyWork,pending\n", "")
	_ = os.WriteFile(filepath.Join(dir, "other.csv"), []byte("id,title,status\n1,InOther,pending\n"), 0o644)
	_ = os.WriteFile(cfg, []byte(`
backend:
  - {type: csv, options: {source: work.csv}}
  - {type: csv, name: other, options: {source: other.csv}}
`), 0o644)

	_, out, _ := do(t, "-c", cfg, "list", "-json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 3 || rows[0]["backend"] != "csv1" || rows[2]["backend"] != "other" {
		t.Fatalf("%v %s", err, out)
	}
	if _, out, _ = do(t, "-c", cfg, "list"); !strings.Contains(out, "BACKEND") || !strings.Contains(out, "other") {
		t.Fatalf("table:\n%s", out)
	}
	// Backend is a filter field.
	if _, out, _ = do(t, "-c", cfg, "list", "-q", "backend=other", "-json"); !strings.Contains(out, "InOther") || strings.Contains(out, "InWork") {
		t.Fatalf("filter: %s", out)
	}
	// Unique ID works bare; ambiguous ID is a usage error naming the candidates.
	if code, _, errs := do(t, "-c", cfg, "show", "2"); code != 0 {
		t.Fatalf("unique id: %d %s", code, errs)
	}
	code, _, errs := do(t, "-c", cfg, "set-status", "1", "done")
	if code != exitUsage || !strings.Contains(errs, "csv1") || !strings.Contains(errs, "other") {
		t.Fatalf("ambiguous: %d %q", code, errs)
	}
	if code, _, errs = do(t, "-c", cfg, "set-status", "1", "done", "-b", "other"); code != 0 {
		t.Fatalf("-b: %d %s", code, errs)
	}
	if code, _, errs = do(t, "-c", cfg, "set-status", "csv1:1", "in_progress"); code != 0 {
		t.Fatalf("NAME:ID: %d %s", code, errs)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "work.csv"))
	b, _ := os.ReadFile(filepath.Join(dir, "other.csv"))
	if !strings.Contains(string(a), "1,InWork,in_progress") || !strings.Contains(string(b), "1,InOther,done") {
		t.Fatalf("work:\n%s\nother:\n%s", a, b)
	}
	// add targets the first backend unless -b; delete honours -b.
	if code, out, _ = do(t, "-c", cfg, "add", "-title", "N", "-b", "other"); code != 0 || strings.TrimSpace(out) != "other:2" {
		t.Fatalf("add: %d %q", code, out)
	}
	if code, _, _ = do(t, "-c", cfg, "delete", "1"); code != exitUsage {
		t.Fatalf("ambiguous delete: %d", code)
	}
	if code, _, errs = do(t, "-c", cfg, "delete", "1", "-b", "other"); code != 0 {
		t.Fatalf("delete: %d %s", code, errs)
	}
	if code, _, _ = do(t, "-c", cfg, "show", "1", "-b", "nope"); code == 0 {
		t.Fatal("unknown backend must fail")
	}
}

func TestInvalidConfigIsReported(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, "board:\n  columns:\n    - {name: a, values: [x]}\n    - {name: b, values: [x]}\n")
	code, _, errs := do(t, "-c", cfg, "list")
	if code != exitError || !strings.Contains(errs, "mapped to both") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestInitWritesConfigThatWorks(t *testing.T) {
	dir, file, _ := setup(t, issuesCSV, "")
	out := filepath.Join(dir, "new.yaml")
	code, _, errs := do(t, "init", "-source", file, "-o", out)
	if code != 0 || !strings.Contains(errs, "wrote "+out) {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	// The generated columns come from the file's values, not the defaults.
	_, so, _ := do(t, "-c", out, "statuses")
	for _, want := range []string{"open", "in review", "parked"} {
		if !strings.Contains(so, want) {
			t.Errorf("missing %q:\n%s", want, so)
		}
	}
	if strings.Contains(so, "in_progress") {
		t.Errorf("default columns leaked:\n%s", so)
	}
	data, _ := os.ReadFile(out)
	for _, want := range []string{"- type: csv", "options:", "id: key", "status: state", "source: work.csv"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %q:\n%s", want, data)
		}
	}
}

func TestInitRefusesWhenAnAliasConfigExists(t *testing.T) {
	dir, file, _ := setup(t, issuesCSV, "")
	t.Chdir(dir)
	_ = os.Remove("tkan.yaml") // setup wrote one
	_ = os.WriteFile(".tkan.yml", []byte("# mine\n"), 0o644)
	code, _, errs := do(t, "init", file)
	if code != exitError || !strings.Contains(errs, ".tkan.yml already exists") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if _, err := os.Stat("tkan.yaml"); err == nil {
		t.Fatal("wrote tkan.yaml next to an existing .tkan.yml")
	}
}

func TestInitRefusesToOverwriteUnlessForced(t *testing.T) {
	dir, file, _ := setup(t, issuesCSV, "")
	side := filepath.Join(dir, "mine.yaml")
	_ = os.WriteFile(side, []byte("# mine\n"), 0o644)
	code, _, errs := do(t, "init", "-o", side, file)
	if code != exitError || !strings.Contains(errs, "already exists") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if b, _ := os.ReadFile(side); string(b) != "# mine\n" {
		t.Fatalf("file was modified: %q", b)
	}
	if code, _, errs = do(t, "init", "-force", "-o", side, file); code != 0 {
		t.Fatalf("force: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(side); !strings.Contains(string(b), "type: csv") {
		t.Fatalf("not overwritten: %q", b)
	}
}

func TestInitStdoutBackendAndErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "new.csv")
	code, out, _ := do(t, "init", "-backend", "csv", "-o", "-", "-source", missing)
	if code != 0 || !strings.Contains(out, "- type: csv") || !strings.Contains(out, "source: "+missing) {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if _, err := os.Stat(missing + ".tkan.yaml"); err == nil {
		t.Fatal("stdout mode must not write files")
	}
	if code, out, _ = do(t, "init", "-backend", "custom", "-o", "-"); code != 0 || !strings.Contains(out, "- type: custom") || !strings.Contains(out, "command:") {
		t.Fatalf("custom: code=%d out=%q", code, out)
	}
	if code, _, errs := do(t, "init", "-source", filepath.Join(dir, "x.unknown")); code != exitUsage || !strings.Contains(errs, "-backend") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if code, _, errs := do(t, "init", "-backend", "nope", "-o", "-"); code != exitError || !strings.Contains(errs, "supported: csv") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestInitWorksDespiteBrokenConfig(t *testing.T) {
	dir, file, cfg := setup(t, issuesCSV, "")
	_ = os.WriteFile(cfg, []byte("board: [not, a, mapping"), 0o644)
	if code, _, errs := do(t, "init", "-force", "-o", filepath.Join(dir, "tkan.yaml"), file); code != 0 {
		t.Fatalf("code=%d err=%s", code, errs)
	}
}

func TestListQueryOperators(t *testing.T) {
	csv := "id,title,description,status,priority,notes,type,owner\n" +
		"1,Fix login,,pending,high,,fix,ann\n" +
		"2,Bot fillers,,in_progress,medium,wip,feature,bob\n" +
		"3,Docs,,done,,,chore,\n"
	_, _, cfg := setup(t, csv, "")
	ids := func(q string) string {
		code, out, errs := do(t, "-c", cfg, "list", "-q", q, "-json")
		if code != 0 {
			t.Fatalf("%q: code=%d %s", q, code, errs)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, r := range rows {
			s = append(s, r["id"].(string))
		}
		return strings.Join(s, ",")
	}
	for q, want := range map[string]string{
		"priority=high":             "1",
		"priority!=high":            "2,3",
		"owner=":                    "3",
		"owner=ann,bob":             "1,2",
		"title~^bot":                "2",
		"col:done":                  "3",
		"type!:fix -docs":           "2",
		"notes:wip priority^med":    "2",
		"fix":                       "1",
		"owner:ann":                 "1",
		`"fix login" priority=high`: "1",
	} {
		if got := ids(q); got != want {
			t.Errorf("%q: got %q want %q", q, got, want)
		}
	}
	// -q combines with -status.
	_, out, _ := do(t, "-c", cfg, "list", "-status", "pending", "-q", "owner=bob", "-json")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("combined: %s", out)
	}
	// Unknown field is a usage error that lists the valid fields.
	code, _, errs := do(t, "-c", cfg, "list", "-q", "colour:red")
	if code != exitUsage || !strings.Contains(errs, "unknown field") || !strings.Contains(errs, "owner") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestHelpFilterAndSearchConfig(t *testing.T) {
	code, out, _ := do(t, "help", "filter")
	if code != 0 || !strings.Contains(out, "field!:value") || !strings.Contains(out, "field~regex") {
		t.Fatalf("code=%d\n%s", code, out)
	}
	if code, _, errs := do(t, "help", "nope"); code != exitUsage || !strings.Contains(errs, "filter") {
		t.Fatalf("code=%d %q", code, errs)
	}

	csv := "id,title,status,owner\n1,Fix,open,ann\n2,Other,open,bob\n"
	// Configured plain-text search over an extra column.
	_, _, cfg := setup(t, csv, "board:\n  search: [owner]\n")
	_, out, errs := do(t, "-c", cfg, "list", "-q", "bob", "-json")
	if !strings.Contains(out, `"id": "2"`) || strings.Contains(out, `"id": "1"`) {
		t.Fatalf("search owner: %s %s", out, errs)
	}
	// Title is no longer searched by plain text.
	if _, out, _ = do(t, "-c", cfg, "list", "-q", "fix", "-json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("title should not match: %s", out)
	}
	// Unknown searchable field fails at start-up.
	_, _, cfg2 := setup(t, csv, "board:\n  search: [nope]\n")
	code, _, errs = do(t, "-c", cfg2, "list")
	if code != exitError || !strings.Contains(errs, "board.search") {
		t.Fatalf("code=%d %q", code, errs)
	}
}

func TestInitWritesSearchLine(t *testing.T) {
	_, file, _ := setup(t, "id,title,status,owner\n1,a,open,ann\n", "")
	_, out, _ := do(t, "init", "-o", "-", "-source", file)
	if !strings.Contains(out, "search: [id, title, description, notes]") || !strings.Contains(out, "owner") {
		t.Fatalf("%s", out)
	}
}

func TestBoardSortDoesNotAffectListAndIsValidated(t *testing.T) {
	csv := "id,title,status,priority\n1,a,pending,low\n2,b,pending,high\n"
	_, _, cfg := setup(t, csv, "board:\n  sort: [priority: [high, low]]\n")
	_, out, errs := do(t, "-c", cfg, "list", "-json")
	if strings.Index(out, `"id": "1"`) > strings.Index(out, `"id": "2"`) || strings.Contains(out, "sort") {
		t.Fatalf("list must keep store order: %s %s", out, errs)
	}
	if _, out, _ = do(t, "-c", cfg, "config"); !strings.Contains(out, "priority:high,low") {
		t.Fatalf("config output:\n%s", out)
	}
	_, _, bad := setup(t, csv, "board:\n  sort: [-nope]\n")
	if code, _, errs := do(t, "-c", bad, "list"); code != exitError || !strings.Contains(errs, "board.sort: unknown field") {
		t.Fatalf("code=%d %q", code, errs)
	}
}

func TestEditChangesOnlyGivenFields(t *testing.T) {
	_, file, cfg := setup(t, issuesCSV, issuesYAML)
	if code, _, errs := do(t, "-c", cfg, "edit", "A-1", "-assignee", "ann", "-due", "2026-12-01"); code != 0 {
		t.Fatalf("edit: %d %s", code, errs)
	}
	code, out, _ := do(t, "-c", cfg, "show", "A-1", "-json")
	if code != 0 || !strings.Contains(out, `"assignee": "ann"`) && !strings.Contains(out, `"assignee":"ann"`) || !strings.Contains(out, "Ship it") {
		t.Fatalf("show: %d %s", code, out)
	}
	// an explicit empty value clears the field
	if code, _, _ := do(t, "-c", cfg, "edit", "A-1", "-assignee", ""); code != 0 {
		t.Fatal("clear failed")
	}
	if _, out, _ = do(t, "-c", cfg, "show", "A-1", "-json"); strings.Contains(out, "ann") || !strings.Contains(out, "2026-12-01") {
		t.Fatalf("clear/keep: %s", out)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "A-2,Review it,in review") {
		t.Fatalf("other rows changed: %s", data)
	}
}

func TestEditErrors(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML)
	for name, args := range map[string][]string{
		"no fields":  {"edit", "A-1"},
		"bad date":   {"edit", "A-1", "-due", "soon"},
		"empty name": {"edit", "A-1", "-title", " "},
		"no id":      {"edit", "-title", "x"},
	} {
		if code, _, _ := do(t, append([]string{"-c", cfg}, args...)...); code != exitUsage {
			t.Errorf("%s: code %d, want %d", name, code, exitUsage)
		}
	}
	if code, _, _ := do(t, "-c", cfg, "edit", "NOPE", "-title", "x"); code != exitNotFound {
		t.Errorf("unknown id: code %d, want %d", code, exitNotFound)
	}
}

func TestAddTrimsAndValidates(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status\n", "")
	if code, _, _ := do(t, "-c", cfg, "add", "-title", " x ", "-due", "tomorrow"); code != exitUsage {
		t.Fatalf("bad -due accepted: code %d", code)
	}
	if code, _, _ := do(t, "-c", cfg, "add", "-title", " x ", "-start", "2026-13-40"); code != exitUsage {
		t.Fatalf("bad -start accepted: code %d", code)
	}
	if code, _, e := do(t, "-c", cfg, "add", "-title", "  padded  ", "-due", "2026-05-01"); code != exitOK {
		t.Fatalf("valid add failed: %d %s", code, e)
	}
	_, out, _ := do(t, "-c", cfg, "list", "-json")
	if !strings.Contains(out, `"title": "padded"`) && !strings.Contains(out, `"title":"padded"`) {
		t.Fatalf("title not trimmed:\n%s", out)
	}
}

func TestStrayArgsAndHelpFlag(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status\n1,A,,pending\n", "")
	for _, args := range [][]string{{"list", "doing"}, {"add", "My title"}, {"statuses", "x"}} {
		code, _, e := do(t, append([]string{"-c", cfg}, args...)...)
		if code != exitUsage || !strings.Contains(e, "unexpected argument") {
			t.Errorf("%v: code %d, stderr %q", args, code, e)
		}
	}
	if code, _, _ := do(t, "-c", cfg, "list", "-h"); code != exitOK {
		t.Errorf("list -h: code %d, want 0", code)
	}
}

func TestVersionFlagAndCommand(t *testing.T) {
	for _, args := range [][]string{{"-version"}, {"version"}} {
		code, out, _ := do(t, args...)
		if code != exitOK || strings.TrimSpace(out) != versionString() {
			t.Errorf("%v: code %d out %q", args, code, out)
		}
	}
	old := buildDate
	t.Cleanup(func() { buildDate = old })
	buildDate = "2026-01-02"
	_, out, _ := do(t, "version")
	if want := "tkan " + version + " (built 2026-01-02)"; strings.TrimSpace(out) != want {
		t.Errorf("stamped: got %q, want %q", out, want)
	}
}

func TestConfigFlagAfterSubcommandAndDoubleDash(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status\n-5,Dash,,pending\n1,One,,pending\n", "")
	if code, out, e := do(t, "list", "-c", cfg, "-json"); code != exitOK || !strings.Contains(out, "Dash") {
		t.Fatalf("list -c: %d %q %q", code, out, e)
	}
	if code, out, e := do(t, "list", "-c="+cfg); code != exitOK || !strings.Contains(out, "One") {
		t.Fatalf("list -c=: %d %q %q", code, out, e)
	}
	if code, out, e := do(t, "-c", cfg, "show", "-json", "--", "-5"); code != exitOK || !strings.Contains(out, "Dash") {
		t.Fatalf("show -- -5: %d %q %q", code, out, e)
	}
}

func TestHelpPerCommandAndCompletion(t *testing.T) {
	for _, c := range visibleCommands() {
		code, out, e := do(t, "help", c.name)
		if code != exitOK || !strings.Contains(out, c.name) {
			t.Errorf("help %s: code %d out %q err %q", c.name, code, out, e)
		}
	}
	if _, out, _ := do(t, "help", "add"); !strings.Contains(out, "-title") || !strings.Contains(out, "-due") {
		t.Errorf("help add lacks flags:\n%s", out)
	}
	if code, _, e := do(t, "help", "nope"); code != exitUsage || !strings.Contains(e, "topics:") {
		t.Errorf("unknown topic: %d %q", code, e)
	}
	for _, sh := range []string{"bash", "zsh", "fish"} {
		if code, out, _ := do(t, "completion", sh); code != exitOK || !strings.Contains(out, "set-status") {
			t.Errorf("completion %s: %d %q", sh, code, out)
		}
	}
	if code, _, _ := do(t, "completion", "tcsh"); code != exitUsage {
		t.Errorf("unknown shell: code %d", code)
	}
}

func TestSetStatusManyIDsAndNoOp(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status\n1,A,,pending\n2,B,,pending\n3,C,,done\n", "")
	code, out, e := do(t, "-c", cfg, "set-status", "1", "2", "3", "done")
	if code != exitOK || !strings.Contains(out, "3 already") || strings.Count(out, "-> done") != 2 {
		t.Fatalf("code %d out %q err %q", code, out, e)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), "pending") {
		t.Fatalf("not all moved:\n%s", data)
	}
	// one unknown ID: nothing is changed
	do(t, "-c", cfg, "set-status", "1", "2", "pending")
	before, _ := os.ReadFile(file)
	if code, _, _ := do(t, "-c", cfg, "set-status", "1", "99", "done"); code != exitNotFound {
		t.Fatalf("unknown id: code %d", code)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("partial change:\n%s", after)
	}
}

func TestEditStatus(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status\n1,A,,pending\n", "")
	if code, _, e := do(t, "-c", cfg, "edit", "1", "-status", "done", "-title", "A2"); code != exitOK {
		t.Fatalf("code %d %s", code, e)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "1,A2,,done") {
		t.Fatalf("file:\n%s", data)
	}
	if code, _, _ := do(t, "-c", cfg, "edit", "1", "-status", "nowhere"); code != exitUsage {
		t.Fatalf("unknown status: code %d", code)
	}
}

func TestDeleteDryRunManyAndMissing(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status\n1,A,,pending\n2,B,,pending\n3,C,,pending\n", "")
	before, _ := os.ReadFile(file)
	code, out, _ := do(t, "-c", cfg, "delete", "-n", "1", "2")
	if code != exitOK || !strings.Contains(out, `would delete csv1:1 "A"`) {
		t.Fatalf("dry run: %d %q", code, out)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("dry run changed the file")
	}
	if code, _, _ := do(t, "-c", cfg, "delete", "1", "99"); code != exitNotFound {
		t.Fatalf("unknown id: %d", code)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("partial delete")
	}
	code, out, _ = do(t, "-c", cfg, "delete", "1", "2")
	if code != exitOK || !strings.Contains(out, `deleted csv1:2 "B"`) {
		t.Fatalf("delete: %d %q", code, out)
	}
	if data, _ := os.ReadFile(file); strings.Contains(string(data), "A") || strings.Contains(string(data), ",B,") {
		t.Fatalf("not deleted:\n%s", data)
	}
}

func TestInitHonoursGlobalConfigFlagAndChecksDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "conf.yaml")
	if code, _, e := do(t, "-c", target, "init", "-backend", "csv"); code != exitOK {
		t.Fatalf("init -c: %d %s", code, e)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("init ignored -c: %v", err)
	}
	missingDir := filepath.Join(dir, "nope", "x.yaml")
	if code, _, e := do(t, "init", "-backend", "csv", "-o", missingDir); code != exitError || !strings.Contains(e, "does not exist") {
		t.Fatalf("missing dir: %d %q", code, e)
	}
	if code, _, e := do(t, "init", "a.csv", "b.csv"); code != exitUsage || !strings.Contains(e, `"b.csv"`) {
		t.Fatalf("extra arg: %d %q", code, e)
	}
	if code, _, _ := do(t, "init", "-h"); code != exitOK {
		t.Fatalf("init -h: %d", code)
	}
}

func TestExitCodeContract(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status,priority\n1,Alpha,,pending,high\n2,Beta,,done,low\n", "")
	c := func(args ...string) []string { return append([]string{"-c", cfg}, args...) }
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"unknown command", []string{"bogus"}, exitUsage},
		{"bad top-level flag", []string{"-nope"}, exitUsage},
		{"bad subcommand flag", c("list", "-nope"), exitUsage},
		{"help without args", []string{"help"}, exitOK},
		{"help unknown topic", []string{"help", "zzz"}, exitUsage},
		{"show unknown id", c("show", "99"), exitNotFound},
		{"show without id", c("show"), exitUsage},
		{"set-status unknown id", c("set-status", "99", "done"), exitNotFound},
		{"set-status unknown status", c("set-status", "1", "nowhere"), exitUsage},
		{"set-status too few args", c("set-status", "1"), exitUsage},
		{"delete unknown id", c("delete", "99"), exitNotFound},
		{"add unknown status", c("add", "-title", "x", "-status", "nowhere"), exitUsage},
		{"add without title", c("add"), exitUsage},
		{"list unknown status", c("list", "-status", "nowhere"), exitUsage},
		{"list bad query", c("list", "-q", "title~("), exitUsage},
		{"list unknown query field", c("list", "-q", "nofield=1"), exitUsage},
	}
	for _, tc := range cases {
		if got, _, _ := do(t, tc.args...); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPlainOutputAndRawStatusFilters(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status,priority\n1,Alpha,,pending,high\n2,Beta,,done,low\n3,Gamma,,odd_status,\n", "")
	_, out, _ := do(t, "-c", cfg, "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "BACKEND") || !strings.Contains(lines[1], "Alpha") {
		t.Fatalf("list table:\n%s", out)
	}
	if _, out, _ = do(t, "-c", cfg, "show", "1"); !strings.Contains(out, "title:       Alpha") || !strings.Contains(out, "column:      pending") {
		t.Fatalf("show text:\n%s", out)
	}
	// a raw value that is not a column name matches by raw status
	if _, out, _ = do(t, "-c", cfg, "list", "-status", "odd_status"); !strings.Contains(out, "Gamma") || strings.Contains(out, "Alpha") {
		t.Fatalf("raw status filter:\n%s", out)
	}
	// -q and -status together can leave nothing: header only, exit 0
	code, out, _ := do(t, "-c", cfg, "list", "-status", "done", "-q", "priority=high")
	if code != exitOK || strings.Contains(out, "Alpha") || strings.Contains(out, "Beta") {
		t.Fatalf("empty combination: %d\n%s", code, out)
	}
	var cols []struct{ Name string }
	_, out, _ = do(t, "-c", cfg, "statuses", "-json")
	if err := json.Unmarshal([]byte(out), &cols); err != nil || len(cols) < 3 {
		t.Fatalf("statuses -json: %v\n%s", err, out)
	}
}

func TestAddFlagsRoundTrip(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status\n", "")
	code, _, e := do(t, "-c", cfg, "add", "-title", "T", "-status", "done", "-priority", "high", "-due", "2026-05-01", "-b", "csv1")
	if code != exitOK {
		t.Fatalf("add: %d %s", code, e)
	}
	_, out, _ := do(t, "-c", cfg, "show", "1")
	for _, want := range []string{"status:      done", "priority:    high", "due:         2026-05-01"} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
}

func TestListIDsAndTSV(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status,priority,assignee\n1,\"Two\nlines\tand tab\",,pending,,\n2,Plain,,done,high,ann\n", "")
	if _, out, _ := do(t, "-c", cfg, "list", "-ids"); out != "csv1:1\ncsv1:2\n" {
		t.Fatalf("ids: %q", out)
	}
	_, out, _ := do(t, "-c", cfg, "list", "-tsv")
	rows := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(rows) != 2 {
		t.Fatalf("tsv rows: %q", out)
	}
	cells := strings.Split(rows[0], "\t")
	if len(cells) != 9 || cells[8] != `Two\nlines\tand tab` || cells[4] != "" {
		t.Fatalf("tsv cells (%d): %q", len(cells), cells)
	}
	if code, _, _ := do(t, "-c", cfg, "list", "-ids", "-json"); code != exitUsage {
		t.Fatalf("conflicting formats: %d", code)
	}
}

func TestHelpMentionsConfigLookup(t *testing.T) {
	_, out, _ := do(t, "help")
	if !strings.Contains(out, "~/.tkan.yaml") {
		t.Fatalf("usage lacks the home lookup:\n%s", out)
	}
}

func TestSetStatusWarnsWhenOverWIPLimit(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status\n1,A,,todo\n2,B,,todo\n3,C,,doing\n", "board:\n  columns:\n    - {name: Todo, values: [todo]}\n    - {name: Doing, values: [doing], limit: 1}\n")
	code, _, e := do(t, "-c", cfg, "set-status", "1", "Doing")
	if code != exitOK || !strings.Contains(e, "over its WIP limit (2/1)") {
		t.Fatalf("code %d stderr %q", code, e)
	}
	// within the limit: silent
	if _, _, e = do(t, "-c", cfg, "set-status", "1", "Todo"); strings.Contains(e, "WIP") {
		t.Fatalf("unexpected warning %q", e)
	}
	if code, _, e = do(t, "-c", cfg, "edit", "2", "-status", "Doing"); code != exitOK || !strings.Contains(e, "WIP limit") {
		t.Fatalf("edit -status: %d %q", code, e)
	}
}

func TestListWithSavedView(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status,priority\n1,A,,pending,high\n2,B,,pending,low\n3,C,,done,high\n", "board:\n  views:\n    - {name: urgent, query: \"priority=high\"}\n    - {name: open, query: \"col:pending\"}\n")
	_, out, _ := do(t, "-c", cfg, "list", "-v", "urgent", "-ids")
	if out != "csv1:1\ncsv1:3\n" {
		t.Fatalf("view urgent: %q", out)
	}
	_, out, _ = do(t, "-c", cfg, "list", "-v", "URGENT", "-q", "col:pending", "-ids") // combined with -q
	if out != "csv1:1\n" {
		t.Fatalf("view + -q: %q", out)
	}
	if code, _, e := do(t, "-c", cfg, "list", "-v", "nope"); code != exitUsage || !strings.Contains(e, "views: urgent, open") {
		t.Fatalf("unknown view: %d %q", code, e)
	}
}

func TestBoardGroupMustBeAKnownField(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status,priority\n1,A,,pending,high\n", "board:\n  group: priorty\n")
	code, _, e := do(t, "-c", cfg, "list")
	if code != exitError || !strings.Contains(e, "board.group") || !strings.Contains(e, `did you mean "priority"`) {
		t.Fatalf("code %d err %q", code, e)
	}
	_, _, cfg = setup(t, "id,title,description,status,priority\n1,A,,pending,high\n", "board:\n  group: priority\n")
	if code, _, e := do(t, "-c", cfg, "list"); code != exitOK {
		t.Fatalf("valid group rejected: %d %s", code, e)
	}
}

func TestCountGroupings(t *testing.T) {
	csv := "id,title,description,status,priority,type,assignee,labels\n" +
		"1,A,,pending,high,bug,ann,\"ui,api\"\n2,B,,pending,low,bug,bob,ui\n3,C,,done,high,feature,,\n4,D,,done,high,,ann,\n"
	_, _, cfg := setup(t, csv, "")
	// count runs the command and returns KEY->COUNT plus the keys in output order.
	count := func(args ...string) (map[string]string, []string, int) {
		code, out, _ := do(t, append([]string{"-c", cfg, "count"}, args...)...)
		got, keys := map[string]string{}, []string(nil)
		for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Fields(line)
			if i == 0 || len(f) != 2 {
				continue
			}
			got[f[0]] = f[1]
			keys = append(keys, f[0])
		}
		return got, keys, code
	}
	got, order, code := count()
	if code != exitOK || got["pending"] != "2" || got["done"] != "2" || got["in_progress"] != "0" || got["failed"] != "0" || got["TOTAL"] != "4" {
		t.Fatalf("by column: %d %v", code, got)
	}
	if strings.Join(order, ",") != "pending,in_progress,done,failed,TOTAL" {
		t.Fatalf("columns must keep board order: %v", order)
	}
	got, order, _ = count("-by", "priority")
	if got["high"] != "3" || got["low"] != "1" || order[0] != "high" {
		t.Fatalf("by priority (biggest first): %v %v", got, order)
	}
	got, _, _ = count("-by", "type")
	if got["(none)"] != "1" || got["bug"] != "2" || got["feature"] != "1" {
		t.Fatalf("by type: %v", got)
	}
	got, _, _ = count("-by", "label") // task 1 counts under ui and api; 3 and 4 have none
	if got["ui"] != "2" || got["api"] != "1" || got["(none)"] != "2" || got["TOTAL"] != "5" {
		t.Fatalf("by label: %v", got)
	}
	_, out, _ := do(t, "-c", cfg, "count", "-by", "assignee", "-q", "priority=high", "-json")
	var js struct {
		By     string
		Total  int
		Counts []struct {
			Key   string
			Count int
		}
	}
	if err := json.Unmarshal([]byte(out), &js); err != nil || js.By != "assignee" || js.Total != 3 || js.Counts[0].Key != "ann" || js.Counts[0].Count != 2 {
		t.Fatalf("json: %v %+v\n%s", err, js, out)
	}
	for name, args := range map[string][]string{"bad -by": {"-by", "colour"}, "bad -status": {"-status", "nowhere"}, "stray arg": {"extra"}} {
		if _, _, code := count(args...); code != exitUsage {
			t.Errorf("%s: code %d", name, code)
		}
	}
}

func TestEditLabelsAddRemove(t *testing.T) {
	cases := []struct {
		in      string
		add, rm []string
		want    string
	}{
		{"", []string{"ui"}, nil, "ui"},
		{"ui,api", []string{"Ops"}, nil, "ui,api,Ops"},
		{"ui,api", []string{"UI"}, nil, "ui,api"},    // duplicate (case-insensitive): first spelling wins
		{"ui,api,ui", nil, []string{"API"}, "ui"},    // removal ignores case; stored duplicates collapse
		{" ui , api ", nil, nil, "ui,api"},           // whitespace tidied
		{"ui", []string{"x"}, []string{"ui"}, "x"},   // remove first, then add
		{"ui", []string{"ui"}, []string{"ui"}, "ui"}, // removed and re-added: stays
		{"ui", nil, []string{"zzz"}, "ui"},
	}
	for _, c := range cases {
		if got := editLabels(c.in, c.add, c.rm); got != c.want {
			t.Errorf("editLabels(%q, +%v, -%v) = %q, want %q", c.in, c.add, c.rm, got, c.want)
		}
	}
}

func TestEditLabelFlagsAndLabelsCommand(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status,labels\n1,A,,pending,\"ui,api\"\n2,B,,pending,ui\n", "")
	if code, _, e := do(t, "-c", cfg, "edit", "1", "-add-label", "ops,Ci", "-rm-label", "api"); code != exitOK {
		t.Fatalf("edit: %d %s", code, e)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), `"ui,ops,Ci"`) {
		t.Fatalf("labels not updated:\n%s", data)
	}
	// repeatable flags and combination with -labels (replace first, then add)
	if code, _, e := do(t, "-c", cfg, "edit", "2", "-labels", "x", "-add-label", "y", "-add-label", "z"); code != exitOK {
		t.Fatalf("edit: %d %s", code, e)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), `"x,y,z"`) {
		t.Fatalf("repeatable add-label failed:\n%s", data)
	}
	_, out, _ := do(t, "-c", cfg, "labels")
	for _, want := range []string{"LABEL", "ui", "ops", "Ci", "x", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("labels output lacks %q:\n%s", want, out)
		}
	}
	// only label flags is a valid edit (nothing else given)
	if code, _, _ := do(t, "-c", cfg, "edit", "1", "-rm-label", "ui"); code != exitOK {
		t.Fatalf("label-only edit rejected: %d", code)
	}
}

func TestListFieldsSortLimit(t *testing.T) {
	csv := "id,title,description,status,priority,labels,points\n" +
		"1,Alpha,,pending,low,ui,3\n2,Beta,,pending,high,\"ui,api\",10\n3,Gamma,,done,high,,2\n"
	_, _, cfg := setup(t, csv, "")
	list := func(args ...string) (string, string, int) {
		code, out, e := do(t, append([]string{"-c", cfg, "list"}, args...)...)
		return out, e, code
	}
	out, _, _ := list("-tsv", "-fields", "id,title,labels,points")
	if out != "1\tAlpha\tui\t3\n2\tBeta\tui,api\t10\n3\tGamma\t\t2\n" {
		t.Fatalf("tsv fields: %q", out)
	}
	out, _, _ = list("-fields", "id,points")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 4 || strings.Fields(lines[0])[0] != "ID" || strings.Fields(lines[0])[1] != "POINTS" {
		t.Fatalf("table fields:\n%s", out)
	}
	out, _, _ = list("-ids", "-sort", "-priority,-id") // text order: low > high, so descending puts low first
	if out != "csv1:1\ncsv1:3\ncsv1:2\n" {
		t.Fatalf("sort -priority,id: %q", out)
	}
	out, _, _ = list("-ids", "-sort", "points") // numbers sort numerically
	if out != "csv1:3\ncsv1:1\ncsv1:2\n" {
		t.Fatalf("sort points: %q", out)
	}
	out, _, _ = list("-ids", "-sort", "-points", "-limit", "2")
	if out != "csv1:2\ncsv1:1\n" {
		t.Fatalf("limit: %q", out)
	}
	for name, args := range map[string][]string{
		"unknown field": {"-fields", "id,nope"}, "unknown sort": {"-sort", "nope"},
		"fields with json": {"-json", "-fields", "id"}, "negative limit": {"-limit", "-1"},
	} {
		if _, _, code := list(args...); code != exitUsage {
			t.Errorf("%s: code %d", name, code)
		}
	}
	if _, e, _ := list("-fields", "titel"); !strings.Contains(e, `did you mean "title"`) {
		t.Errorf("no hint: %q", e)
	}
}

func TestAddJSONAndLongTextSources(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status,notes\n", "")
	old := stdin
	defer func() { stdin = old }()

	stdin = strings.NewReader("line one\nline two\n")
	code, out, e := do(t, "-c", cfg, "add", "-title", "T", "-description", "-", "-json")
	if code != exitOK {
		t.Fatalf("add: %d %s", code, e)
	}
	var got struct {
		ID, Description, Column string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.ID != "1" || got.Description != "line one\nline two" || got.Column == "" {
		t.Fatalf("add -json: %v %+v\n%s", err, got, out)
	}

	notes := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notes, []byte("from a file\nsecond"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, e = do(t, "-c", cfg, "add", "-title", "U", "-notes-file", notes); code != exitOK {
		t.Fatalf("notes-file: %d %s", code, e)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "from a file\nsecond") {
		t.Fatalf("notes not stored:\n%s", data)
	}
	if code, _, _ = do(t, "-c", cfg, "add", "-title", "V", "-notes", "x", "-notes-file", notes); code != exitUsage {
		t.Fatalf("both -notes and -notes-file: %d", code)
	}
	stdin = strings.NewReader("x")
	if code, _, _ = do(t, "-c", cfg, "add", "-title", "W", "-description", "-", "-notes", "-"); code != exitUsage {
		t.Fatalf("two stdin reads: %d", code)
	}
	if code, _, _ = do(t, "-c", cfg, "add", "-title", "X", "-description-file", filepath.Join(t.TempDir(), "missing")); code != exitUsage {
		t.Fatalf("missing file: %d", code)
	}
}

func TestEditAndSetStatusJSONAndStdin(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status,notes\n1,A,,pending,\n2,B,,done,\n", "")
	old := stdin
	defer func() { stdin = old }()
	stdin = strings.NewReader("multi\nline note\n")
	code, out, e := do(t, "-c", cfg, "edit", "1", "-notes", "-", "-json")
	if code != exitOK {
		t.Fatalf("edit: %d %s", code, e)
	}
	var task struct{ ID, Notes, Column string }
	if err := json.Unmarshal([]byte(out), &task); err != nil || task.Notes != "multi\nline note" || task.Column != "pending" {
		t.Fatalf("edit -json: %v %+v\n%s", err, task, out)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "multi\nline note") {
		t.Fatalf("notes not stored:\n%s", data)
	}
	// only -json is not an edit
	if code, _, _ := do(t, "-c", cfg, "edit", "1", "-json"); code != exitUsage {
		t.Fatalf("-json alone: %d", code)
	}
	code, out, _ = do(t, "-c", cfg, "set-status", "1", "2", "done", "-json")
	var res []struct {
		ID      string
		Column  string
		Changed bool
	}
	if code != exitOK || json.Unmarshal([]byte(out), &res) != nil || len(res) != 2 || !res[0].Changed || res[1].Changed || res[0].Column != "done" {
		t.Fatalf("set-status -json: %d %+v\n%s", code, res, out)
	}
}

func TestCompletionScriptsCompleteFlagsAndValues(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		_, out, _ := do(t, "completion", sh)
		for _, want := range []string{"-status", "-tsv", "-add-label", "__complete columns", "__complete backends", "__complete ids", "set-status", "filter"} {
			if sh == "fish" && strings.HasPrefix(want, "-") { // fish spells long-ish options "-o name"
				want = "-o " + want[1:]
			}
			if sh == "fish" && want == "-o status" {
				want = "__fish_prev_arg_in -status"
			}
			if !strings.Contains(out, want) {
				t.Errorf("%s script lacks %q", sh, want)
			}
		}
		if strings.Contains(out, "__complete \"") || strings.Contains(strings.SplitN(out, "__complete", 2)[0], " __complete ") {
			t.Errorf("%s: hidden command must not be offered as a subcommand", sh)
		}
	}
	// flags come from each command's FlagSet, so a new flag shows up without touching the script
	for _, c := range commandTable() {
		if c.name == "list" {
			flags := strings.Join(commandFlags(c), " ")
			for _, f := range []string{"json", "ids", "fields", "sort", "limit", "q", "v", "status"} {
				if !strings.Contains(" "+flags+" ", " "+f+" ") {
					t.Errorf("list flags %q lack %s", flags, f)
				}
			}
		}
	}
}

func TestHiddenCompleteCommand(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status\n1,A,,pending\n2,B,,done\n", "")
	_, out, _ := do(t, "-c", cfg, "__complete", "columns")
	if !strings.Contains(out, "pending\n") || !strings.Contains(out, "done\n") {
		t.Fatalf("columns: %q", out)
	}
	if _, out, _ = do(t, "-c", cfg, "__complete", "ids"); out != "1\n2\n" {
		t.Fatalf("ids: %q", out)
	}
	if _, out, _ = do(t, "-c", cfg, "__complete", "backends"); out != "csv1\n" {
		t.Fatalf("backends: %q", out)
	}
	if code, out, _ := do(t, "-c", cfg, "__complete", "bogus"); code != exitOK || out != "" {
		t.Fatalf("unknown kind: %d %q", code, out)
	}
	if _, out, _ := do(t, "help"); strings.Contains(out, "__complete") {
		t.Fatal("hidden command listed in help")
	}
}

func TestConfigFlagDoesNotStealFlagValues(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status,notes\n1,A,,pending,\n", "")
	// "-c" as the value of -title / -notes is data, not the config flag
	if code, _, e := do(t, "-c", cfg, "add", "-title", "-c"); code != exitOK {
		t.Fatalf("add -title -c: %d %s", code, e)
	}
	if code, _, e := do(t, "-c", cfg, "edit", "1", "-notes", "-c", "-title", "B"); code != exitOK {
		t.Fatalf("edit -notes -c: %d %s", code, e)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "\n2,-c,") || !strings.Contains(string(data), "1,B,,pending,-c") {
		t.Fatalf("values lost:\n%s", data)
	}
	// the real flag after the subcommand still works, also next to such values
	if code, out, e := do(t, "list", "-q", "title:B", "-c", cfg, "-ids"); code != exitOK || out != "csv1:1\n" {
		t.Fatalf("list -c: %d %q %s", code, out, e)
	}
	// bool single-letter flag does not swallow the next token
	if code, out, _ := do(t, "delete", "-n", "-c", cfg, "1"); code != exitOK || !strings.Contains(out, "would delete") {
		t.Fatalf("delete -n -c: %d %q", code, out)
	}
}

func TestValueFlagsAreDetectedFromUsage(t *testing.T) {
	for name, want := range map[string]map[string]bool{
		"add":    {"title": true, "b": true, "json": false, "status": true},
		"delete": {"n": false, "b": true},
		"edit":   {"add-label": true, "json": false, "notes": true},
		"list":   {"limit": true, "ids": false, "q": true},
	} {
		c, _ := findCommand(name)
		got := valueFlags(c)
		for flag, takes := range want {
			if got[flag] != takes {
				t.Errorf("%s -%s: takes value = %v, want %v (all: %v)", name, flag, got[flag], takes, got)
			}
		}
	}
}

func TestUsageAndDocsListEveryTaskField(t *testing.T) {
	// the add/edit synopsis in the usage text must carry every field flag, so
	// adding a field to taskFields without updating the text fails here
	var addLine, editLine string
	for _, line := range strings.Split(usage, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "tkan [-c CONFIG] add "):
			addLine = t
		case strings.HasPrefix(t, "tkan [-c CONFIG] edit "):
			editLine = t
		}
	}
	for _, f := range taskFields {
		for name, line := range map[string]string{"add": addLine, "edit": editLine} {
			if !strings.Contains(line, "-"+f.name+" ") {
				t.Errorf("usage of %s lacks -%s: %q", name, f.name, line)
			}
		}
	}
	// the generated usage and the flags really registered agree
	_, _, cfg := setup(t, "id,title,status\n1,a,pending\n", "")
	for _, name := range []string{"add", "edit"} {
		_, _, e := do(t, "-c", cfg, name)
		for _, f := range taskFields {
			if !strings.Contains(e, "-"+f.name+" "+f.meta) {
				t.Errorf("%s error usage lacks -%s %s: %q", name, f.name, f.meta, e)
			}
		}
	}
}

// stubBackend is a minimal backend for tests that need a store that fails.
type stubBackend struct {
	tasks []task.Task
	err   error
}

func (s stubBackend) List(context.Context) ([]task.Task, error) { return s.tasks, s.err }
func (s stubBackend) Statuses() []string {
	var out []string
	for _, t := range s.tasks {
		out = append(out, t.Status)
	}
	return out
}
func (s stubBackend) Update(context.Context, task.Task) error { return s.err }
func (s stubBackend) Create(context.Context, task.Task) (task.Task, error) {
	return task.Task{}, s.err
}
func (s stubBackend) Delete(context.Context, string) error { return s.err }
func (s stubBackend) Close() error                         { return nil }

func TestListWarnsAndExitsOneWhenABackendFails(t *testing.T) {
	a := &app{
		be: backend.NewSet([]backend.Named{
			{Name: "healthy", Type: "stub", Backend: stubBackend{tasks: []task.Task{{ID: "1", Title: "Healthy", Status: "pending"}}}},
			{Name: "remote", Type: "stub", Backend: stubBackend{err: errors.New("tool is down")}},
		}),
		spec: board.DefaultSpec(),
	}
	for _, args := range [][]string{{"-ids"}, {"-json"}, {}} {
		var out, e bytes.Buffer
		code := cmdList(context.Background(), a, args, &out, &e)
		if code != exitError || !strings.Contains(out.String(), "1") || !strings.Contains(e.String(), "warning") || !strings.Contains(e.String(), "remote") || !strings.Contains(e.String(), "tool is down") {
			t.Errorf("list %v: code %d out %q err %q", args, code, out.String(), e.String())
		}
	}
	var out, e bytes.Buffer
	if code := cmdCount(context.Background(), a, nil, &out, &e); code != exitError || !strings.Contains(out.String(), "TOTAL") {
		t.Errorf("count: code %d out %q", code, out.String())
	}
}

func TestConfigCommandShowsEffectiveSettings(t *testing.T) {
	yml := `backend:
  - type: csv
    options: {source: work.csv}
  - {type: custom, name: remote, options: {command: [./tool.sh]}}
board:
  unmapped: hide
  search: [title]
  group: priority
  views:
    - {name: urgent, query: "priority=high"}
  columns:
    - {name: Todo, values: [pending], limit: 2, color: "75"}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "work.csv"), []byte("id,title,description,status,priority\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte("#!/bin/sh\necho '{\"tasks\":[]}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "tkan.yaml")
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, e := do(t, "-c", cfg, "config")
	if code != exitOK {
		t.Fatalf("%d %s", code, e)
	}
	for _, want := range []string{"config: " + cfg, "name: csv1", "name: remote", "type: custom", "command:", "unmapped: hide", "search:", "group: priority", "name: urgent", "limit: 2", `color: "75"`} {
		if !strings.Contains(out, want) {
			t.Errorf("config output lacks %q:\n%s", want, out)
		}
	}
	// the default unmapped mode is spelled out
	_, _, cfg2 := setup(t, "id,title,description,status\n", "")
	if _, out, _ = do(t, "-c", cfg2, "config"); !strings.Contains(out, "unmapped: append") {
		t.Fatalf("default unmapped missing:\n%s", out)
	}
}

func TestVersionAndHelpIgnoreABrokenConfig(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(bad, []byte("backend: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-c", bad, "version"}, {"-c", bad, "-version"}, {"-c", bad, "help"}, {"-c", "/no/such/file.yaml", "version"}} {
		if code, _, e := do(t, args...); code != exitOK {
			t.Errorf("%v: code %d (%s)", args, code, e)
		}
	}
	if code, _, _ := do(t, "-c", bad, "list"); code != exitError {
		t.Errorf("list with a broken config must fail with 1, got %d", code)
	}
}

func TestEmptyListIsAnEmptyJSONArray(t *testing.T) {
	_, _, cfg := setup(t, "id,title,description,status\n1,A,,pending\n", "")
	code, out, _ := do(t, "-c", cfg, "list", "-q", "title:zzzz", "-json")
	if code != exitOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("got %d %q, want []", code, out)
	}
}

func TestShowWithBackendFlagAndPrefix(t *testing.T) {
	dir := t.TempDir()
	for name, title := range map[string]string{"a.csv": "From A", "b.csv": "From B"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("id,title,description,status\n1,"+title+",,pending\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "tkan.yaml")
	y := "backend:\n  - {type: csv, name: a, options: {source: a.csv}}\n  - {type: csv, name: b, options: {source: b.csv}}\n"
	if err := os.WriteFile(cfg, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, e := do(t, "-c", cfg, "show", "1"); code != exitUsage || !strings.Contains(e, "several backends") {
		t.Fatalf("ambiguous id: %d %q", code, e)
	}
	if code, out, _ := do(t, "-c", cfg, "show", "1", "-b", "b"); code != exitOK || !strings.Contains(out, "From B") {
		t.Fatalf("-b: %d %q", code, out)
	}
	if code, out, _ := do(t, "-c", cfg, "show", "A:1"); code != exitOK || !strings.Contains(out, "From A") {
		t.Fatalf("NAME:ID: %d %q", code, out)
	}
	if code, _, _ := do(t, "-c", cfg, "show", "-b", "zzz", "1"); code == exitOK {
		t.Fatal("unknown backend accepted")
	}
}

func TestDefaultBackendFromConfigAndEnv(t *testing.T) {
	dir := t.TempDir()
	for name, title := range map[string]string{"a.csv": "From A", "b.csv": "From B"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("id,title,description,status\n1,"+title+",,pending\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "tkan.yaml")
	write := func(extra string) {
		y := extra + "backend:\n  - {type: csv, name: a, options: {source: a.csv}}\n  - {type: csv, name: b, options: {source: b.csv}}\n"
		if err := os.WriteFile(cfg, []byte(y), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("default_backend: B\n") // case-insensitive
	if code, out, e := do(t, "-c", cfg, "show", "1"); code != exitOK || !strings.Contains(out, "From B") {
		t.Fatalf("bare id with a default: %d %q %s", code, out, e)
	}
	if _, out, _ := do(t, "-c", cfg, "show", "a:1"); !strings.Contains(out, "From A") {
		t.Fatalf("NAME:ID must beat the default: %q", out)
	}
	if _, out, _ := do(t, "-c", cfg, "show", "1", "-b", "a"); !strings.Contains(out, "From A") {
		t.Fatalf("-b must beat the default: %q", out)
	}
	if _, out, _ := do(t, "-c", cfg, "add", "-title", "New"); strings.TrimSpace(out) != "b:2" {
		t.Fatalf("add goes to the default backend: %q", out)
	}
	if _, out, _ := do(t, "-c", cfg, "add", "-title", "New A", "-b", "a"); strings.TrimSpace(out) != "a:2" {
		t.Fatalf("add -b: %q", out)
	}
	t.Setenv("TKAN_BACKEND", "a") // the environment overrides the config
	if _, out, _ := do(t, "-c", cfg, "show", "1"); !strings.Contains(out, "From A") {
		t.Fatalf("env default: %q", out)
	}
	t.Setenv("TKAN_BACKEND", "nope")
	if code, _, e := do(t, "-c", cfg, "list"); code != exitError || !strings.Contains(e, `default backend "nope"`) {
		t.Fatalf("unknown env default: %d %q", code, e)
	}
	t.Setenv("TKAN_BACKEND", "")
	write("default_backend: zzz\n")
	if code, _, e := do(t, "-c", cfg, "list"); code != exitError || !strings.Contains(e, "default_backend") {
		t.Fatalf("unknown config default: %d %q", code, e)
	}
	write("") // no default: ambiguity is reported as before
	if code, _, _ := do(t, "-c", cfg, "show", "1"); code != exitUsage {
		t.Fatalf("no default: %d", code)
	}
}

func TestCommandBackendRunsOncePerListCommand(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho x >> calls.log\ncat > /dev/null\necho '{\"tasks\":[{\"id\":\"1\",\"title\":\"One\",\"status\":\"pending\",\"priority\":\"high\"}]}'\n"
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "tkan.yaml")
	// search and sort force the start-up field check that used to list a second time
	y := "backend:\n  - {type: custom, options: {command: [./tool.sh]}}\nboard:\n  search: [title]\n  sort: [priority]\n"
	if err := os.WriteFile(cfg, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
		return strings.Count(string(data), "x")
	}
	if code, _, e := do(t, "-c", cfg, "list", "-ids"); code != exitOK {
		t.Fatalf("list: %d %s", code, e)
	}
	if n := calls(); n != 1 {
		t.Fatalf("the tool ran %d times for one list, want 1", n)
	}
	os.Remove(filepath.Join(dir, "calls.log"))
	if code, _, e := do(t, "-c", cfg, "show", "1"); code != exitOK {
		t.Fatalf("show: %d %s", code, e)
	}
	if n := calls(); n != 1 {
		t.Fatalf("the tool ran %d times for one show, want 1", n)
	}
}

func TestAddRejectsStartAfterDueAsUsageError(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status\n", "")
	before, _ := os.ReadFile(file)
	code, _, e := do(t, "-c", cfg, "add", "-title", "x", "-start", "2026-06-01", "-due", "2026-05-01")
	if code != exitUsage || !strings.Contains(e, "after due") {
		t.Fatalf("code %d err %q", code, e)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("an invalid task was written")
	}
}

// The board polls and reloads through Set.List, so open must not cache lists
// for it (it did once: external edits never showed up).
func TestOpenForBoardSeesExternalEdits(t *testing.T) {
	_, file, cfg := setup(t, "id,title,status\n1,A,pending\n", "")
	a, err := open(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.be.Close()
	ctx := context.Background()
	if ts, _ := a.be.List(ctx); len(ts) != 1 {
		t.Fatalf("got %d tasks, want 1", len(ts))
	}
	if err := os.WriteFile(file, []byte("id,title,status\n1,A,pending\n2,B,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ts, _ := a.be.List(ctx); len(ts) != 2 {
		t.Fatalf("board list is stale: got %d tasks, want 2", len(ts))
	}
}

func TestAuditTrailOfCLIWrites(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status\n1,A,pending\n2,B,pending\n", baseYAML+"audit: {file: audit.jsonl}\n")
	for _, args := range [][]string{
		{"-c", cfg, "set-status", "1", "done"},
		{"-c", cfg, "add", "-title", "New one"},
		{"-c", cfg, "delete", "2"},
		{"-c", cfg, "show", "99"}, // not found: no entry, read only
	} {
		do(t, args...)
	}
	code, out, errs := do(t, "-c", cfg, "audit")
	if code != exitOK || errs != "" {
		t.Fatalf("audit: %d %q", code, errs)
	}
	for _, want := range []string{"update", "create", "delete", "status=done", `"New one"`} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output lacks %q:\n%s", want, out)
		}
	}
	// Each command is its own session, so the last one before this run is "previous".
	if !strings.Contains(out, "previous") {
		t.Errorf("no previous session marker:\n%s", out)
	}
	_, out, _ = do(t, "-c", cfg, "audit", "-q", "op=delete")
	if strings.Contains(out, "update") || !strings.Contains(out, "delete") {
		t.Errorf("filter op=delete:\n%s", out)
	}
	code, out, _ = do(t, "-c", cfg, "audit", "-limit", "1", "-json")
	var rows []struct {
		Op          string `json:"op"`
		SessionKind string `json:"session_kind"`
	}
	if code != exitOK || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Op != "delete" {
		t.Fatalf("-limit 1 -json: %d %s", code, out)
	}
	if code, _, errs = do(t, "-c", cfg, "audit", "-q", "nope=1"); code != exitUsage || !strings.Contains(errs, "unknown field") {
		t.Errorf("bad query: %d %q", code, errs)
	}
}

func TestAuditWithoutFileWarns(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status\n1,A,pending\n", "")
	code, _, errs := do(t, "-c", cfg, "audit")
	if code != exitOK || !strings.Contains(errs, "audit.file") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestAuditFileExtensionIsValidated(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status\n1,A,pending\n", baseYAML+"audit: {file: audit.log}\n")
	if code, _, errs := do(t, "-c", cfg, "list"); code != exitError || !strings.Contains(errs, "audit.file") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestInvalidPollOptionIsRejected(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status\n1,A,pending\n", "backend:\n  - type: csv\n    options: {source: work.csv, poll: fast}\n")
	code, _, errs := do(t, "-c", cfg, "list")
	if code != exitError || !strings.Contains(errs, "poll must be") {
		t.Fatalf("%d %q", code, errs)
	}
}
