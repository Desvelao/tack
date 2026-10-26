package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"tack/internal/backend"
	"tack/internal/hook"
	"tack/internal/query"
	"tack/internal/task"
)

// cmdWatch polls the backends and fires the hooks that need no write of ours
// (task.appeared, task.disappeared, task.moved, task.changed, task.entered.COLUMN,
// task.left.COLUMN, task.due, column.wip_exceeded, reload.ok, reload.fail), as the board does while it is open. With -json it
// also prints each event, one JSON line, to stdout. It runs until interrupted.
func cmdWatch(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(errw)
	q := fs.String("q", "", "only watch the tasks matching this filter (see \"tack help filter\")")
	interval := fs.Duration("interval", 2*time.Second, "time between two polls")
	asJSON := fs.Bool("json", false, "print every event as a JSON line")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	if a == nil {
		return exitOK
	}
	if *interval <= 0 {
		fmt.Fprintln(errw, "tack: -interval must be positive")
		return exitUsage
	}
	var qry query.Query
	if *q != "" {
		var err error
		if qry, err = query.Parse(*q); err != nil {
			fmt.Fprintln(errw, "tack: filter:", err)
			return exitUsage
		}
		// A misspelt field would match nothing and the loop would run for hours
		// firing nothing, so check it against the fields of the tasks now, as
		// list and count do. A backend that cannot be read at all is left to the loop,
		// which reports it on every pass.
		if tasks, err := a.be.List(ctx); err == nil || isPartial(err) {
			if err := qry.Check(query.KnownFields(tasks)); err != nil {
				fmt.Fprintln(errw, "tack: filter:", err)
				return exitUsage
			}
		}
	}
	if a.hooks == nil && !*asJSON {
		fmt.Fprintln(errw, "tack: no hooks configured and no -json: nothing to do")
		return exitError
	}
	if *asJSON {
		emit := func(ev hook.Event) { fmt.Fprint(out, hook.EventJSON(ev)) }
		if a.hooks == nil {
			a.hooks = hook.Observing("watch", emit)
			a.hooks.SetBoard(a.spec)
		} else {
			a.hooks.SetObserver(emit)
		}
	}
	a.hooks.SetSource("watch")
	return watchLoop(ctx, a, qry, *interval, errw)
}

// watchLoop lists on every interval and hands the list to a hook.Watcher until
// ctx ends.
func watchLoop(ctx context.Context, a *app, qry query.Query, interval time.Duration, errw io.Writer) int {
	w := hook.NewWatcher(a.hooks, "watch")
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for first := true; ; first = false {
		if ctx.Err() != nil {
			return exitInterrupted
		}
		if err := watchOnce(ctx, a, w, qry, first); err != nil && w.Reload(err) {
			fmt.Fprintln(errw, "tack: watch:", err)
		}
		select {
		case <-ctx.Done():
			return exitInterrupted
		case <-tick.C:
		}
	}
}

// watchOnce re-reads the backends (the one-shot Set caches its lists) and feeds
// the list to w. A backend that loaded in part still counts, as in the board.
// reload.ok fires on the first pass and when a backend says its store changed.
func watchOnce(ctx context.Context, a *app, w *hook.Watcher, qry query.Query, first bool) error {
	changed, cerr := a.be.Changed()
	rerr := errors.Join(cerr, a.be.Reload())
	tasks, err := a.be.List(ctx)
	var perr *backend.PartialError
	if err != nil && !errors.As(err, &perr) {
		return err
	}
	b, berr := a.board()
	if berr != nil {
		return berr
	}
	if !qry.Empty() {
		deps := b.Deps(tasks)
		kept := make([]task.Task, 0, len(tasks))
		for _, t := range tasks {
			o := withColumn(b, t)
			o.deps, o.Blockers = deps, deps.Blockers(t)
			if qry.Match(o.lookup(), a.spec.Search) {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}
	w.Observe(tasks, perr, a.be.TakeWritten())
	if err == nil {
		err = rerr
	}
	if err == nil && (first || changed) {
		w.Reload(nil)
	}
	return err
}

func isPartial(err error) bool {
	var perr *backend.PartialError
	return errors.As(err, &perr)
}
