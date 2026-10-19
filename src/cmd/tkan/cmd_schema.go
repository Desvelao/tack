package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"tkan/internal/action"
	"tkan/internal/actionlog"
	"tkan/internal/audit"
	"tkan/internal/query"
)

// exitCode is one entry of the exit-code contract for agents.
type exitCode struct {
	Code    int    `json:"code"`
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
}

// exitCodes is the single list of exit codes: `tkan schema` prints it and the
// MCP server names a failure by it.
var exitCodes = []exitCode{
	{exitOK, "ok", "done"},
	{exitError, "error", "the command failed"},
	{exitUsage, "usage", "bad flags or arguments"},
	{exitNotFound, "not found", "no such task (or, for next, no task is ready)"},
	{exitPrecondition, "precondition failed", "set-status -from / edit -if did not match, or the task is claimed by another agent"},
	{exitInterrupted, "interrupted", "killed by SIGINT, SIGTERM or SIGHUP"},
}

type schemaFlag struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

type schemaCommand struct {
	Name  string       `json:"name"`
	Usage []string     `json:"usage,omitempty"`
	Flags []schemaFlag `json:"flags"`
}

type schemaField struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Flag        string `json:"flag,omitempty"` // the add/edit flag that sets it
}

type schemaFilterField struct {
	Name        string `json:"name"`
	Alias       string `json:"alias,omitempty"`
	Description string `json:"description"`
}

type schemaFilter struct {
	Fields []string            `json:"fields"`
	Docs   []schemaFilterField `json:"docs"`
	Search []string            `json:"search,omitempty"` // what a plain word is matched against
}

type schemaDoc struct {
	Version      string                  `json:"version"`
	ExitCodes    []exitCode              `json:"exit_codes"`
	TaskFields   []schemaField           `json:"task_fields"`
	Commands     []schemaCommand         `json:"commands"`
	Filters      map[string]schemaFilter `json:"filters"`
	Placeholders []string                `json:"placeholders"`
	Actions      []actionRow             `json:"actions"`
}

// parseFlagDefaults reads the flag package's PrintDefaults: "  -name type" with
// the usage on the next lines, or "  -n<TAB>usage" for a one-letter bool flag.
func parseFlagDefaults(text string) []schemaFlag {
	var flags []schemaFlag
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "  -"):
			head, usage, _ := strings.Cut(strings.TrimPrefix(line, "  -"), "\t")
			f := strings.Fields(head)
			if len(f) == 0 {
				continue
			}
			fl := schemaFlag{Name: f[0], Type: "bool", Description: strings.TrimSpace(usage)}
			if len(f) > 1 {
				fl.Type = f[1]
			}
			flags = append(flags, fl)
		case strings.HasPrefix(line, "    \t") && len(flags) > 0:
			last := &flags[len(flags)-1]
			last.Description = strings.TrimSpace(last.Description + " " + strings.TrimSpace(line))
		default: // the "Usage of" line, a blank line, or the field help after the flags
			if len(flags) > 0 && !strings.HasPrefix(line, " ") {
				return flags
			}
		}
	}
	return flags
}

// describeFlags runs the command with -h, which prints its flags before any
// data is touched (the way `tkan help CMD` does), and reads them back.
func describeFlags(c command) []schemaFlag {
	var buf bytes.Buffer
	switch {
	case c.online != nil:
		c.online(context.Background(), nil, []string{"-h"}, &buf, &buf)
	case c.name == "init":
		c.offline([]string{"-h"}, &buf, &buf)
	}
	return parseFlagDefaults(buf.String())
}

func filterDocs(docs []query.FieldDoc) []schemaFilterField {
	out := make([]schemaFilterField, 0, len(docs))
	for _, d := range docs {
		if d.Name != "" {
			out = append(out, schemaFilterField{d.Name, d.Alias, d.Desc})
		}
	}
	return out
}

