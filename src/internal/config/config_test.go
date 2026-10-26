package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tack/internal/action"
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
	p := write(t, dir, "tack.yaml", `
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
	if _, err := Locate(""); err == nil || !strings.Contains(err.Error(), "tack init") {
		t.Fatalf("got %v", err)
	}
	write(t, ".", DefaultFile, "")
	if p, err := Locate(""); err != nil || p != DefaultFile {
		t.Fatalf("%q %v", p, err)
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
	// neither are ~/.config/tack/ or non-dotted names in the home directory
	if err := os.MkdirAll(filepath.Join(home, ".config", "tack"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".config", "tack"), DefaultFile, "")
	write(t, home, DefaultFile, "")
	if _, err := Locate(""); err == nil {
		t.Fatal("found a config outside the lookup locations")
	}
	// ~/.tack.yml, then ~/.tack.yaml wins over it
	write(t, home, ".tack.yml", "")
	if p, err := Locate(""); err != nil || p != filepath.Join(home, ".tack.yml") {
		t.Fatalf("home .tack.yml: %q %v", p, err)
	}
	write(t, home, ".tack.yaml", "")
	if p, err := Locate(""); err != nil || p != filepath.Join(home, ".tack.yaml") {
		t.Fatalf("home .tack.yaml: %q %v", p, err)
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
	write(t, ".", "tack.yaml", "")
	write(t, ".", ".tack.yml", "")
	if p, err := Locate(""); err != nil || p != ".tack.yml" {
		t.Fatalf("got %q %v, want .tack.yml", p, err)
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
	write(t, ".", "tack.yml", "")
	if p, err := Locate(""); err != nil || p != "tack.yml" {
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

func TestBoardAgentsLoad(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.yaml", "backend:\n  - {type: csv}\nboard:\n  agents: [claude, codex]\n"))
	if err != nil || strings.Join(c.Board.Agents, ",") != "claude,codex" {
		t.Fatalf("%v %+v", err, c.Board.Agents)
	}
	if _, err := Load(write(t, t.TempDir(), "c.yaml", "backend:\n  - {type: csv}\nboard:\n  agents: ['']\n")); err == nil || !strings.Contains(err.Error(), "board.agents[0]") {
		t.Fatalf("empty agent name: %v", err)
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

func TestLoadActions(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "tack.yaml", `
backend:
  - {type: csv, options: {source: a.csv}}
actions:
  - name: notify
    command: [notify-send, "#{id}", "{title}"]
  - name: Claude
    key: c
    mode: Interactive
    command: [claude, "{json}"]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Actions) != 2 || c.Actions[0].Mode != "background" || c.Actions[1].Mode != "interactive" || c.Actions[0].Stdin != "none" {
		t.Fatalf("%+v", c.Actions)
	}
}

func TestLoadRejectsBadActions(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"unknown placeholder": {"actions:\n  - {name: a, command: [echo, '{nope}']}\n", "actions[1]: command[1]: unknown placeholder {nope}"},
		"duplicate":           {"actions:\n  - {name: a, command: [x]}\n  - {name: A, command: [x]}\n", "actions[2]: duplicate name"},
		"missing command":     {"actions:\n  - {name: a}\n", "command is required"},
		"typo key":            {"actions:\n  - {name: a, comand: [x]}\n", `unknown key "comand" in actions[]`},
	}
	for name, c := range cases {
		p := write(t, t.TempDir(), "tack.yaml", "backend:\n  - {type: csv, options: {source: a.csv}}\n"+c.yaml)
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestLoadRejectsBadHooks(t *testing.T) {
	const base = "backend:\n  - type: csv\n    options: {source: a.csv}\n"
	cases := map[string]struct{ yml, want string }{
		"unknown event":     {"hooks:\n  - {name: a, on: [task.moved], command: [x]}\n  - {name: b, on: [task.mooved], command: [x]}\n", "hooks[2]"},
		"missing on":        {"hooks:\n  - {name: a, command: [x]}\n", "hooks[1]: on is required"},
		"bad when":          {"hooks:\n  - {name: a, on: [task.moved], when: 'priorty=high', command: [x]}\n", "hooks[1]: when"},
		"duplicate":         {"hooks:\n  - {name: a, on: [task.moved], command: [x]}\n  - {name: A, on: [task.moved], command: [x]}\n", "hooks[2]: duplicate name"},
		"depth cap":         {"hook_max_depth: 4\n", "hook_max_depth"},
		"parallel cap":      {"hook_max_parallel: 65\n", "hook_max_parallel"},
		"bad unique":        {"hooks:\n  - {name: a, on: [task.moved], unique: card, command: [x]}\n", "hooks[1]: invalid unique"},
		"hook depth cap":    {"hooks:\n  - {name: a, on: [task.moved], max_depth: 9, command: [x]}\n", "hooks[1]"},
		"unknown key":       {"hooks:\n  - {name: a, on: [task.moved], comand: [x]}\n", "comand"},
		"interactive":       {"hooks:\n  - {name: a, on: [task.moved], mode: interactive, command: [x]}\n", "hooks[1]"},
		"unknown placehold": {"hooks:\n  - {name: a, on: [task.moved], command: [x, '{titel}']}\n", "hooks[1]"},
	}
	for name, c := range cases {
		p := write(t, t.TempDir(), "tack.yaml", base+c.yml)
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to mention %q", name, err, c.want)
		}
	}
	ok := write(t, t.TempDir(), "tack.yaml", base+"hooks:\n  - {name: a, on: [task.moved, reload.fail], command: [x, '{event}', '{old_status}']}\n")
	if c, err := Load(ok); err != nil || len(c.Hooks) != 1 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestActionMaxParallel(t *testing.T) {
	const base = "backend:\n  - {type: csv, options: {source: a.csv}}\n"
	cases := map[string]struct {
		yml  string
		want int    // effective limit when it loads
		err  string // or the error it must mention
	}{
		"default": {"", action.DefaultMaxParallel, ""},
		"set":     {"action_max_parallel: 5\n", 5, ""},
		"one":     {"action_max_parallel: 1\n", 1, ""},
		"zero":    {"action_max_parallel: 0\n", action.DefaultMaxParallel, ""},
		"neg":     {"action_max_parallel: -1\n", 0, "action_max_parallel"},
		"cap":     {fmt.Sprintf("action_max_parallel: %d\n", action.HardMaxParallel+1), 0, "out of range"},
	}
	for name, c := range cases {
		p := write(t, t.TempDir(), "tack.yaml", base+c.yml)
		cfg, err := Load(p)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: got %v, want it to mention %q", name, err, c.err)
			}
			continue
		}
		if err != nil || cfg.ActionParallel() != c.want {
			t.Errorf("%s: %v, want limit %d", name, err, c.want)
			continue

		}
	}
}

