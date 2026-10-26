package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/config"
	"tack/internal/task"
)

func TestSchemaOutput(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+actionsYAML)
	code, out, errs := do(t, "-c", cfg, "schema")
	if code != exitOK {
		t.Fatalf("schema: %d %s", code, errs)
	}
	var s schemaDoc
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if s.Version == "" {
		t.Error("no version")
	}
	cmds := map[string]schemaCommand{}
	for _, c := range s.Commands {
		cmds[c.Name] = c
		if len(c.Usage) == 0 && c.Name != "completion" {
			t.Logf("no usage line for %s", c.Name)
		}
	}
	for _, c := range commandTable() {
		if _, ok := cmds[c.name]; !ok && !strings.HasPrefix(c.name, "__") {
			t.Errorf("command %s missing from the schema", c.name)
		}
	}
	if _, ok := cmds["__complete"]; ok {
		t.Error("the hidden __complete command must not be listed")
	}
	flagOf := func(cmd, name string) (schemaFlag, bool) {
		for _, f := range cmds[cmd].Flags {
			if f.Name == name {
				return f, true
			}
		}
		return schemaFlag{}, false
	}
	for _, c := range [][2]string{{"set-status", "from"}, {"set-status", "json"}, {"delete", "json"}, {"delete", "n"},
		{"claim", "lease"}, {"edit", "if"}, {"list", "q"}, {"run", "force"}, {"mcp", "read-only"}} {
		if _, ok := flagOf(c[0], c[1]); !ok {
			t.Errorf("%s: flag -%s missing: %+v", c[0], c[1], cmds[c[0]].Flags)
		}
	}
	if f, _ := flagOf("claim", "lease"); f.Type != "duration" || f.Description == "" {
		t.Errorf("claim -lease = %+v", f)
	}
	if f, _ := flagOf("set-status", "json"); f.Type != "bool" || f.Description == "" {
		t.Errorf("set-status -json = %+v", f)
	}
	var codes []int
	for _, e := range s.ExitCodes {
		codes = append(codes, e.Code)
	}
	if !slices.Equal(codes, []int{0, 1, 2, 3, 4, 130}) {
		t.Errorf("exit codes = %v", codes)
	}
	var names []string
	for _, f := range s.TaskFields {
		names = append(names, f.Name)
	}
	for _, want := range []string{"id", "status", "column", "assignee", "claimed_at", "blocked_by", "due"} {
		if !slices.Contains(names, want) {
			t.Errorf("task field %s missing: %v", want, names)
		}
	}
	if !slices.Equal(s.Filters["audit"].Fields, audit.Fields) || !slices.Equal(s.Filters["runs"].Fields, actionlog.Fields) {
		t.Errorf("audit / runs filter fields differ: %+v", s.Filters)
	}
	if !slices.Contains(s.Filters["tasks"].Fields, "column") || len(s.Filters["tasks"].Docs) == 0 {
		t.Errorf("task filter = %+v", s.Filters["tasks"])
	}
	if !slices.Equal(s.Placeholders, action.Placeholders()) {
		t.Errorf("placeholders = %v", s.Placeholders)
	}
	var note *actionRow
	for i := range s.Actions {
		if s.Actions[i].Name == "note" {
			note = &s.Actions[i]
		}
	}
	if note == nil || note.Mode != "background" || note.Stdin != "none" || len(note.Command) == 0 {
		t.Errorf("action note = %+v", note)
	}
	if code, _, _ := do(t, "-c", cfg, "schema", "extra"); code != exitUsage {
		t.Errorf("stray argument: exit %d", code)
	}
}

func TestSchemaWithoutActions(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML)
	_, out, _ := do(t, "-c", cfg, "schema")
	if !strings.Contains(out, `"actions": []`) {
		t.Errorf("no actions should be an empty list:\n%s", out)
	}
}

func TestActionsCommand(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+actionsYAML)
	code, out, _ := do(t, "-c", cfg, "actions")
	if code != exitOK || !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "note") || !strings.Contains(out, "interactive") {
		t.Errorf("actions: %d %q", code, out)
	}
	code, out, _ = do(t, "-c", cfg, "actions", "-json")
	var rows []actionRow
	if err := json.Unmarshal([]byte(out), &rows); code != exitOK || err != nil || len(rows) < 3 || rows[0].Name != "note" {
		t.Errorf("actions -json: %d %v %q", code, err, out)
	}
	_, _, plain := setup(t, issuesCSV, issuesYAML)
	if code, out, _ = do(t, "-c", plain, "actions", "-json"); code != exitOK || strings.TrimSpace(out) != "[]" {
		t.Errorf("no actions: %d %q", code, out)
	}
}

func TestActionsJSONHasWorkdir(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+"actions:\n  - name: wt\n    command: [pwd]\n    workdir: work/{slug}\n    create_workdir: true\n  - name: plain\n    command: [pwd]\n")
	code, out, _ := do(t, "-c", cfg, "actions", "-json")
	var rows []actionRow
	if err := json.Unmarshal([]byte(out), &rows); code != exitOK || err != nil || len(rows) != 2 || rows[0].Workdir != "work/{slug}" || rows[1].Workdir != "" {
		t.Errorf("actions -json workdir: %d %v %q", code, err, out)
	}
}

func TestDeleteJSON(t *testing.T) {
	_, file, cfg := setup(t, "id,title,description,status\n1,A,,pending\n2,B,,pending\n", "")
	code, out, _ := do(t, "-c", cfg, "delete", "-n", "-json", "1")
	var res []struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(out), &res); code != exitOK || err != nil || len(res) != 1 || res[0].Deleted {
		t.Fatalf("dry run: %d %v %q", code, err, out)
	}
	code, out, _ = do(t, "-c", cfg, "delete", "-json", "1", "2")
	if err := json.Unmarshal([]byte(out), &res); code != exitOK || err != nil || len(res) != 2 || !res[0].Deleted || res[1].ID != "2" {
		t.Fatalf("delete: %d %v %q", code, err, out)
	}
	_ = file
}

func TestSchemaSurvivesAPartiallyFailingBackend(t *testing.T) {
	a := &app{
		be: backend.NewSet([]backend.Named{
			{Name: "healthy", Type: "stub", Backend: stubBackend{tasks: []task.Task{{ID: "1", Title: "Healthy", Status: "pending"}}}},
			{Name: "remote", Type: "stub", Backend: stubBackend{err: errors.New("tool is down")}},
		}),
		spec: board.DefaultSpec(),
		cfg:  &config.Config{},
	}
	s, err := buildSchema(context.Background(), a)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	if len(s.Filters["tasks"].Fields) == 0 {
		t.Error("no task filter fields")
	}
}
