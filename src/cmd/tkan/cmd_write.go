package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"tkan/internal/board"
	"tkan/internal/task"
)

// editLabels applies the label edits to a comma-separated Labels value:
// remove first, then add; matching ignores case, the first spelling wins and
// the order is kept.
func editLabels(labels string, add, remove []string) string {
	t := task.Task{Labels: labels}
	var out []string
	seen := map[string]bool{}
	drop := map[string]bool{}
	for _, r := range remove {
		drop[strings.ToLower(r)] = true
	}
	for _, l := range append(t.LabelList(), add...) {
		k := strings.ToLower(l)
		if seen[k] || drop[k] && !contains(add, l) {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return strings.Join(out, ",")
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// warnOverLimit prints a warning on errw when column col now holds more cards
// than its WIP limit. It never fails the command: limits are advisory.
func warnOverLimit(ctx context.Context, a *app, b board.Board, col int, errw io.Writer) {
	tasks, _ := a.be.List(ctx) // a partial list is fine for a warning
	n := 0
	for _, t := range tasks {
		if b.ColumnOf(t.Status) == col {
			n++
		}
	}
	if lim, over := b.Over(col, n); over {
		fmt.Fprintf(errw, "tkan: warning: column %q is over its WIP limit (%d/%d)\n", b.Columns[col].Name, n, lim)
	}
}

func cmdSetStatus(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("set-status", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	asJSON := fs.Bool("json", false, "print the resulting tasks as JSON (with a changed flag)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) < 2 {
		fmt.Fprintln(errw, "usage: tkan set-status ID... STATUS [-b NAME]")
		return exitUsage
	}
	ids, status := pos[:len(pos)-1], pos[len(pos)-1]
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	m, ok := b.Lookup(status)
	if !ok {
		return unknownStatus(errw, b, status)
	}
	// Resolve every ID first so a bad one changes nothing.
	tasks := make([]task.Task, len(ids))
	for i, id := range ids {
		if tasks[i], err = a.find(ctx, *backendName, id); err != nil {
			return fail(errw, err)
		}
	}
	type result struct {
		taskOut
		Changed bool `json:"changed"`
	}
	var results []result
	moved := false
	for _, t := range tasks {
		if t.Status == m.Raw {
			results = append(results, result{withColumn(b, t), false})
			if !*asJSON {
				fmt.Fprintf(out, "%s:%s already %s\n", t.Backend, t.ID, b.Columns[m.Col].Name)
			}
			continue
		}
		t.Status = m.Raw
		if err := a.be.Update(ctx, t); err != nil {
			return fail(errw, err)
		}
		results = append(results, result{withColumn(b, t), true})
		if !*asJSON {
			fmt.Fprintf(out, "%s:%s -> %s (%s)\n", t.Backend, t.ID, b.Columns[m.Col].Name, m.Raw)
		}
		moved = true
	}
	if *asJSON {
		if code := writeJSON(out, results); code != exitOK {
			return code
		}
	}
	if moved {
		warnOverLimit(ctx, a, b, m.Col, errw)
	}
	return exitOK
}

func cmdAdd(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(errw)
	var t task.Task
	var status string
	for _, f := range taskFields {
		fs.StringVar(f.ptr(&t), f.name, "", f.help)
	}
	fs.StringVar(&t.Backend, "b", "", "backend to create the task in (default: the first one)")
	fs.StringVar(&status, "status", "", "initial column name or raw status (default: first column)")
	descFile := fs.String("description-file", "", "read the description from this file (instead of -description)")
	notesFile := fs.String("notes-file", "", "read the notes from this file (instead of -notes)")
	asJSON := fs.Bool("json", false, "print the created task as JSON (like show -json)")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	if strings.TrimSpace(t.Title) == "" {
		fmt.Fprintln(errw, "usage: tkan add "+fieldUsage()+" [-b NAME] [-status S] [-json]")
		return exitUsage
	}
	usedStdin := false
	for _, src := range []struct {
		field *string
		val   string
		file  string
		name  string
	}{{&t.Description, t.Description, *descFile, "description"}, {&t.Notes, t.Notes, *notesFile, "notes"}} {
		switch {
		case src.file != "" && src.val != "":
			fmt.Fprintf(errw, "add: use either -%s or -%s-file, not both\n", src.name, src.name)
			return exitUsage
		case src.file != "" || src.val == "-":
			path := src.file
			if path == "" {
				path = "-"
			}
			text, err := readText(path, &usedStdin)
			if err != nil {
				fmt.Fprintf(errw, "add: -%s: %v\n", src.name, err)
				return exitUsage
			}
			*src.field = text
		}
	}
	for _, f := range taskFields {
		*f.ptr(&t) = strings.TrimSpace(*f.ptr(&t))
	}
	for name, v := range map[string]string{"due": t.Due, "start": t.Start} {
		if err := checkDate(name, v); err != nil {
			fmt.Fprintf(errw, "add: %v\n", err)
			return exitUsage
		}
	}
	if err := t.Validate(); err != nil { // same rules the backends apply, reported as a usage error
		fmt.Fprintf(errw, "add: %v\n", err)
		return exitUsage
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	if status == "" {
		t.Status = b.WriteValue(0)
	} else {
		m, ok := b.Lookup(status)
		if !ok {
			return unknownStatus(errw, b, status)
		}
		t.Status = m.Raw
	}
	if t.Backend == "" {
		t.Backend = a.defaultBackend
	}
	created, err := a.be.Create(ctx, t)
	if err != nil {
		return fail(errw, err)
	}
	if *asJSON {
		return writeJSON(out, withColumn(b, created))
	}
	if len(a.be.Names()) > 1 {
		fmt.Fprintf(out, "%s:%s\n", created.Backend, created.ID)
	} else {
		fmt.Fprintln(out, created.ID)
	}
	return exitOK
}

// cmdEdit updates only the fields whose flags were given, so an empty value
// (-assignee "") clears a field while an absent flag leaves it alone.
func cmdEdit(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	var in task.Task
	targets := map[string]*string{} // flag name -> the value given
	for _, f := range taskFields {
		targets[f.name] = f.ptr(&in)
		fs.StringVar(f.ptr(&in), f.name, "", f.help)
	}
	status := fs.String("status", "", "move to this column name or raw status value")
	asJSON := fs.Bool("json", false, "print the updated task as JSON (like show -json)")
	descFile := fs.String("description-file", "", "read the new description from this file")
	notesFile := fs.String("notes-file", "", "read the new notes from this file")
	var addLabels, rmLabels listFlag
	fs.Var(&addLabels, "add-label", "add label(s), comma-separated; repeatable (applied after -labels)")
	fs.Var(&rmLabels, "rm-label", "remove label(s), comma-separated; repeatable")
	pos, err := parseArgs(fs, args)
	usageMsg := "usage: tkan edit ID [-b NAME] [-status S] [-add-label L] [-rm-label L] [-json] " + fieldUsage()
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(errw, usageMsg)
		return exitUsage
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	delete(given, "b")
	delete(given, "json")
	// Long text can come from stdin (-description -) or a file (-description-file F).
	usedStdin := false
	for _, src := range []struct {
		field *string
		name  string
		file  string
	}{{&in.Description, "description", *descFile}, {&in.Notes, "notes", *notesFile}} {
		fromFile := given[src.name+"-file"]
		delete(given, src.name+"-file")
		switch {
		case fromFile && given[src.name]:
			fmt.Fprintf(errw, "edit: use either -%s or -%s-file, not both\n", src.name, src.name)
			return exitUsage
		case fromFile || (given[src.name] && *src.field == "-"):
			path := src.file
			if !fromFile {
				path = "-"
			}
			text, err := readText(path, &usedStdin)
			if err != nil {
				fmt.Fprintf(errw, "edit: -%s: %v\n", src.name, err)
				return exitUsage
			}
			*src.field, given[src.name] = text, true
		}
	}
	if len(given) == 0 {
		fmt.Fprintln(errw, usageMsg)
		return exitUsage
	}
	if given["title"] && strings.TrimSpace(in.Title) == "" {
		fmt.Fprintln(errw, "edit: title cannot be empty")
		return exitUsage
	}
	for _, name := range []string{"due", "start"} {
		if given[name] {
			if err := checkDate(name, *targets[name]); err != nil {
				fmt.Fprintf(errw, "edit: %v\n", err)
				return exitUsage
			}
		}
	}
	t, err := a.find(ctx, *backendName, pos[0])
	if err != nil {
		return fail(errw, err)
	}
	limitCol := -1
	var limitBoard board.Board
	if given["status"] {
		b, err := a.board()
		if err != nil {
			return fail(errw, err)
		}
		m, ok := b.Lookup(*status)
		if !ok {
			return unknownStatus(errw, b, *status)
		}
		t.Status = m.Raw
		limitCol, limitBoard = m.Col, b
		delete(given, "status")
	}
	dst := map[string]*string{}
	for _, f := range taskFields {
		dst[f.name] = f.ptr(&t)
	}
	delete(given, "add-label")
	delete(given, "rm-label")
	for name := range given {
		*dst[name] = strings.TrimSpace(*targets[name])
	}
	if len(addLabels) > 0 || len(rmLabels) > 0 {
		t.Labels = editLabels(t.Labels, addLabels, rmLabels)
	}
	if err := a.be.Update(ctx, t); err != nil {
		return fail(errw, err)
	}
	if limitCol >= 0 {
		warnOverLimit(ctx, a, limitBoard, limitCol, errw)
	}
	if *asJSON {
		b, err := a.board()
		if err != nil {
			return fail(errw, err)
		}
		return writeJSON(out, withColumn(b, t))
	}
	fmt.Fprintf(out, "updated %s:%s\n", t.Backend, t.ID)
	return exitOK
}

func cmdDelete(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	dryRun := fs.Bool("n", false, "dry run: print what would be deleted, change nothing")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) < 1 {
		fmt.Fprintln(errw, "usage: tkan delete ID... [-b NAME] [-n]")
		return exitUsage
	}
	// Resolve every ID first so a bad one deletes nothing.
	tasks := make([]task.Task, len(pos))
	for i, id := range pos {
		if tasks[i], err = a.find(ctx, *backendName, id); err != nil {
			return fail(errw, err)
		}
	}
	for _, t := range tasks {
		if *dryRun {
			fmt.Fprintf(out, "would delete %s:%s %q\n", t.Backend, t.ID, t.Title)
			continue
		}
		if err := a.be.Delete(ctx, t.Backend, t.ID); err != nil {
			return fail(errw, err)
		}
		fmt.Fprintf(out, "deleted %s:%s %q\n", t.Backend, t.ID, t.Title)
	}
	return exitOK
}
