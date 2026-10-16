package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadBackendListAndGeneratedNames(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "tkan.yaml", `
backend:
  - type: csv
    options:
      source: data/items.csv
      fields: {status: state}
  - type: CSV
    name: second
    options: {source: b.csv}
  - type: custom
    options: {command: [./x]}
board:
  unmapped: other
  columns:
    - name: Todo
      values: [open, new]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range c.Backends {
		names = append(names, e.Type+"/"+e.Name)
	}
	if got := strings.Join(names, " "); got != "csv/csv1 csv/second custom/custom3" {
		t.Fatalf("names: %s", got)
	}
	if c.Backends[0].Options.Kind == 0 {
		t.Fatal("options missing")
	}
	if len(c.Board.Columns) != 1 || c.Board.Unmapped != "other" {
		t.Fatalf("%+v", c)
	}
}

func TestLoadRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"missing type":   "backend:\n  - options: {source: a.csv}\n",
		"duplicate name": "backend:\n  - {type: csv, name: a}\n  - {type: csv, name: A}\n",
		"colon in name":  "backend:\n  - {type: csv, name: 'a:b'}\n",
		"newline":        "backend:\n  - {type: csv, name: \"a\\nb\"}\n",
		"comma":          "backend:\n  - {type: csv, name: 'a,b'}\n",
		"slash":          "backend:\n  - {type: csv, name: 'a/b'}\n",
		"inner space":    "backend:\n  - {type: csv, name: 'a b'}\n",
	}
	for what, y := range cases {
		p := write(t, t.TempDir(), "c.yaml", y)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected an error", what)
		}
	}
	for _, name := range []string{"work", "team-1", "my.csv", "under_score", "équipe"} {
		p := write(t, t.TempDir(), "c.yaml", "backend:\n  - {type: csv, name: "+name+"}\n")
		if _, err := Load(p); err != nil {
			t.Errorf("name %q rejected: %v", name, err)
		}
	}
}

func TestLegacyFormatIsRejected(t *testing.T) {
	for _, y := range []string{
		"backend: csv\nsource: a.csv\n",
		"source: a.csv\n",
		"csv:\n  header: true\n",
	} {
		p := write(t, t.TempDir(), "c.yaml", y)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "docs/config.md") {
			t.Errorf("%q: got %v", y, err)
		}
	}
}

func TestLoadRejectsInvalidBoard(t *testing.T) {
	p := write(t, t.TempDir(), "c.yaml", "board:\n  columns:\n    - {name: a, values: [x]}\n    - {name: b, values: [x]}\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "mapped to both") {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyFileLoadsButHasNoBackends(t *testing.T) {
	p := write(t, t.TempDir(), "c.yaml", "")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(); err == nil || !strings.Contains(err.Error(), "no backends") {
		t.Fatalf("got %v", err)
	}
}

func TestLocate(t *testing.T) {
	if p, err := Locate("x.yaml"); err != nil || p != "x.yaml" {
		t.Fatalf("%q %v", p, err)
	}
	t.Setenv("HOME", t.TempDir()) // never pick up a real user config
	t.Chdir(t.TempDir())
	if _, err := Locate(""); err == nil || !strings.Contains(err.Error(), "tkan init") {
		t.Fatalf("got %v", err)
	}
	write(t, ".", DefaultFile, "")
	if p, err := Locate(""); err != nil || p != DefaultFile {
		t.Fatalf("%q %v", p, err)
	}
}

func TestOtherYAMLErrorsHaveNoLegacyHint(t *testing.T) {
	p := write(t, t.TempDir(), "c.yaml", "backend:\n  - type: csv\nboard:\n  nope: 1\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "nope") || strings.Contains(err.Error(), "docs/config.md") {
		t.Fatalf("got %v", err)
	}
}

func TestLocateFallsBackToHomeOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	// a config in a parent directory is not picked up
	write(t, root, DefaultFile, "")
	if _, err := Locate(""); err == nil {
		t.Fatal("found a config in a parent directory")
	}
	// neither are ~/.config/tkan/ or non-dotted names in the home directory
	if err := os.MkdirAll(filepath.Join(home, ".config", "tkan"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".config", "tkan"), DefaultFile, "")
	write(t, home, DefaultFile, "")
	if _, err := Locate(""); err == nil {
		t.Fatal("found a config outside the lookup locations")
	}
	// ~/.tkan.yml, then ~/.tkan.yaml wins over it
	write(t, home, ".tkan.yml", "")
	if p, err := Locate(""); err != nil || p != filepath.Join(home, ".tkan.yml") {
		t.Fatalf("home .tkan.yml: %q %v", p, err)
	}
	write(t, home, ".tkan.yaml", "")
	if p, err := Locate(""); err != nil || p != filepath.Join(home, ".tkan.yaml") {
		t.Fatalf("home .tkan.yaml: %q %v", p, err)
	}
	// and the current directory wins over home
	write(t, ".", DefaultFile, "")
	if p, err := Locate(""); err != nil || p != DefaultFile {
		t.Fatalf("cwd config: %q %v", p, err)
	}
}

func TestLocatePrefersDottedNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	write(t, ".", "tkan.yaml", "")
	write(t, ".", ".tkan.yml", "")
	if p, err := Locate(""); err != nil || p != ".tkan.yml" {
		t.Fatalf("got %q %v, want .tkan.yml", p, err)
	}
}

func TestLocateAcceptsAllNamesInPriorityOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	// add files from lowest to highest priority; the newest must always win
	for i := len(ConfigNames) - 1; i >= 0; i-- {
		write(t, ".", ConfigNames[i], "")
		if p, err := Locate(""); err != nil || p != ConfigNames[i] {
			t.Fatalf("with %v present: got %q %v, want %q", ConfigNames[i:], p, err, ConfigNames[i])
		}
	}
}

func TestLocateIgnoresDirectoriesNamedLikeAConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if err := os.Mkdir(DefaultFile, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, ".", "tkan.yml", "")
	if p, err := Locate(""); err != nil || p != "tkan.yml" {
		t.Fatalf("got %q %v", p, err)
	}
}

func samePath(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra == rb
}

func TestViewsAreValidatedAtLoad(t *testing.T) {
	bad := map[string]string{
		"no name":   "backend:\n  - {type: csv}\nboard:\n  views:\n    - {query: x}\n",
		"no query":  "backend:\n  - {type: csv}\nboard:\n  views:\n    - {name: a}\n",
		"duplicate": "backend:\n  - {type: csv}\nboard:\n  views:\n    - {name: a, query: x}\n    - {name: A, query: y}\n",
		"syntax":    "backend:\n  - {type: csv}\nboard:\n  views:\n    - {name: a, query: 'title~('}\n",
	}
	for what, y := range bad {
		if _, err := Load(write(t, t.TempDir(), "c.yaml", y)); err == nil {
			t.Errorf("%s accepted", what)
		}
	}
	if _, err := Load(write(t, t.TempDir(), "c.yaml", "backend:\n  - {type: csv}\nboard:\n  views:\n    - {name: mine, query: 'assignee=ann col!:done'}\n")); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownKeysGetHints(t *testing.T) {
	cases := map[string][]string{
		"backend:\n  - {type: csv}\nboard:\n  unmaped: hide\n":                                   {`unknown key "unmaped" in board`, `did you mean "unmapped"`, "valid keys: columns"},
		"backend:\n  - {type: csv}\nboard:\n  column:\n    - {name: a}\n":                        {`unknown key "column"`, `did you mean "columns"`},
		"backend:\n  - {type: csv}\nboard:\n  columns:\n    - {name: a, limt: 2}\n":              {`in board.columns[]`, `did you mean "limit"`},
		"backend:\n  - {typ: csv}\n":                                                             {`in backend[]`, `did you mean "type"`},
		"backend:\n  - {type: csv}\nbaord: {}\n":                                                 {`in top level`, `did you mean "board"`},
		"backend:\n  - {type: csv}\nboard:\n  columns:\n    - {name: a, values: [x], zzzz: 1}\n": {`unknown key "zzzz"`, "valid keys: name, values"},
	}
	for y, wants := range cases {
		_, err := Load(write(t, t.TempDir(), "c.yaml", y))
		if err == nil {
			t.Errorf("%q accepted", y)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%q: error lacks %q: %v", y, w, err)
			}
		}
	}
}
