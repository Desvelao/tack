package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/outcome"
	"tack/internal/task"
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
		fmt.Fprintf(errw, "tack: warning: column %q is over its WIP limit (%d/%d)\n", b.Columns[col].Name, n, lim)
	}
}

// warnOverLimitOf is warnOverLimit for the column that holds the raw status.
// Commands that move a card as a side effect (add, report, claim, release,
// next -claim) call it, so the advisory limit is told wherever a card lands.
func warnOverLimitOf(ctx context.Context, a *app, status string, errw io.Writer) {
	b, err := a.board()
	if err != nil {
		return
	}
	if col := b.ColumnOf(status); col >= 0 {
		warnOverLimit(ctx, a, b, col, errw)
	}
}

// guardFor builds the context of a guarded write ("only if FIELD is VALUE").
// A status is given as a column name or a raw value, like everywhere else.
func guardFor(ctx context.Context, a *app, field, value string) (context.Context, string, error) {
	if (&task.Task{}).FieldPtr(field) == nil {
		return ctx, "", fmt.Errorf("unknown field %q (fields: %s)", field, strings.Join(task.FieldNames, ", "))
	}
	if field == "status" {
		b, err := a.board()
		if err != nil {
			return ctx, "", err
		}
		m, ok := b.Lookup(value)
		if !ok {
			return ctx, "", fmt.Errorf("unknown status %q", value)
		}
		value = m.Raw
	}
	return backend.WithExpect(ctx, field, value), value, nil
}

