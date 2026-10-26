package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/backend"
	"tack/internal/hook"
	"tack/internal/query"
)

// cmdHelp prints the usage text, the filter syntax or one command's synopsis
// and flags. It needs no data file.
func cmdHelp(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(out, usage, strings.Join(backend.Supported(), ", "))
		return exitOK
	}
	topic := strings.ToLower(args[0])
	switch topic {
	case "filter", "query", "search":
		return filterHelp(args[1:], out, errw)
	case "events", "event":
		return eventsHelp(out)
	}
	if c, ok := findCommand(topic); ok {
		io.WriteString(out, synopsis(topic))
		// Running the command with -h prints its flags (flag parsing happens
		// before the app is touched, so nil is fine).
		switch {
		case c.online != nil:
			c.online(context.Background(), nil, []string{"-h"}, out, out)
		case c.name == "init":
			c.offline([]string{"-h"}, out, out)
		}
		return exitOK
	}
	names := []string{"filter", "events"}
	for _, c := range commandTable() {
		names = append(names, c.name)
	}
	fmt.Fprintf(errw, "tack: no help topic %q (topics: %s)\n", args[0], strings.Join(names, ", "))
	return exitUsage
}

// eventsHelp prints the events a hook can listen to and the placeholders it gets.
func eventsHelp(out io.Writer) int {
	fmt.Fprintln(out, "Hook events (on:). Listening to an event also gets the ones indented under it.")
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, e := range hook.Events() {
		name, task := e.Name, ""
		if e.Parent != "" {
			name = "  " + name
		}
		if !e.HasTask {
			task = " (no task: when: does not apply)"
		}
		fmt.Fprintf(w, "  %s\t%s%s\n", name, e.Desc, task)
	}
	w.Flush()
	ph := hook.ExtraPlaceholders()
	for i, p := range ph {
		ph[i] = "{" + p + "}"
	}
	fmt.Fprintf(out, "\nBesides the task placeholders a hook command gets %s;\ntry one with: tack hooks test NAME ID -event EVENT -n\n", strings.Join(ph, " "))
	return exitOK
}

// synopsis extracts a command's lines from the usage text.
func synopsis(name string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(usage, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "tack ") {
			f := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(t, "tack "), "[-c CONFIG] "))
			in = len(f) > 0 && f[0] == name
		} else if !strings.HasPrefix(line, "  ") {
			in = false
		}
		if in {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// filterHelp prints the filter syntax and, for the source asked for (tasks by
// default, or audit / runs), what each field means.
func filterHelp(args []string, out, errw io.Writer) int {
	which := "tasks"
	if len(args) > 0 {
		which = strings.ToLower(args[0])
	}
	var title string
	var docs []query.FieldDoc
	var search []string
	switch which {
	case "tasks", "task", "list", "fields", "field":
		title, docs, search = "Task fields (board, list, count)", query.TaskFieldDocs(nil), nil
	case "audit":
		title, docs, search = "Audit fields (tack audit, A in the board)", audit.FieldDocs, audit.SearchFields
	case "runs", "run", "actions":
		title, docs, search = "Action run fields (tack runs, R in the board)", actionlog.FieldDocs, actionlog.SearchFields
	default:
		fmt.Fprintf(errw, "tack: no filter help for %q (tasks, audit, runs)\n", args[0])
		return exitUsage
	}
	io.WriteString(out, query.Help(search)+"\n")
	io.WriteString(out, query.FieldsHelp(title, docs, search))
	if which == "tasks" || which == "task" || which == "list" || which == "fields" || which == "field" {
		io.WriteString(out, "Plus any other column of the file, by its name (spaces as underscores).\n")
	}
	io.WriteString(out, "Other sources: tack help filter audit | runs\n")
	return exitOK
}

// fieldsUsage is a flag set's -h output: the flags, then what each field of its
// -q filter means.
func fieldsUsage(fs *flag.FlagSet, title string, docs []query.FieldDoc, search []string) func() {
	return func() {
		fmt.Fprintf(fs.Output(), "Usage of %s:\n", fs.Name())
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), "\n"+query.FieldsHelp(title, docs, search))
	}
}
