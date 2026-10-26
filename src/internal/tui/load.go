package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/task"
)

// spinEvery is the tick while a backend is loading, so its spinner turns.
const spinEvery = 100 * time.Millisecond

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// backendLoadedMsg is one backend's list, loaded in the background.
type backendLoadedMsg struct {
	idx   int
	tasks []task.Task
	err   error
}

// asyncLoad is the state of a board that lists its backends in the background.
type asyncLoad struct {
	validate func([]task.Task) error // checks the board spec against the first complete load (nil = none)
	loading  map[string]bool         // backends whose list is still running
	got      [][]task.Task           // lists that arrived, by backend index (config order)
	errs     map[string]error        // backends whose last list failed
	spin     int                     // spinner frame
	reason   string                  // why a refresh runs ("" = the first load)
	notify   bool                    // a manual reload: say "reloaded" at the end
}

// NewAsync builds a model that opens at once: the configured columns are drawn
// empty and each backend of be is listed in the background (see Init), with a
// loading indicator until its cards arrive. validate, if set, checks the board
// spec against the tasks once every backend has answered.
func NewAsync(be *backend.Set, name string, spec board.Spec, validate func([]task.Task) error) Model {
	m := bareModel(be, name, spec)
	names := be.Names()
	m.async = &asyncLoad{validate: validate, loading: map[string]bool{}, got: make([][]task.Task, len(names)), errs: map[string]error{}}
	for _, n := range names {
		m.async.loading[n] = true
	}
	if b, err := board.New(spec, nil); err == nil {
		m.board = b
		m.deps = b.Deps(nil)
	}
	return m
}

// loadAll starts the list of every backend.
func (m Model) loadAll() tea.Cmd {
	cmds := make([]tea.Cmd, len(m.be.Names()))
	for i := range cmds {
		cmds[i] = m.loadOne(i)
	}
	return tea.Batch(cmds...)
}

func (m Model) loadOne(i int) tea.Cmd {
	be := m.be
	return func() tea.Msg {
		tasks, err := be.ListOne(context.Background(), i)
		return backendLoadedMsg{idx: i, tasks: tasks, err: err}
	}
}

// startRefresh lists every backend again in the background; the cards are
// replaced once all have answered. A no-op while a load is running.
func (m *Model) startRefresh(reason string, manual bool) tea.Cmd {
	if len(m.async.loading) > 0 {
		return nil
	}
	for _, n := range m.be.Names() {
		m.async.loading[n] = true
	}
	m.async.got = make([][]task.Task, len(m.be.Names()))
	m.async.reason, m.async.notify = reason, manual
	return m.loadAll()
}

// loadingNames lists the backends still loading, config order.
func (m Model) loadingNames() []string {
	var out []string
	for _, n := range m.be.Names() {
		if m.async != nil && m.async.loading[n] {
			out = append(out, n)
		}
	}
	return out
}

// busy reports whether a background list is running.
func (m Model) busy() bool { return m.async != nil && len(m.async.loading) > 0 }

// writeDoneMsg is a backend write that ran in the background; done finishes it on the model.
type writeDoneMsg struct {
	err  error
	done func(*Model, error)
}

// write runs fn, a backend write. On the interactive board it runs in the
// background (the caller has already shown the result on the board), so a slow
// backend never freezes the keys; elsewhere it runs at once. done gets fn's error.
func (m *Model) write(fn func() error, done func(*Model, error)) tea.Cmd {
	if m.async == nil {
		done(m, fn())
		return nil
	}
	m.writing++
	if m.busy() {
		m.wantRefresh = "after write" // the list that is running may not include it
	}
	return func() tea.Msg { return writeDoneMsg{err: fn(), done: done} }
}

// idle says nothing is listing or writing in the background.
func (m Model) idle() bool { return !m.busy() && m.writing == 0 }

// reloadNow lists the backends again: at once, or (interactive board) in the
// background as soon as nothing else is running, from the next Update.
func (m *Model) reloadNow(reason string) {
	if m.async == nil {
		m.refresh(reason)
		return
	}
	m.wantRefresh = reason
}

// showLocal puts t on the board in place of the card with the same key (added
// if new, taken away if del) without listing the backend: the interactive board
// shows a write at once and does not wait for the slow list that would confirm it.
func (m *Model) showLocal(t task.Task, del bool) { m.showLocalMany([]task.Task{t}, del) }