func cmdSetStatus(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("set-status", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	asJSON := fs.Bool("json", false, "print the resulting tasks as JSON (with a changed flag)")
	from := fs.String("from", "", "only move tasks that are still in this column/status (exit 4 otherwise); checked inside the backend's lock")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) < 2 {
		fmt.Fprintln(errw, "usage: tack set-status ID... STATUS [-b NAME] [-from STATUS] [-json]")
		return exitUsage
	}
	ids, status := pos[:len(pos)-1], pos[len(pos)-1]
	wctx := ctx
	guarded := false
	fs.Visit(func(f *flag.Flag) { guarded = guarded || f.Name == "from" })
	if guarded {
		var err error
		if wctx, _, err = guardFor(ctx, a, "status", *from); err != nil {
			fmt.Fprintf(errw, "set-status: -from: %v\n", err)
			return exitUsage
		}
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	m, ok := b.Lookup(status)
	if !ok {
		return unknownStatus(errw, b, status)
	}
	// Resolve every ID first so a bad one changes nothing.
	tasks, err := a.findAll(ctx, *backendName, ids)
	if err != nil {
		return fail(errw, err)
	}
	type result struct {
		taskOut
		Changed bool `json:"changed"`
	}
	var results []result
	moved, nMoved := false, 0
	for _, t := range tasks {
		if t.Status == m.Raw && !guarded {
			results = append(results, result{withColumn(b, t), false})
			if !*asJSON {
				fmt.Fprintf(out, "%s:%s already %s\n", t.Backend, t.ID, b.Columns[m.Col].Name)
			}
			continue
		}
		old := t
		t.Status = m.Raw
		// With -from the snapshot is checked first (no write for a stale view); the
		// backend checks again under its lock, which is what decides a race.
		err := backend.CheckExpect(wctx, old)
		if err == nil {
			err = a.be.Update(backend.WithOld(wctx, old), t)
		}
		if err != nil {
			// Not atomic: report what was already done before failing.
			if *asJSON && len(results) > 0 {
				writeJSON(out, results)
			}
			if moved {
				fmt.Fprintf(errw, "set-status: stopped after moving %d of %d tasks\n", nMoved, len(tasks))
				warnOverLimit(ctx, a, b, m.Col, errw)
			}
			return fail(errw, err)
		}
		results = append(results, result{withColumn(b, t), true})
		if !*asJSON {
			fmt.Fprintf(out, "%s:%s -> %s (%s)\n", t.Backend, t.ID, b.Columns[m.Col].Name, m.Raw)
		}
		moved = true
		nMoved++
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
		fmt.Fprintln(errw, "usage: tack add "+fieldUsage()+" [-b NAME] [-status S] [-json]")
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
	for _, d := range []struct{ name, v string }{{"due", t.Due}, {"start", t.Start}} {
		if err := checkDate(d.name, d.v); err != nil {
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
	warnOverLimitOf(ctx, a, created.Status, errw)
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
	appendNotes := fs.String("append-notes", "", "append this text to the notes (a blank line apart; - reads stdin), without rewriting them")
	appendFile := fs.String("append-notes-file", "", "append the text of this file to the notes")
	var addLabels, rmLabels listFlag
	fs.Var(&addLabels, "add-label", "add label(s), comma-separated; repeatable (applied after -labels)")
	fs.Var(&rmLabels, "rm-label", "remove label(s), comma-separated; repeatable")
	ifGuard := fs.String("if", "", "only edit if the task still has FIELD=VALUE (exit 4 otherwise); checked inside the backend's lock")
	pos, err := parseArgs(fs, args)
	usageMsg := "usage: tack edit ID [-b NAME] [-if FIELD=VALUE] [-status S] [-add-label L] [-rm-label L] [-append-notes TEXT] [-json] " + fieldUsage()
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(errw, usageMsg)
		return exitUsage
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	delete(given, "b")
	delete(given, "json")
	guarded := given["if"]
	delete(given, "if")
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
	// -append-notes adds to the notes the store holds right now, so it cannot be
	// combined with a rewrite of them.
	addText, appending := "", given["append-notes"] || given["append-notes-file"]
	if appending {
		switch {
		case given["append-notes"] && given["append-notes-file"]:
			fmt.Fprintln(errw, "edit: use either -append-notes or -append-notes-file, not both")
			return exitUsage
		case given["notes"]:
			fmt.Fprintln(errw, "edit: -append-notes cannot be combined with -notes or -notes-file")
			return exitUsage
		}
		path := *appendFile
		if given["append-notes"] {
			path, addText = "-", *appendNotes
		}
		if given["append-notes-file"] || addText == "-" {
			text, err := readText(path, &usedStdin)
			if err != nil {
				fmt.Fprintf(errw, "edit: -append-notes: %v\n", err)
				return exitUsage
			}
			addText = text
		}
		addText = strings.TrimSpace(addText)
		delete(given, "append-notes")
		delete(given, "append-notes-file")
	}
	if len(given) == 0 && !appending {
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
	wctx := ctx
	if guarded {
		field, value, ok := strings.Cut(*ifGuard, "=")
		if !ok {
			fmt.Fprintln(errw, "edit: -if wants FIELD=VALUE (for example -if status=todo)")
			return exitUsage
		}
		var err error
		if wctx, _, err = guardFor(ctx, a, strings.TrimSpace(field), value); err != nil {
			fmt.Fprintf(errw, "edit: -if: %v\n", err)
			return exitUsage
		}
	}
	t, err := a.find(ctx, *backendName, pos[0])
	if err != nil {
		return fail(errw, err)
	}
	old := t.Clone()
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
	if addText != "" {
		t.Notes = outcome.AppendBlock(t.Notes, addText)
	}
	if len(addLabels) > 0 || len(rmLabels) > 0 {
		t.Labels = editLabels(t.Labels, addLabels, rmLabels)
	}
	if err := t.Validate(); err != nil { // the rules of add (e.g. start after due), as a usage error
		fmt.Fprintf(errw, "edit: %v\n", err)
		return exitUsage
	}
	err = backend.CheckExpect(wctx, old) // a stale view needs no write; the backend decides a race
	if err == nil && addText != "" {
		t, err = updateAppendingNotes(ctx, a, wctx, old, t, func(notes string) string { return outcome.AppendBlock(notes, addText) })
	} else if err == nil {
		err = a.be.Update(backend.WithOld(wctx, old), t)
	}
	if err != nil {
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
	asJSON := fs.Bool("json", false, "print the tasks as JSON (with a deleted flag)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) < 1 {
		fmt.Fprintln(errw, "usage: tack delete ID... [-b NAME] [-n] [-json]")
		return exitUsage
	}
	// Resolve every ID first so a bad one deletes nothing.
	tasks, err := a.findAll(ctx, *backendName, pos)
	if err != nil {
		return fail(errw, err)
	}
	type deletedOut struct {
		Backend string `json:"backend"`
		ID      string `json:"id"`
		Title   string `json:"title"`
		Deleted bool   `json:"deleted"`
	}
	results := []deletedOut{}
	deleted := 0
	for _, t := range tasks {
		if *dryRun {
			results = append(results, deletedOut{t.Backend, t.ID, t.Title, false})
			if !*asJSON {
				fmt.Fprintf(out, "would delete %s:%s %q\n", t.Backend, t.ID, t.Title)
			}
			continue
		}
		if err := a.be.Delete(ctx, t.Backend, t.ID); err != nil {
			if *asJSON && deleted > 0 {
				writeJSON(out, results)
			}
			if deleted > 0 { // not atomic: say how far it got
				fmt.Fprintf(errw, "delete: stopped after deleting %d of %d tasks\n", deleted, len(tasks))
			}
			return fail(errw, err)
		}
		deleted++
		results = append(results, deletedOut{t.Backend, t.ID, t.Title, true})
		if !*asJSON {
			fmt.Fprintf(out, "deleted %s:%s %q\n", t.Backend, t.ID, t.Title)
		}
	}
	if *asJSON {
		return writeJSON(out, results)
	}
	return exitOK
}

// updateAppendingNotes writes t, the edit of old that appends to the notes,
// guarded on the notes still being old's. Somebody else appending first (two
// agents running `tack report` on one task) makes the write fail with
// ErrPrecondition; then the edit is applied again to the task as the store
// holds it, with add run on the notes found there, so neither note is lost.
// A backend takes one guard, so a -if guard carried by ctx is checked against
// each fresh task instead (an ErrPrecondition when it no longer holds); it
// cannot be enforced inside the write itself, which is guarded on the notes.
// It returns the task as written.
func updateAppendingNotes(ctx context.Context, a *app, guard context.Context, old, t task.Task, add func(notes string) string) (task.Task, error) {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		wctx := backend.WithExpect(backend.WithOld(ctx, old), "notes", old.Notes)
		if err = a.be.Update(wctx, t); !errors.Is(err, backend.ErrPrecondition) {
			return t, err
		}
		cur, ferr := a.find(ctx, old.Backend, old.ID)
		if ferr != nil {
			return t, ferr
		}
		if err = backend.CheckExpect(guard, cur); err != nil {
			return t, err
		}
		t = task.Merge(cur, old, t)
		t.Notes = add(cur.Notes)
		old = cur
	}
	return t, err
}
