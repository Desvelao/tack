package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"tkan/internal/backend"
	"tkan/internal/board"
	"tkan/internal/query"
	"tkan/internal/task"
)

func cmdList(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(errw)
	status := fs.String("status", "", "only tasks in this column (or with this raw status value)")
	q := fs.String("q", "", "filter query, e.g. 'priority=high col:doing' (syntax and fields: tkan help filter)")
	view := fs.String("v", "", "saved view (board.views) to apply; combined with -q")
	asJSON := fs.Bool("json", false, "JSON output")
	ids := fs.Bool("ids", false, "print only BACKEND:ID, one per line (for piping into set-status / delete)")
	tsv := fs.Bool("tsv", false, "tab-separated output with escaped tabs/newlines, one task per line")
	fields := fs.String("fields", "", "columns of the table/-tsv output, comma-separated (any filter field, e.g. id,title,labels,story_points)")
	sortBy := fs.String("sort", "", "order the output: comma-separated fields, a leading - means descending (e.g. -priority,id)")
	limit := fs.Int("limit", 0, "print at most N tasks (after -sort)")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	if (*asJSON && *ids) || (*asJSON && *tsv) || (*ids && *tsv) {
		fmt.Fprintln(errw, "list: use only one of -json, -ids, -tsv")
		return exitUsage
	}
	if *fields != "" && (*asJSON || *ids) {
		fmt.Fprintln(errw, "list: -fields applies to the table and -tsv output, not -json or -ids")
		return exitUsage
	}
	if *limit < 0 {
		fmt.Fprintln(errw, "list: -limit must be 0 (no limit) or more")
		return exitUsage
	}
	filtered, partial, code, ok := selectTasks(ctx, a, *status, *view, *q, errw)
	if !ok {
		return code
	}
	known := query.KnownFields(taskList(filtered))
	cols := defaultListFields
	if *fields != "" {
		cols = nil
		for _, f := range strings.Split(*fields, ",") {
			if f = query.Normalize(f); f != "" {
				cols = append(cols, f)
			}
		}
		if err := query.CheckSearch(cols, known); err != nil {
			var uf *query.UnknownFieldError
			if errors.As(err, &uf) {
				uf.Prefix = "list -fields"
			}
			fmt.Fprintln(errw, "tkan:", err)
			return exitUsage
		}
	}
	if *sortBy != "" {
		var keys []board.SortKey
		for _, f := range strings.Split(*sortBy, ",") {
			k := board.SortKey{Field: query.Normalize(strings.TrimPrefix(strings.TrimSpace(f), "-")), Desc: strings.HasPrefix(strings.TrimSpace(f), "-")}
			if k.Field != "" {
				keys = append(keys, k)
			}
		}
		if err := query.CheckSort(keys, known); err != nil {
			var uf *query.UnknownFieldError
			if errors.As(err, &uf) {
				uf.Prefix = "list -sort"
			}
			fmt.Fprintln(errw, "tkan:", err)
			return exitUsage
		}
		sort.SliceStable(filtered, func(i, j int) bool {
			return query.CompareBy(filtered[i].lookup(), filtered[j].lookup(), keys) < 0
		})
	}
	if *limit > 0 && len(filtered) > *limit {
		filtered = filtered[:*limit]
	}
	finish := func(code int) int {
		if partial && code == exitOK {
			return exitError
		}
		return code
	}
	if *asJSON {
		return finish(writeJSON(out, filtered))
	}
	if *ids {
		for _, t := range filtered {
			fmt.Fprintf(out, "%s:%s\n", t.Backend, t.ID)
		}
		return finish(exitOK)
	}
	cell := func(t taskOut, f string) string {
		v, _ := t.lookup()(f)
		return v
	}
	if *tsv {
		for _, t := range filtered {
			row := make([]string, len(cols))
			for i, f := range cols {
				row[i] = tsvEscape(cell(t, f))
			}
			fmt.Fprintln(out, strings.Join(row, "\t"))
		}
		return finish(exitOK)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	head := make([]string, len(cols))
	for i, f := range cols {
		head[i] = strings.ToUpper(f)
	}
	fmt.Fprintln(w, strings.Join(head, "\t"))
	for _, t := range filtered {
		row := make([]string, len(cols))
		for i, f := range cols {
			row[i] = strings.Join(strings.Fields(cell(t, f)), " ") // one line per task
		}
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
	return finish(exitOK)
}

// defaultListFields are the columns of the plain `list` table and -tsv output.
var defaultListFields = []string{"backend", "id", "status", "column", "priority", "type", "assignee", "due", "title"}

func taskList(out []taskOut) []task.Task {
	ts := make([]task.Task, len(out))
	for i, o := range out {
		ts[i] = o.Task
	}
	return ts
}

// selectTasks lists the tasks and applies -status, -v and -q (shared by list
// and count). When ok is false the caller returns code. partial is true when a
// backend failed: the others' tasks are still returned, with a warning on errw.
func selectTasks(ctx context.Context, a *app, status, view, q string, errw io.Writer) (filtered []taskOut, partial bool, code int, ok bool) {
	tasks, err := a.be.List(ctx)
	if err != nil {
		var perr *backend.PartialError
		if !errors.As(err, &perr) {
			return nil, false, fail(errw, err), false
		}
		fmt.Fprintln(errw, "tkan: warning:", err)
		partial = true
	}
	b, err := a.board()
	if err != nil {
		return nil, false, fail(errw, err), false
	}
	var match board.Match
	if status != "" {
		var found bool
		if match, found = b.Lookup(status); !found {
			return nil, false, unknownStatus(errw, b, status), false
		}
	}
	if view != "" {
		v, found := a.spec.View(view)
		if !found {
			var names []string
			for _, x := range a.spec.Views {
				names = append(names, x.Name)
			}
			fmt.Fprintf(errw, "tkan: unknown view %q (views: %s)\n", view, strings.Join(names, ", "))
			return nil, false, exitUsage, false
		}
		q = strings.TrimSpace(v.Query + " " + q)
	}
	var qry query.Query
	if q != "" {
		var err error
		if qry, err = query.Parse(q); err == nil {
			err = qry.Check(query.KnownFields(tasks))
		}
		if err != nil {
			fmt.Fprintln(errw, "tkan: filter:", err)
			return nil, false, exitUsage, false
		}
	}
	deps := b.Deps(tasks)
	filtered = make([]taskOut, 0, len(tasks))
	for _, t := range tasks {
		if status != "" {
			if match.Name && b.ColumnOf(t.Status) != match.Col {
				continue
			}
			if !match.Name && !strings.EqualFold(t.Status, match.Raw) {
				continue
			}
		}
		o := withColumn(b, t)
		o.deps, o.Blockers = deps, deps.Blockers(t)
		if !qry.Empty() && !qry.Match(o.lookup(), a.spec.Search) {
			continue
		}
		filtered = append(filtered, o)
	}
	return filtered, partial, 0, true
}

type countRow struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// cmdCount prints how many tasks fall into each column, priority, type,
// assignee or label (a task with several labels counts once per label).
func cmdCount(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("count", flag.ContinueOnError)
	fs.SetOutput(errw)
	by := fs.String("by", "column", "group by: column, priority, type, assignee or label")
	status := fs.String("status", "", "only tasks in this column (or with this raw status value)")
	q := fs.String("q", "", "filter query (see: tkan help filter)")
	view := fs.String("v", "", "saved view (board.views) to apply; combined with -q")
	asJSON := fs.Bool("json", false, "JSON output")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	field := strings.ToLower(*by)
	switch field {
	case "column", "priority", "type", "assignee", "label", "labels":
	default:
		fmt.Fprintf(errw, "count: -by must be column, priority, type, assignee or label, got %q\n", *by)
		return exitUsage
	}
	filtered, partial, code, ok := selectTasks(ctx, a, *status, *view, *q, errw)
	if !ok {
		return code
	}
	const none = "(none)"
	counts := map[string]*countRow{}
	var order []string
	add := func(key string) {
		if strings.TrimSpace(key) == "" {
			key = none
		}
		k := strings.ToLower(key)
		if counts[k] == nil {
			counts[k] = &countRow{Key: key}
			order = append(order, k)
		}
		counts[k].Count++
	}
	if field == "column" { // board order, including empty columns
		b, err := a.board()
		if err != nil {
			return fail(errw, err)
		}
		for _, c := range b.Columns {
			k := strings.ToLower(c.Name)
			counts[k] = &countRow{Key: c.Name}
			order = append(order, k)
		}
	}
	for _, t := range filtered {
		switch field {
		case "column":
			if t.Column != "" { // hidden (unmapped: hide) values have no column
				add(t.Column)
			}
		case "priority":
			add(t.Priority)
		case "type":
			add(t.Type)
		case "assignee":
			add(t.Assignee)
		default:
			labels := t.LabelList()
			if len(labels) == 0 {
				add("")
			}
			for _, l := range labels {
				add(l)
			}
		}
	}
	rows := make([]countRow, 0, len(order))
	total := 0
	for _, k := range order {
		rows = append(rows, *counts[k])
		total += counts[k].Count
	}
	if field != "column" { // biggest first, ties alphabetical
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Count != rows[j].Count {
				return rows[i].Count > rows[j].Count
			}
			return strings.ToLower(rows[i].Key) < strings.ToLower(rows[j].Key)
		})
	}
	code = exitOK
	if partial {
		code = exitError
	}
	if *asJSON {
		if c := writeJSON(out, struct {
			By     string     `json:"by"`
			Total  int        `json:"total"`
			Counts []countRow `json:"counts"`
		}{strings.TrimSuffix(field, "s"), total, rows}); c != exitOK {
			return c
		}
		return code
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "%s\tCOUNT\n", strings.ToUpper(strings.TrimSuffix(field, "s")))
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%d\n", r.Key, r.Count)
	}
	fmt.Fprintf(w, "TOTAL\t%d\n", total)
	w.Flush()
	return code
}