func TestLoadBackendActionsAndHooks(t *testing.T) {
	p := write(t, t.TempDir(), "tack.yaml", `
backend:
  - type: csv
    name: work
    options: {source: a.csv}
    actions:
      - {name: open, key: o, command: [gh, issue, view, "{id}"]}
    hooks:
      - {name: ping, on: [task.moved], command: [true]}
  - {type: csv, options: {source: b.csv}}
actions:
  - {name: open, key: o, command: [xdg-open, "{id}"]}
hooks:
  - {name: log, on: [reload.ok], command: [true]}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	all := c.AllActions()
	if len(all) != 2 || all[0].Backend != "" || all[1].Backend != "work" || all[1].Mode != "background" {
		t.Fatalf("actions %+v", all)
	}
	if h := c.AllHooks(); len(h) != 2 || h[0].Name != "log" || h[1].Backend != "work" {
		t.Fatalf("hooks %+v", h)
	}

	const two = "backend:\n  - type: csv\n    name: work\n    options: {source: a.csv}\n"
	cases := map[string]struct{ yml, want string }{
		"reload hook": {"    hooks:\n      - {name: r, on: [reload.fail], command: [x]}\n", "backend[1] work: hooks[1]: reload.fail concerns the whole board"},
		"bad action":  {"    actions:\n      - {name: a, command: [x, '{titel}']}\n", "backend[1] work: actions[1]: command[1]: unknown placeholder"},
		"key clash":   {"    actions:\n      - {name: b, key: o, command: [x]}\nactions:\n  - {name: a, key: o, command: [x]}\n", `backend[1] work: actions "a" and "b" both use key "o"`},
		"typo":        {"    action:\n      - {name: a, command: [x]}\n", `unknown key "action" in backend[]`},
	}
	for name, cs := range cases {
		_, err := Load(write(t, t.TempDir(), "tack.yaml", two+cs.yml))
		if err == nil || !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: got %v, want %q", name, err, cs.want)
		}
	}
}

func TestDisabledBackendIsSkipped(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "tack.yaml", `
backend:
  - type: csv
    disabled: true
    options: {source: a.csv}
    actions:
      - {name: off, command: [x]}
  - type: csv
    options: {source: b.csv}
`)
	write(t, dir, "b.csv", "id,title,status\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[1].Name != "csv2" || len(c.AllActions()) != 0 {
		t.Fatalf("%+v %v", c.Backends, c.AllActions())
	}
	be, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	if got := strings.Join(be.Names(), ","); got != "csv2" {
		t.Fatalf("names: %s", got)
	}
}

func TestDisabledBackendErrors(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.yaml", "default_backend: csv1\nbackend:\n  - {type: csv, disabled: true, options: {source: a.csv}}\n  - {type: csv, options: {source: b.csv}}\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("got %v", err)
	}
	p = write(t, dir, "b.yaml", "backend:\n  - {type: csv, disabled: true, options: {source: a.csv}}\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(); err == nil || !strings.Contains(err.Error(), "no backends enabled") {
		t.Fatalf("got %v", err)
	}
	p = write(t, dir, "c.yaml", "backend:\n  - {type: '', disabled: true}\n")
	if _, err := Load(p); err == nil {
		t.Fatal("disabled entry must still be validated")
	}
}

func TestCommandImpliesCustom(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "a.yaml", "backend:\n  - command: [./x]\n  - {type: custom, name: old, options: {command: [./y]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[0].Type != "custom" || c.Backends[0].Name != "custom1" || c.Backends[0].Command[0] != "./x" {
		t.Fatalf("%+v", c.Backends[0])
	}
	if c.Backends[1].Type != "custom" || len(c.Backends[1].Command) != 0 {
		t.Fatalf("type: custom with options must still load: %+v", c.Backends[1])
	}
	_, err = Load(write(t, dir, "b.yaml", "backend:\n  - {type: csv, command: [./x]}\n"))
	if err == nil || !strings.Contains(err.Error(), "only for the custom backend") {
		t.Fatalf("got %v", err)
	}
}
