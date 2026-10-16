package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	_ "tkan/internal/backend/csv"
	_ "tkan/internal/backend/custom"
	"tkan/internal/board"
)

func gen(t *testing.T, csv, out string) (string, []string, *Config) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data.csv")
	if csv != "" {
		if err := os.WriteFile(data, []byte(csv), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outPath := filepath.Join(dir, out)
	text, notes, err := Generate(InitOptions{Backend: "csv", Location: data, Output: outPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(outPath) // must parse and validate
	if err != nil {
		t.Fatalf("generated config does not load: %v\n%s", err, text)
	}
	return text, notes, cfg
}

func names(spec board.Spec) []string {
	var n []string
	for _, c := range spec.Columns {
		n = append(n, c.Name)
	}
	return n
}

func TestGenerateFromDefaultWorkflowFile(t *testing.T) {
	text, notes, cfg := gen(t, "id,title,description,status,priority,notes,type\n1,a,,pending,,,\n2,b,,done,,,\n3,c,,in_progress,,,\n", "data.csv.tkan.yaml")
	if len(notes) != 0 {
		t.Fatalf("notes: %v", notes)
	}
	if got := names(cfg.Board); !reflect.DeepEqual(got, []string{"pending", "in_progress", "done", "failed"}) {
		t.Fatalf("columns: %v\n%s", got, text)
	}
	if len(cfg.Backends) != 1 || cfg.Backends[0].Type != "csv" || cfg.Backends[0].Name != "csv1" {
		t.Fatalf("backends: %+v", cfg.Backends)
	}
	for _, f := range []string{"id: id", "title: title", "status: status", "description: description", "type: type", "- type: csv", "options:"} {
		if !strings.Contains(text, f) {
			t.Errorf("missing %q in\n%s", f, text)
		}
	}
	if !strings.Contains(text, "source: data.csv") {
		t.Errorf("source should be relative:\n%s", text)
	}
}

func TestGenerateSynonymsAndOrdering(t *testing.T) {
	text, _, cfg := gen(t, "Key,Summary,State,Reporter\nA-1,x,Closed,ann\nA-2,y,in review,bob\nA-3,z,Open,\nA-4,w,Weird,\nA-5,v,open,\n", "c.yaml")
	for _, f := range []string{"id: Key", "title: Summary", "status: State", "kept untouched: Reporter"} {
		if !strings.Contains(text, f) {
			t.Errorf("missing %q in\n%s", f, text)
		}
	}
	// ranked workflow first, unknown after; case-insensitive duplicates merged; no canonical backfill.
	want := []string{"Open", "in review", "Closed", "Weird"}
	if got := names(cfg.Board); !reflect.DeepEqual(got, want) {
		t.Fatalf("columns %v want %v\n%s", got, want, text)
	}
}

func TestGenerateMissingRequiredField(t *testing.T) {
	text, notes, _ := gen(t, "id,title,owner\n1,a,ann\n", "c.yaml")
	if len(notes) != 1 || !strings.Contains(notes[0], `"status"`) {
		t.Fatalf("notes: %v", notes)
	}
	if !strings.Contains(text, "# status: <column name or position>   # TODO") {
		t.Fatalf("expected TODO placeholder:\n%s", text)
	}
	if got := names(mustLoad(t, text).Board); len(got) != 4 {
		t.Fatalf("default columns expected, got %v", got)
	}
}

func mustLoad(t *testing.T, text string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.yaml")
	_ = os.WriteFile(p, []byte(text), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerateNoDataFileUsesGenericTemplate(t *testing.T) {
	text, notes, cfg := gen(t, "", "tkan.yaml")
	if len(notes) != 1 || !strings.Contains(notes[0], "no data file yet") {
		t.Fatalf("notes: %v", notes)
	}
	if !strings.Contains(text, "header: true") || len(cfg.Board.Columns) != 4 {
		t.Fatalf("%s", text)
	}
}

func TestGeneratedConfigDrivesBackend(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "issues.csv")
	_ = os.WriteFile(data, []byte("key,summary,state\nA-1,x,open\nA-2,y,done\n"), 0o644)
	out := filepath.Join(dir, DefaultFile)
	text, _, err := Generate(InitOptions{Backend: "csv", Location: data, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(out, []byte(text), 0o644)
	cfg, err := Load(out)
	if err != nil {
		t.Fatal(err)
	}
	be, err := cfg.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	b, err := board.New(cfg.Board, be.Statuses())
	if err != nil || b.ColumnOf("open") != 0 || b.ColumnOf("done") != 1 {
		t.Fatalf("%v %+v", err, b.Columns)
	}
}

func TestUnknownBackend(t *testing.T) {
	if _, _, err := Generate(InitOptions{Backend: "nope"}); err == nil || !strings.Contains(err.Error(), "supported: csv, custom") {
		t.Fatalf("got %v", err)
	}
}

func TestGenerateCustomBackendLoads(t *testing.T) {
	text, _, err := Generate(InitOptions{Backend: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, text)
	if len(cfg.Backends) != 1 || cfg.Backends[0].Type != "custom" {
		t.Fatalf("%+v\n%s", cfg.Backends, text)
	}
}

func TestQuoteKeepsYAMLSpecialWordsAsStrings(t *testing.T) {
	for _, w := range []string{"null", "Null", "NULL", "~", "true", "No", "off", "123", "1.5", "0x1f", "in_progress", "todo"} {
		var got struct{ V []string }
		if err := yaml.Unmarshal([]byte("v: ["+quote(w)+"]\n"), &got); err != nil || len(got.V) != 1 || got.V[0] != w {
			t.Errorf("%q -> %q decoded as %q (%v)", w, quote(w), got.V, err)
		}
	}
}

func TestGenerateOrdersMoreWorkflowWords(t *testing.T) {
	text, _, cfg := gen(t, "id,title,status\n1,a,Released\n2,b,on-hold\n3,c,In-Progress\n4,d,Triage\n5,e,Weird\n6,f,Ready\n7,g,won't do\n8,h,Verify\n", "c.yaml")
	want := []string{"Triage", "Ready", "In-Progress", "Verify", "Released", "on-hold", "won't do", "Weird"}
	if got := names(cfg.Board); !reflect.DeepEqual(got, want) {
		t.Fatalf("columns %v want %v\n%s", got, want, text)
	}
}