func cmdShow(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(errw)
	asJSON := fs.Bool("json", false, "JSON output")
	// Accept the ID before or after flags.
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(errw, "usage: tkan show ID [-b NAME] [-json]")
		return exitUsage
	}
	id := pos[0]
	t, err := a.find(ctx, *backendName, id)
	if err != nil {
		return fail(errw, err)
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	o := withColumn(b, t)
	if *asJSON {
		return writeJSON(out, o)
	}
	fmt.Fprintf(out, "backend:     %s\nid:          %s\ntitle:       %s\nstatus:      %s\ncolumn:      %s\npriority:    %s\ntype:        %s\nassignee:    %s\ndue:         %s\nstart:       %s\nlabels:      %s\nblocked_by:  %s\ndescription: %s\nnotes:       %s\n",
		t.Backend, t.ID, t.Title, t.Status, o.Column, t.Priority, t.Type, t.Assignee, t.Due, t.Start, t.Labels, t.BlockedBy, t.Description, t.Notes)
	return exitOK
}

// tsvEscape keeps a cell on one line and free of tabs: \, tab, newline and CR are escaped.
var tsvEscape = strings.NewReplacer("\\", "\\\\", "\t", `\t`, "\n", `\n`, "\r", `\r`).Replace

func cmdStatuses(a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("statuses", flag.ContinueOnError)
	fs.SetOutput(errw)
	asJSON := fs.Bool("json", false, "JSON output")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	if *asJSON {
		return writeJSON(out, b.Columns)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "COLUMN\tWRITES\tVALUES")
	for _, c := range b.Columns {
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.Name, c.Write, strings.Join(c.Values, ", "))
	}
	w.Flush()
	return exitOK
}
