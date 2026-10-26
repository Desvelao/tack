package hook

import (
	"time"

	"tack/internal/backend"
	"tack/internal/task"
)

// Watcher turns successive lists of the tasks into the events of a long-running
// process: task.appeared / task.disappeared / task.moved for outside changes,
// task.due and column.wip_exceeded for time and board state, reload.ok and
// reload.fail for the reloads themselves. The TUI and `tack watch` both drive
// one. A nil *Watcher does nothing.
type Watcher struct {
	r         *Runner
	source    string
	prev      []task.Task
	loaded    bool            // a first list was seen (its tasks are not "appeared")
	partial   map[string]bool // backends whose last list was incomplete
	reloadErr string          // last reload failure fired, to fire a repeating one once
}

// NewWatcher returns a Watcher firing through r; source is the Source of the
// reload events ("tui", "watch").
func NewWatcher(r *Runner, source string) *Watcher {
	return &Watcher{r: r, source: source}
}

// Observe takes the latest list. own holds the tasks this process wrote itself
// (Set.TakeWritten): their events fired when they were made. perr names the
// backends that loaded only in part. The first list fires nothing but the
// time-based events.
func (w *Watcher) Observe(tasks []task.Task, perr *backend.PartialError, own map[string]bool) {
	if w == nil {
		return
	}
	// A backend that did not load completely misses tasks in that list; the list
	// where it recovers would show all of them as new. So a backend is left out
	// of the comparison when it was incomplete in either of the two lists.
	skip := map[string]bool{}
	for n := range w.partial {
		skip[n] = true
	}
	w.partial = map[string]bool{}
	if perr != nil {
		for _, n := range perr.Names {
			skip[n], w.partial[n] = true, true
		}
	}
	first := !w.loaded
	w.loaded = true
	prev := w.prev
	w.prev = tasks
	if !w.r.Active() {
		return
	}
	if !first {
		w.r.Diff(prev, tasks, own, skip)
	}
	w.r.Tick(tasks, time.Now())
}

// Tick fires the time-based events again for the last list, for a process that
// does not reload on every pass (task.due after midnight).
func (w *Watcher) Tick() {
	if w != nil && w.loaded {
		w.r.Tick(w.prev, time.Now())
	}
}

// Reload records how a reload went: reload.ok every time, reload.fail once per
// distinct error. It reports whether err is a new failure (or nil), for the
// caller's own logging.
func (w *Watcher) Reload(err error) bool {
	if w == nil {
		return true
	}
	if err == nil {
		w.reloadErr = ""
		w.r.Fire(Event{Name: ReloadOK, Source: w.source})
		return true
	}
	if err.Error() == w.reloadErr {
		return false
	}
	w.reloadErr = err.Error()
	w.r.Fire(Event{Name: ReloadFail, Source: w.source, Err: err.Error()})
	return true
}
