package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/hook"
	"tack/internal/task"
)

// cmdHooks lists the configured hooks (`tack hooks`) or fires one by hand
// (`tack hooks test NAME [ID]`): the event is made up, the `when:` filter and
// the depth limit apply as for a real one, -n only prints the command.
func cmdHooks(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("hooks", flag.ContinueOnError)
	fs.SetOutput(errw)
	asJSON := fs.Bool("json", false, "JSON output (list)")
	backendName := fs.String("b", "", "backend name (test: needed when the ID exists in several)")
	event := fs.String("event", "", "test: the event to fire (default: the hook's first on: event; see tack help events)")
	oldStatus := fs.String("old-status", "", "test: the previous status (task.moved, task.update.success)")
	dry := fs.Bool("n", false, "test: print the command that would run and exit")
	pos, err := parseArgs(fs, args)
	if err != nil || (len(pos) > 0 && (pos[0] != "test" || len(pos) < 2 || len(pos) > 3)) {
		fmt.Fprintln(errw, "usage: tack hooks [-json] | tack hooks test NAME [ID] [-b NAME] [-event E] [-old-status S] [-n]")
		return exitUsage
	}
	if len(pos) == 0 {
		return listHooks(a, *asJSON, out, errw)
	}
	id := ""
	if len(pos) == 3 {
		id = pos[2]
	}
	return testHook(ctx, a, pos[1], id, *backendName, *event, *oldStatus, *dry, out, errw)
}

// hookRow is one hook as `tack hooks` prints it.
type hookRow struct {
	Name    string   `json:"name"`
	Backend string   `json:"backend,omitempty"`
	On      []string `json:"on"`
	When    string   `json:"when,omitempty"`
	Command string   `json:"command"`
	Timeout string   `json:"timeout"`
}

func listHooks(a *app, asJSON bool, out, errw io.Writer) int {
	rows := []hookRow{}
	for _, s := range a.cfg.AllHooks() {
		r := hookRow{Name: s.Name, Backend: s.Backend, On: s.On, When: s.When, Timeout: s.Timeout}
		if r.Timeout == "" {
			r.Timeout = action.DefaultTimeout.String()
		}
		// The command as configured, placeholders unfilled.
		r.Command = action.Plan{Argv: s.Command}.Line()
		rows = append(rows, r)
	}
	if asJSON {
		return writeJSON(out, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(errw, "tack: no hooks configured (see docs/config.md)")
		return exitOK
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tBACKEND\tON\tWHEN\tTIMEOUT\tCOMMAND")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, dash(r.Backend), strings.Join(r.On, ","), dash(r.When), r.Timeout, r.Command)
	}
	w.Flush()
	return exitOK
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func testHook(ctx context.Context, a *app, name, id, backendName, event, oldStatus string, dry bool, out, errw io.Writer) int {
	var names []string
	var first *hook.Spec // any hook of that name: its on: picks the default event
	all := a.cfg.AllHooks()
	for i := range all {
		names = append(names, all[i].Name)
		if first == nil && strings.EqualFold(all[i].Name, name) {
			first = &all[i]
		}
	}
	if first == nil {
		if len(names) == 0 {
			fmt.Fprintf(errw, "tack: no hook %q (the config defines no `hooks:`; see docs/config.md)\n", name)
		} else {
			fmt.Fprintf(errw, "tack: no hook %q (hooks: %s)\n", name, strings.Join(names, ", "))
		}
		return exitUsage
	}
	ev, err := hook.TestEvent(first, event, task.Task{}, "")
	if err != nil {
		fmt.Fprintln(errw, "tack:", err)
		return exitUsage
	}
	needsTask := hook.HasTask(ev.Name)
	if needsTask && id == "" {
		fmt.Fprintf(errw, "tack: %s carries a task: give its ID (tack hooks test %s ID)\n", ev.Name, name)
		return exitUsage
	}
	if !needsTask && id != "" {
		fmt.Fprintf(errw, "tack: %s carries no task; drop the ID\n", ev.Name)
		return exitUsage
	}
	var t task.Task
	if needsTask {
		if t, err = a.find(ctx, backendName, id); err != nil {
			return fail(errw, err)
		}
	}
	s, ok := a.hooks.Find(name, t.Backend)
	if !ok {
		fmt.Fprintf(errw, "tack: hook %q does not apply to the tasks of backend %s\n", name, t.Backend)
		return exitUsage
	}
	if ev, err = hook.TestEvent(s, ev.Name, t, oldStatus); err != nil {
		fmt.Fprintln(errw, "tack:", err)
		return exitUsage
	}
	if why := a.hooks.Check(s, ev); why != "" {
		fmt.Fprintf(errw, "tack: precondition failed: hook %s would not run on %s: %s\n", s.Name, ev.Name, why)
		return exitPrecondition
	}
	if dry {
		p := a.hooks.Preview(s, ev)
		if p.Err != nil {
			return fail(errw, p.Err)
		}
		fmt.Fprintf(out, "cd %s && %s\n", action.ShellWord(p.Dir), p.Line())
		for _, kv := range p.Env {
			fmt.Fprintln(out, "env", action.ShellWord(kv))
		}
		if p.Stdin != "" {
			fmt.Fprintf(out, "stdin: %s (%d bytes)\n", s.Stdin, len(p.Stdin))
		}
		return exitOK
	}
	rec := a.hooks.Test(s, ev)
	if rec.Stdout != "" {
		io.WriteString(out, rec.Stdout)
		if !strings.HasSuffix(rec.Stdout, "\n") {
			fmt.Fprintln(out)
		}
	}
	if s := strings.TrimSpace(rec.Stderr); s != "" && rec.Error == "" {
		fmt.Fprintln(errw, s)
	}
	if rec.Result != actionlog.ResultOK {
		fmt.Fprintf(errw, "tack: hook %s: %s\n", s.Name, rec.Error)
		return exitError
	}
	return exitOK
}