// showLocalMany is showLocal for several cards, with one rebuild of the board.
func (m *Model) showLocalMany(ts []task.Task, del bool) {
	if m.async == nil {
		return // the plain board lists again after every write
	}
	in := map[cardKey]task.Task{}
	for _, t := range ts {
		in[keyOf(t)] = t
	}
	tasks := make([]task.Task, 0, len(m.tasks)+len(ts))
	for _, o := range m.tasks {
		if t, ok := in[keyOf(o)]; ok {
			delete(in, keyOf(o))
			if !del {
				tasks = append(tasks, t)
			}
			continue
		}
		tasks = append(tasks, o)
	}
	if !del {
		for _, t := range ts {
			if _, left := in[keyOf(t)]; left {
				tasks = append(tasks, t)
			}
		}
	}
	b, err := board.New(m.spec, m.be.Statuses())
	if err != nil {
		m.fail("error: " + err.Error())
		return
	}
	m.applyTasks(tasks, b, nil, false)
}

// loaded handles one backend's answer.
func (m *Model) backendLoaded(msg backendLoadedMsg) {
	a := m.async
	name := m.be.Names()[msg.idx]
	delete(a.loading, name)
	a.got[msg.idx] = msg.tasks
	if msg.err != nil {
		a.errs[name] = msg.err
	} else {
		delete(a.errs, name)
	}
	done := len(a.loading) == 0
	if !m.loaded || done { // the first load shows each backend as it arrives; a refresh swaps once
		m.applyAsync(done)
	}
	if !done {
		return
	}
	m.finishAsync()
}

// applyAsync shows the lists that arrived so far; final says every backend answered.
func (m *Model) applyAsync(final bool) {
	a := m.async
	var tasks []task.Task
	for _, ts := range a.got {
		tasks = append(tasks, ts...)
	}
	var perr *backend.PartialError
	for _, n := range m.be.Names() { // config order
		if err, bad := a.errs[n]; bad {
			if perr == nil {
				perr = &backend.PartialError{}
			}
			perr.Names = append(perr.Names, n)
			perr.Errs = append(perr.Errs, err)
		}
	}
	b, err := board.New(m.spec, m.be.Statuses())
	if err != nil {
		m.fail("error: " + err.Error())
		return
	}
	m.applyTasks(tasks, b, perr, final)
	if perr != nil {
		m.fail("error: " + perr.Error())
	}
}

// finishAsync runs once the last backend answered.
func (m *Model) finishAsync() {
	a := m.async
	var err error
	var perr *backend.PartialError
	for _, n := range m.be.Names() {
		if e, bad := a.errs[n]; bad {
			if perr == nil {
				perr = &backend.PartialError{}
			}
			perr.Names = append(perr.Names, n)
			perr.Errs = append(perr.Errs, e)
		}
	}
	if perr != nil {
		err = perr
	}
	reason := a.reason
	if reason == "" {
		reason = "start"
	}
	m.auditReload(reason, err)
	if err == nil && a.validate != nil {
		if verr := a.validate(m.tasks); verr != nil {
			m.fail("error: " + verr.Error())
		}
	}
	if a.notify && err == nil {
		m.notice = "reloaded"
	}
	a.validate, a.reason, a.notify = nil, "", false // the spec is checked once
}

// loadingTag is the header text for the backends still loading: a spinner and
// their names, or just "loading" with a single backend.
func (m Model) loadingTag() string {
	names := m.loadingNames()
	if len(names) == 0 && m.writing == 0 {
		return ""
	}
	frame := spinFrames[m.async.spin%len(spinFrames)]
	var parts []string
	for _, n := range names {
		if len(m.be.Names()) == 1 {
			parts = append(parts, frame+" loading")
		} else {
			parts = append(parts, frame+" "+n)
		}
	}
	if m.writing > 0 {
		parts = append(parts, frame+" saving")
	}
	return strings.Join(parts, "  ")
}

// failedTag names the backends whose last list failed.
func (m Model) failedTag() string {
	if m.async == nil || len(m.async.errs) == 0 {
		return ""
	}
	var parts []string
	for _, n := range m.be.Names() {
		if _, bad := m.async.errs[n]; bad {
			parts = append(parts, "✗ "+n)
		}
	}
	return strings.Join(parts, "  ")
}

// writeKeys change data; they wait until the backends have listed, so nothing
// is written against a board that is still empty.
var writeKeys = map[string]bool{
	"n": true, "e": true, "d": true, "u": true, "x": true, ".": true,
	"H": true, "L": true, "shift+left": true, "shift+right": true,
}

// blocksWrite says key must wait for the load; it sets the notice.
func (m *Model) blocksWrite(key string) bool {
	if m.idle() || !writeKeys[key] {
		return false
	}
	m.notice = m.busyNotice()
	return true
}

// busyNotice says what a write has to wait for.
func (m Model) busyNotice() string {
	if m.busy() {
		return fmt.Sprintf("still loading: %s", strings.Join(m.loadingNames(), ", "))
	}
	return "still saving"
}

// cardOf is the card on the board with the key of t.
func (m Model) cardOf(t task.Task) (task.Task, bool) {
	for _, o := range m.tasks {
		if o.Backend == t.Backend && o.ID == t.ID {
			return o, true
		}
	}
	return task.Task{}, false
}