// buildSchema describes the CLI for an agent: nothing here is written by hand
// that the code already knows.
func buildSchema(ctx context.Context, a *app) (schemaDoc, error) {
	tasks, err := a.be.List(ctx)
	if err != nil {
		return schemaDoc{}, err
	}
	s := schemaDoc{
		Version:      version,
		ExitCodes:    exitCodes,
		Placeholders: action.Placeholders(),
		Actions:      actionRows(a),
		Filters: map[string]schemaFilter{
			"tasks": {Fields: query.KnownFields(tasks), Docs: filterDocs(query.TaskFieldDocs(nil))},
			"audit": {Fields: audit.Fields, Docs: filterDocs(audit.FieldDocs), Search: audit.SearchFields},
			"runs":  {Fields: actionlog.Fields, Docs: filterDocs(actionlog.FieldDocs), Search: actionlog.SearchFields},
		},
		TaskFields: []schemaField{
			{Name: "id", Description: "unique within a backend; set by the backend"},
			{Name: "status", Description: "raw stored value", Flag: "-status"},
			{Name: "column", Description: "board column the task is shown in (output only)"},
			{Name: "backend", Description: "name of the backend holding the task (output only)"},
			{Name: "lease", Description: "claim: when it runs out (managed by claim / release)"},
			{Name: "claimed_at", Description: "claim: since when (managed by claim / release)"},
		},
	}
	for _, f := range taskFields {
		s.TaskFields = append(s.TaskFields, schemaField{Name: strings.ReplaceAll(f.name, "-", "_"), Description: f.help, Flag: "-" + f.name})
	}
	for _, c := range commandTable() {
		if strings.HasPrefix(c.name, "__") {
			continue
		}
		sc := schemaCommand{Name: c.name, Flags: describeFlags(c)}
		for _, l := range strings.Split(strings.TrimRight(synopsis(c.name), "\n"), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				sc.Usage = append(sc.Usage, l)
			}
		}
		s.Commands = append(s.Commands, sc)
	}
	return s, nil
}

// cmdSchema: `tkan schema` prints task fields, commands with their flags and
// exit codes, filter fields and the configured actions as JSON.
func cmdSchema(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(errw)
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	s, err := buildSchema(ctx, a)
	if err != nil {
		return fail(errw, err)
	}
	return writeJSON(out, s)
}

// actionRow is one configured action as `tkan actions` prints it.
type actionRow struct {
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	Key       string `json:"key,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Mode      string `json:"mode"`
	Claim     bool   `json:"claim,omitempty"`
	Stdin     string `json:"stdin"`
	OnSuccess string `json:"on_success,omitempty"`
	OnFailure string `json:"on_failure,omitempty"`
	// What each appends to the notes: stdout, summary or error.
	OnSuccessNotes string   `json:"on_success_append_notes,omitempty"`
	OnFailureNotes string   `json:"on_failure_append_notes,omitempty"`
	Command        []string `json:"command"` // as configured, placeholders unfilled
}

func actionRows(a *app) []actionRow {
	rows := []actionRow{}
	for _, s := range a.cfg.AllActions() {
		r := actionRow{Name: s.Name, Label: s.Label, Key: s.Key, Backend: s.Backend, Mode: s.Mode, Claim: s.Claim,
			Stdin: s.Stdin, OnSuccess: s.OnSuccess.Status, OnFailure: s.OnFailure.Status,
			OnSuccessNotes: s.OnSuccess.AppendNotes, OnFailureNotes: s.OnFailure.AppendNotes, Command: s.Command}
		if r.Mode == "" {
			r.Mode = action.ModeBackground
		}
		if r.Stdin == "" {
			r.Stdin = action.StdinNone
		}
		rows = append(rows, r)
	}
	return rows
}

// cmdActions: `tkan actions [-json]` lists the actions of the config.
func cmdActions(_ context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("actions", flag.ContinueOnError)
	fs.SetOutput(errw)
	asJSON := fs.Bool("json", false, "JSON output")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	rows := actionRows(a)
	if *asJSON {
		return writeJSON(out, rows)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKEY\tMODE\tBACKEND\tLABEL")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Key, r.Mode, r.Backend, r.Label)
	}
	w.Flush()
	return exitOK
}
