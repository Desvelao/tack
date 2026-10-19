// Package tui renders a backend.Backend as a kanban board with Bubble Tea.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tkan/internal/action"
	"tkan/internal/actionlog"
	"tkan/internal/audit"
	"tkan/internal/backend"
	"tkan/internal/board"
	"tkan/internal/hook"
	"tkan/internal/query"
	"tkan/internal/task"
)

type mode int

const (
	modeBoard mode = iota
	modeDetail
	modeNew
	modeConfirmDelete
	modeFilter
	modeHelp
	modeEdit
	modeQuick     // `f`: waiting for the field to filter by
	modeAudit     // `A`: the audit trail
	modeActions   // `x`: the action picker
	modeRuns      // `R`: the background runs
	modeRunOutput // output of one run
	modeFleet     // `F`: agents and the tasks they hold
	modeMoveTo    // `m`: waiting for the column number to move the card(s) to
)

const pollEvery = time.Second

// liveEvery is the tick while a running action's output is on screen.
const liveEvery = 250 * time.Millisecond

// undoOp reverses one change: restore the task as it was (move, edit), or
// create it again (delete, which may give it a new ID).
type undoOp struct {
	what     string
	prev     task.Task
	recreate bool
	group    []task.Task // recreate: all the cards of a delete of marked cards (prev is the first)
}

const maxUndo = 30

func (m *Model) pushUndo(op undoOp) {
	m.undo = append(m.undo, op)
	if len(m.undo) > maxUndo {
		m.undo = m.undo[1:]
	}
}

type noticeKind int

const (
	noticeInfo noticeKind = iota
	noticeOK
	noticeFail
)

// okLifetime is how long a success confirmation stays on screen (polled by tickMsg).
const okLifetime = 4 * time.Second

func (m *Model) setNotice(kind noticeKind, text string) {
	// The notice is one line and may carry text from outside (a command's error): keep it drawable.
	m.notice, m.noticeKind, m.noticeAt = oneLine(text), kind, m.clock()
}

// ok reports a backend write that succeeded; fail one that did not.
func (m *Model) ok(text string)   { m.setNotice(noticeOK, text) }
func (m *Model) fail(text string) { m.setNotice(noticeFail, text) }

func (m *Model) clearNotice() { m.notice, m.noticeKind = "", noticeInfo }

// undoLast reverses the most recent move, edit or delete.
func (m *Model) undoLast() tea.Cmd {
	if len(m.undo) == 0 {
		m.notice = "nothing to undo"
		return nil
	}
	op := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	ctx := backend.WithSource(backend.WithNote(context.Background(), "undo of "+op.what), "undo")
	be := m.be
	if op.recreate && len(op.group) > 1 {
		return m.recreateGroup(ctx, op)
	}
	if op.recreate {
		var t task.Task
		return m.write(func() (err error) { t, err = be.Create(ctx, op.prev); return }, func(m *Model, err error) {
			if err != nil {
				m.fail("undo failed: " + err.Error())
				return
			}
			if m.async != nil {
				m.showLocal(t, false)
			} else if m.reload() != nil {
				return
			}
			m.selectID(t.Backend, t.ID)
			m.ok("undid " + op.what)
			if t.ID != op.prev.ID {
				m.notice += fmt.Sprintf(" (restored as #%s, the old #%s is gone)", t.ID, op.prev.ID)
			}
		})
	}
	if len(op.group) > 1 {
		return m.restoreGroup(ctx, op)
	}
	cur, shown := m.cardOf(op.prev)
	hidden := false
	if m.async != nil { // show the old card at once; the write confirms it
		m.showLocal(op.prev, false)
		hidden = !m.selectID(op.prev.Backend, op.prev.ID) && m.filter != ""
	}
	return m.write(func() error { return be.Update(ctx, op.prev) }, func(m *Model, err error) {
		if err != nil {
			m.fail("undo failed: " + err.Error())
			if shown {
				m.showLocal(cur, false)
			}
			return
		}
		if m.async == nil {
			if m.reload() != nil {
				return
			}
			hidden = !m.selectID(op.prev.Backend, op.prev.ID) && m.filter != ""
		}
		if hidden {
			m.ok("undid " + op.what + hiddenHint)
			return
		}
		m.ok("undid " + op.what)
	})
}

type tickMsg time.Time

// Model is the Bubble Tea model. Backend calls are synchronous: fine for
// local files; an API backend should cache inside its implementation.
type Model struct {
	be         *backend.Set
	name       string
	target     int             // index of the backend new tasks are created in
	sortKeys   []board.SortKey // session sort set with s/S; nil = use spec.Sort
	sortIdx    int             // position of the s-cycle in m.fields (0 = configured sort)
	tasks      []task.Task
	spec       board.Spec
	board      board.Board
	deps       board.Deps // which cards are blocked (blocked_by), from the last load
	col, row   int
	mode       mode
	input      textinput.Model
	filter     string           // text of the active filter ("" = none)
	query      query.Query      // parsed filter
	fields     []string         // field names usable in a filter (for validation and help)
	helpPage   int              // 0 = keys, 1 = filter syntax, 2 = filter fields
	helpDocs   []query.FieldDoc // fields page of the audit / runs views (nil = the board's tasks)
	helpTitle  string
	helpSearch []string
	helpBack   mode // mode to return to when the help closes
	width      int
	height     int
	notice     string
	// noticeKind says how the notice is drawn (OK / FAIL / plain); noticeAt is
	// when it was set, so an OK fades after okLifetime while a FAIL stays until a key.
	noticeKind noticeKind
	noticeAt   time.Time
	edit       editForm
	detailY    int              // first visible line of the detail view
	groupBy    string           // field the cards of a column are grouped by ("" = none)
	now        func() time.Time // clock for due-date flags; nil = time.Now
	undo       []undoOp         // newest last; u pops one
	history    []string         // filters applied this session, newest last (up/down in the / prompt)
	histPos    int              // position while browsing history; len(history) = not browsing
	view       string           // name of the saved view behind the current filter ("" = none)

	audit        *audit.Log  // nil = no audit view
	auditQ       query.Query // filter of the audit view
	auditFilter  string
	auditY       int    // first visible line of the audit view
	auditEditing bool   // the audit filter prompt is open
	reloadErr    string // last reload failure recorded, to log a repeating one once

	allActions []action.Spec    // configured actions, of every backend (action.For picks a card's)
	actions    []action.Spec    // the `x` picker's list, for the cards it was opened on
	actionDir  string           // directory their relative cwd resolves against
	actionIdx  int              // highlighted action in the picker
	actionTTY  bool             // `i` in the picker: run the next action with the terminal
	actionBack mode             // mode the picker returns to (board or detail)
	lastAction action.Spec      // the action started last (`.` repeats it; Name "" = none yet)
	lastTTY    bool             // it ran with the terminal because of `i`
	unseen     int              // background runs that ended since the runs view (R) was last opened
	term       io.Writer        // where notify writes the bell / desktop notification; nil = os.Stdout
	marks      map[cardKey]bool // cards marked with space (by backend and ID); x runs on them
	batch      *batch           // action going over the marked cards (nil = none)
	actionMax  int              // runs of a batch at once (action_max_parallel)
	dupArm     dupStart         // a second start of a running action that was warned about

	runs       *runLog            // background runs, shared by the copies of the Model
	runIdx     int                // highlighted run in the runs view (0 = newest)
	runBack    mode               // mode the runs view returns to
	runCur     runItem            // the run whose input and output are shown
	runBackOut mode               // mode the output view returns to
	runY       int                // first visible output line when not following
	runFollow  bool               // keep the end of a running command's output in view
	runStored  []actionlog.Record // earlier sessions' runs from the action log, newest first
	runOlder   int                // stored runs left unread (only the newest runsLoad are loaded)
	runFilter  string             // filter of the runs view
	runQ       query.Query
	runEditing bool // the runs filter prompt is open

	fleetIdx int // highlighted row of the fleet view

	alog *actionlog.Log // optional detailed log of action runs (nil = off)

	partial       map[string]bool // backends whose last list was incomplete (see diffHooks)
	pending       []func(*Model)  // work an action run left for when the user leaves a form (see finishAction)
	deleteTargets []task.Task     // the cards the open delete confirmation is about (the marked ones, else the selected)

	hooks       *hook.Runner // optional event hooks (nil = none)
	loaded      bool         // a first load happened (its tasks are not "appeared")
	async       *asyncLoad   // background listing (NewAsync); nil = every load is synchronous
	writing     int          // backend writes running in the background (interactive board only)
	wantRefresh string       // why a background list is waiting for the board to be idle ("" = none)
}

// editForm is the state of the card edit form (modeEdit).
type editForm struct {
	orig   task.Task
	inputs []textinput.Model // parallel to editFields
	loaded []string          // input values as loaded, to detect which fields were edited
	idx    int
	armed  bool // esc was pressed once with unsaved changes
}

// editFields are the card fields the form edits; id, status and backend are not editable.
var editFields = []struct {
	name string
	ptr  func(*task.Task) *string
}{
	{"title", func(t *task.Task) *string { return &t.Title }},
	{"description", func(t *task.Task) *string { return &t.Description }},
	{"priority", func(t *task.Task) *string { return &t.Priority }},
	{"type", func(t *task.Task) *string { return &t.Type }},
	{"assignee", func(t *task.Task) *string { return &t.Assignee }},
	{"due", func(t *task.Task) *string { return &t.Due }},
	{"start", func(t *task.Task) *string { return &t.Start }},
	{"labels", func(t *task.Task) *string { return &t.Labels }},
	{"blocked_by", func(t *task.Task) *string { return &t.BlockedBy }},
	{"notes", func(t *task.Task) *string { return &t.Notes }},
}

// New builds a model for the backends in be; name is shown in the header and spec defines the columns.
func New(be *backend.Set, name string, spec board.Spec) Model {
	m := bareModel(be, name, spec)
	m.reload()
	return m
}

func bareModel(be *backend.Set, name string, spec board.Spec) Model {
	in := textinput.New()
	in.CharLimit = 200
	return Model{be: be, name: name, spec: spec, input: in, width: 80, height: 24, groupBy: query.Normalize(spec.Group)}
}

// WithAudit gives the model the audit trail shown by `A`.
func (m Model) WithAudit(l *audit.Log) Model {
	m.audit = l
	m.be.SetAuditor(l)
	return m
}

// refresh reloads and records it in the audit trail. A failure that repeats
// (the tick retries every second) is recorded once.
func (m *Model) refresh(reason string) {
	err := m.reload()
	m.auditReload(reason, err)
}

func (m *Model) auditReload(reason string, err error) {
	if err == nil {
		m.reloadErr = ""
		m.hooks.Fire(hook.Event{Name: hook.ReloadOK, Source: "tui"})
		m.be.Audit(audit.OpReload, "", "", nil, fmt.Sprintf("%s: %d tasks", reason, len(m.tasks)))
		return
	}
	if err.Error() != m.reloadErr {
		m.reloadErr = err.Error()
		m.be.Audit(audit.OpReload, "", "", err, reason)
		m.hooks.Fire(hook.Event{Name: hook.ReloadFail, Source: "tui", Err: err.Error()})
	}
}

func (m *Model) reload() error {
	tasks, err := m.be.List(context.Background())
	var perr *backend.PartialError
	loadErr := err
	switch {
	case errors.As(err, &perr): // show what loaded, say what did not
		m.fail("error: " + err.Error())
	case err != nil:
		m.fail("error: " + err.Error())
		return err
	}
	b, err := board.New(m.spec, m.be.Statuses())
	if err != nil {
		m.fail("error: " + err.Error())
		return err
	}
	m.applyTasks(tasks, b, perr, true)
	return loadErr
}

// applyTasks shows tasks on board b. hooks says to compare with the previous
// list for the event hooks (false while a first load is still arriving).
func (m *Model) applyTasks(tasks []task.Task, b board.Board, perr *backend.PartialError, hooks bool) {
	prev, hadSel := m.selected()
	if hooks {
		m.diffHooks(tasks, perr)
	}
	m.tasks = tasks
	m.pruneMarks(perr)
	m.board = b
	m.deps = b.Deps(tasks)
	m.fields = query.KnownFields(tasks)
	m.clamp()
	// Keep the cursor on the same card when the data changed underneath it.
	if hadSel {
		m.selectID(prev.Backend, prev.ID)
	}
}

// lookup is query.TaskLookup plus what only the board knows: running is
// "yes" while a background action of this session runs on the card.
func (m Model) lookup(t task.Task, colName string) query.Lookup {
	get := query.TaskLookup(t, colName, m.deps)
	return func(field string) (string, bool) {
		if field == query.RunningField {
			for _, r := range m.runs.forCard(keyOf(t)) {
				if !r.done {
					return "yes", true
				}
			}
			return "no", true
		}
		return get(field)
	}
}

// isAgent reports whether assignee is one of board.agents (case-insensitive).
func (m Model) isAgent(assignee string) bool {
	a := strings.TrimSpace(assignee)
	if a == "" {
		return false
	}
	for _, n := range m.spec.Agents {
		if strings.EqualFold(strings.TrimSpace(n), a) {
			return true
		}
	}
	return false
}

// columnTotal counts every card in column c, ignoring the filter (WIP limits
// are about the real column, not what is currently shown).
func (m Model) columnTotal(c int) int {
	n := 0
	for _, t := range m.tasks {
		if m.board.ColumnOf(t.Status) == c {
			n++
		}
	}
	return n
}

// column returns the visible tasks of column c (filter applied).
func (m Model) column(c int) []task.Task {
	if c < 0 || c >= len(m.board.Columns) {
		return nil
	}
	colName := m.board.Columns[c].Name
	var out []task.Task
	for _, t := range m.tasks {
		if m.board.ColumnOf(t.Status) != c {
			continue
		}
		if !m.query.Empty() && !m.query.Match(m.lookup(t, colName), m.spec.Search) {
			continue
		}
		out = append(out, t)
	}
	if keys := m.sortFor(c); len(keys) > 0 {
		sort.SliceStable(out, func(i, j int) bool {
			return query.CompareBy(m.lookup(out[i], colName), m.lookup(out[j], colName), keys) < 0
		})
	}
	if m.groupBy != "" { // keep the sorted order inside each group; groups in order of first appearance
		first := map[string]int{}
		for _, t := range out {
			g := m.groupLabel(t, colName)
			if _, ok := first[g]; !ok {
				first[g] = len(first)
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			return first[m.groupLabel(out[i], colName)] < first[m.groupLabel(out[j], colName)]
		})
	}
	return out
}

// groupFields is what `z` cycles through (after "none"); a configured
// board.group that is not listed here is added in front.
var groupFields = []string{"priority", "type", "assignee", "labels"}

// groupLabel is t's group in the current group-by field ("(none)" when empty).
// Cards with several labels are grouped under their first one.
func (m Model) groupLabel(t task.Task, colName string) string {
	v, _ := m.lookup(t, colName)(m.groupBy)
	if m.groupBy == "labels" {
		v = ""
		if l := t.LabelList(); len(l) > 0 {
			v = l[0]
		}
	}
	if v = strings.TrimSpace(v); v == "" {
		return "(none)"
	}
	return v
}

// cycleGroup moves group-by to the next field, then off.
func (m *Model) cycleGroup() {
	cur, hadSel := m.selected()
	fields := append([]string(nil), groupFields...)
	if g := query.Normalize(m.spec.Group); g != "" {
		found := false
		for _, f := range fields {
			found = found || f == g
		}
		if !found {
			fields = append([]string{g}, fields...)
		}
	}
	next := ""
	if m.groupBy == "" {
		next = fields[0]
	} else {
		for i, f := range fields {
			if f == m.groupBy && i+1 < len(fields) {
				next = fields[i+1]
			}
		}
	}
	m.groupBy = next
	if next == "" {
		m.notice = "grouping off"
	} else {
		m.notice = "grouped by " + next
	}
	if hadSel {
		m.selectID(cur.Backend, cur.ID)
	}
	m.clamp()
}

// sortFor is the order inside column c: the session sort if one was chosen
// (s/S), else the column's own `sort`, else board.sort.
func (m Model) sortFor(c int) []board.SortKey {
	if m.sortKeys == nil && len(m.board.Columns[c].Sort) > 0 {
		return m.board.Columns[c].Sort
	}
	return m.activeSort()
}

// activeSort is the session sort if one was chosen, else board.sort. It only
// affects the display order; backends are never told.
func (m Model) activeSort() []board.SortKey {
	if m.sortKeys != nil {
		return m.sortKeys
	}
	return m.spec.Sort
}

// cycleSort moves the session sort to the next field of m.fields (ascending);
// after the last field it returns to the configured sort.
func (m *Model) cycleSort() {
	cur, hadSel := m.selected()
	m.sortIdx++
	if m.sortIdx > len(m.fields) {
		m.sortIdx = 0
	}
	if m.sortIdx == 0 {
		m.sortKeys = nil
	} else {
		m.sortKeys = []board.SortKey{{Field: query.Normalize(m.fields[m.sortIdx-1])}}
	}
	m.afterSort(cur, hadSel)
}

// reverseSort flips the direction of the first active sort key.
func (m *Model) reverseSort() {
	keys := append([]board.SortKey(nil), m.activeSort()...)
	if len(keys) == 0 {
		m.notice = "no sort active (s picks a field)"
		return
	}
	cur, hadSel := m.selected()
	keys[0].Desc = !keys[0].Desc
	m.sortKeys = keys
	m.afterSort(cur, hadSel)
}

// afterSort keeps the cursor on the same card after the order changed.
func (m *Model) afterSort(cur task.Task, hadSel bool) {
	if hadSel {
		m.selectID(cur.Backend, cur.ID)
	}
	m.clamp()
}

func (m Model) selected() (task.Task, bool) {
	c := m.column(m.col)
	if m.row < 0 || m.row >= len(c) {
		return task.Task{}, false
	}
	return c[m.row], true
}

func (m *Model) clamp() {
	if m.col >= len(m.board.Columns) {
		m.col = len(m.board.Columns) - 1
	}
	if m.col < 0 {
		m.col = 0
	}
	if n := len(m.column(m.col)); m.row >= n {
		m.row = n - 1
	}
	if m.row < 0 {
		m.row = 0
	}
}

func (m Model) Init() tea.Cmd {
	if m.async != nil {
		return tea.Batch(tick(spinEvery), m.loadAll())
	}
	return tick(pollEvery)
}

// tickEvery is the next tick: fast only while a run that is going is visible
// (its output, or the runs list), so the output follows it without a permanent fast tick.
func (m Model) tickEvery() time.Duration {
	if m.async != nil && !m.idle() {
		return spinEvery
	}
	switch m.mode {
	case modeFleet:
		return animEvery
	case modeRunOutput:
		if _, running := m.recordOf(m.runCur); running {
			return liveEvery
		}
	case modeRuns:
		for _, r := range m.runs.newest() {
			if !r.done {
				return liveEvery
			}
		}
	}
	return pollEvery
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	if nm, ok := next.(Model); ok {
		if len(nm.pending) > 0 {
			nm.flushPending()
		}
		if nm.wantRefresh != "" && nm.async != nil && nm.idle() && !nm.formOpen() {
			reason := nm.wantRefresh
			nm.wantRefresh = ""
			cmd = tea.Batch(cmd, nm.startRefresh(reason, false))
		}
		next = nm
	}
	return next, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		for i := range m.edit.inputs {
			m.edit.inputs[i].Width = m.editWidth()
		}
	case tickMsg:
		if m.async != nil && !m.idle() {
			m.async.spin++ // the loading spinner turns on every tick
		}
		if m.noticeKind == noticeOK && m.clock().Sub(m.noticeAt) >= okLifetime {
			m.clearNotice() // a success fades; a failure stays until a key
		}
		if m.audit != nil {
			if err := m.audit.Err(); err != nil { // the trail could not be written; the write itself went through
				m.fail("error: " + err.Error())
			}
		}
		if m.mode == modeBoard || m.mode == modeFleet {
			changed, err := m.be.Changed()
			if err != nil {
				if m.notice == "" {
					m.fail("error: " + err.Error())
				}
				m.auditReload("auto: checking for changes", err)
			}
			if changed {
				m.reloadNow("auto: file changed")
			}
		}
		return m, tick(m.tickEvery())
	case actionDoneMsg:
		m.finishAction(msg)
		if msg.batch {
			return m, m.fillBatch()
		}
	case backendLoadedMsg:
		if m.async != nil {
			m.backendLoaded(msg)
		}
	case writeDoneMsg:
		m.writing--
		msg.done(&m, msg.err)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		m.runs.cancelAll()
		return m, tea.Quit
	}
	m.clearNotice()
	if m.mode == modeBoard && m.blocksWrite(key) {
		return m, nil
	}

	switch m.mode {
	case modeNew, modeFilter:
		return m.handleInput(k)
	case modeEdit:
		return m.handleEdit(k)
	case modeAudit:
		return m.handleAuditKey(k)
	case modeActions:
		return m.handleActionsKey(k)
	case modeRuns:
		return m.handleRunsKey(k)
	case modeRunOutput:
		return m.handleRunOutputKey(k)
	case modeFleet:
		return m.handleFleetKey(k)
	case modeMoveTo:
		m.mode = modeBoard
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			if c := int(key[0] - '1'); c < len(m.board.Columns) {
				if !m.idle() {
					m.notice = m.busyNotice()
					return m, nil
				}
				return m, m.moveSelectionTo(c)
			}
		}
		return m, nil
	case modeQuick:
		m.mode = modeBoard
		if key != "esc" {
			m.quickFilter(key)
		}
		return m, nil
	case modeConfirmDelete:
		if key == "y" || key == "Y" {
			// The cards the question was about, not whatever is selected now.
			ts := m.deleteTargets
			m.mode = modeBoard
			if !m.idle() { // a list or write is running: the board would not match what is deleted
				m.notice = m.busyNotice()
				return m, nil
			}
			if len(ts) > 1 {
				return m, m.deleteMany(ts)
			}
			t := ts[0]
			be := m.be
			m.showLocal(t, true)
			cmd := m.write(func() error { return be.Delete(context.Background(), t.Backend, t.ID) }, func(m *Model, err error) {
				if err != nil {
					m.fail("error: " + err.Error())
					m.showLocal(t, false)
					return
				}
				m.pushUndo(undoOp{what: "delete of #" + t.ID, prev: t, recreate: true})
				if m.async == nil && m.reload() != nil {
					return
				}
				m.ok("deleted #" + t.ID + " (u undoes)")
			})
			return m, cmd
		}
		m.mode = modeBoard
		return m, nil
	case modeDetail:
		switch key {
		case "esc", "enter", "q", "?":
			m.mode = modeBoard
		case "e":
			m.startEdit()
		case "x":
			m.openActions()
		case ".":
			return m.repeatAction()
		case "R":
			m.openRuns()
		case "down", "j":
			m.detailY++
		case "up", "k":
			m.detailY--
		case "pgdown", " ":
			m.detailY += m.detailRoom()
		case "pgup":
			m.detailY -= m.detailRoom()
		case "home", "g":
			m.detailY = 0
		case "end", "G":
			m.detailY = 1 << 30
		}
		return m, nil
	case modeHelp:
		switch key {
		case "esc", "enter", "q", "?":
			m.mode = m.helpBack
			if m.mode == modeFilter || m.mode == modeAudit && m.auditEditing || m.mode == modeRuns && m.runEditing {
				m.input.Focus() // back in the prompt that asked for help
			}
		case "tab", "right", "l":
			m.helpPage = m.nextHelpPage(1)
		case "shift+tab", "left", "h":
			m.helpPage = m.nextHelpPage(-1)
		}
		return m, nil
	}

	switch key {
	case "q":
		m.runs.cancelAll()
		return m, tea.Quit
	case "left", "h":
		if m.col > 0 {
			m.col--
			m.clamp() // keep the vertical position where the column is long enough
		}
	case "right", "l":
		if m.col < len(m.board.Columns)-1 {
			m.col++
			m.clamp()
		}
	case "up", "k":
		if m.row > 0 {
			m.row--
		}
	case "down", "j":
		if m.row < len(m.column(m.col))-1 {
			m.row++
		}
	case "g", "home":
		m.row = 0
	case "G", "end":
		m.row = len(m.column(m.col)) - 1
		m.clamp()
	case "pgup":
		m.row -= m.pageSize(false)
		m.clamp()
	case "pgdown":
		m.row += m.pageSize(true)
		m.clamp()
	case "H", "shift+left":
		cmd := m.move(-1)
		return m, cmd
	case "L", "shift+right":
		cmd := m.move(+1)
		return m, cmd
	case "enter":
		if _, ok := m.selected(); ok {
			m.mode, m.detailY = modeDetail, 0
		}
	case "b":
		if n := len(m.be.Names()); n > 1 {
			m.target = (m.target + 1) % n
			m.notice = "new tasks go to backend " + m.be.Names()[m.target]
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if c := int(key[0] - '1'); c < len(m.board.Columns) {
			m.col = c
			m.clamp()
		}
	case "u":
		cmd := m.undoLast()
		return m, cmd
	case "f":
		if _, ok := m.selected(); ok {
			m.mode = modeQuick
		}
	case "v":
		m.cycleView()
	case "z":
		m.cycleGroup()
	case "s":
		m.cycleSort()
	case "S":
		m.reverseSort()
	case "n":
		m.input.Placeholder = "title of the new task"
		m.input.SetValue("")
		m.input.Focus()
		m.mode = modeNew
	case "/":
		m.input.Placeholder = "text  field:value  field=exact  field!=not  -word  (? = syntax)"
		m.input.SetValue(m.filter)
		m.input.Focus()
		m.histPos = len(m.history)
		m.mode = modeFilter
	case "e":
		m.startEdit()
	case "m":
		if _, ok := m.selected(); ok || len(m.marks) > 0 {
			m.mode = modeMoveTo
		}
	case "d":
		if ts := m.markedTasks(); len(ts) > 0 {
			m.deleteTargets, m.mode = ts, modeConfirmDelete
		} else if t, ok := m.selected(); ok {
			m.deleteTargets, m.mode = []task.Task{t}, modeConfirmDelete
		}
	case "r":
		if m.async != nil {
			if m.busy() {
				m.notice = "still loading: " + strings.Join(m.loadingNames(), ", ")
				return m, nil
			}
			if m.writing > 0 { // the list could still miss the write
				m.notice = "still saving"
				return m, nil
			}
			if err := m.be.Reload(); err != nil {
				m.fail("error: " + err.Error())
			}
			return m, m.startRefresh("manual", true)
		}
		if err := m.be.Reload(); err != nil {
			m.fail("error: " + err.Error())
		}
		m.refresh("manual")
		if m.notice == "" { // reload sets an error notice on failure
			m.notice = "reloaded"
		}
	case "esc":
		if m.filter != "" {
			m.filter, m.query, m.view = "", query.Query{}, ""
			m.clamp()
		} else if len(m.marks) > 0 {
			m.marks = nil
			m.notice = "marks cleared"
		}
	case " ":
		m.toggleMark()
	case ".":
		return m.repeatAction()
	case "A":
		m.openAudit()
	case "x":
		m.openActions()
	case "R":
		m.openRuns()
	case "F":
		m.openFleet()
	case "?":
		m.helpPage, m.helpBack, m.mode, m.helpDocs = 0, modeBoard, modeHelp, nil
	}
	return m, nil
}

func (m Model) handleInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "?" && m.mode == modeFilter && m.input.Value() == "" {
		m.helpPage, m.helpBack, m.mode, m.helpDocs = 1, modeFilter, modeHelp, nil
		return m, nil
	}
	switch k.String() {
	case "esc":
		m.input.Blur()
		m.mode = modeBoard
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		m.input.Blur()
		if m.mode == modeFilter {
			q, err := query.Parse(val)
			if err == nil {
				err = q.Check(m.fields)
			}
			if err != nil { // keep the prompt open and the previous filter
				m.notice = "filter: " + err.Error()
				m.input.Focus()
				return m, nil
			}
			m.filter, m.query, m.view = val, q, ""
			m.remember(val)
			m.row = 0
			m.clamp()
		} else if val != "" {
			m.mode = modeBoard
			if !m.idle() { // the new card must not meet a list that started before it
				m.notice = m.busyNotice()
				return m, nil
			}
			be, nt := m.be, task.Task{Title: val, Status: m.board.WriteValue(m.col), Backend: m.be.Names()[m.target]}
			var t task.Task
			cmd := m.write(func() (err error) { t, err = be.Create(context.Background(), nt); return }, func(m *Model, err error) {
				if err != nil {
					m.fail("error: " + err.Error())
					return
				}
				if m.async != nil {
					m.showLocal(t, false)
				} else if m.reload() != nil {
					return
				}
				m.ok("created #" + t.ID)
				if !m.selectID(t.Backend, t.ID) && m.filter != "" {
					m.notice += hiddenHint
				}
			})
			return m, cmd
		}
		m.mode = modeBoard
		return m, nil
	}
	if m.mode == modeFilter && (k.String() == "up" || k.String() == "down") {
		m.browseHistory(k.String() == "up")
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

// remember keeps applied filters (newest last, no repeats) for up/down recall.
func (m *Model) remember(f string) {
	if f == "" {
		return
	}
	for i, h := range m.history {
		if h == f {
			m.history = append(m.history[:i], m.history[i+1:]...)
			break
		}
	}
	m.history = append(m.history, f)
	if len(m.history) > 20 {
		m.history = m.history[1:]
	}
	m.histPos = len(m.history)
}

// browseHistory puts an earlier (up) or later (down) filter into the prompt.
func (m *Model) browseHistory(older bool) {
	if len(m.history) == 0 {
		return
	}
	if m.histPos > len(m.history) {
		m.histPos = len(m.history)
	}
	if older && m.histPos > 0 {
		m.histPos--
	} else if !older && m.histPos < len(m.history) {
		m.histPos++
	}
	val := ""
	if m.histPos < len(m.history) {
		val = m.history[m.histPos]
	}
	m.input.SetValue(val)
	m.input.CursorEnd()
}

// quickFilter filters the board by one property of the selected card:
// t type, a assignee, p priority, l first label.
func (m *Model) quickFilter(key string) {
	t, ok := m.selected()
	if !ok {
		return
	}
	var field, val, op string
	switch key {
	case "t":
		field, val, op = "type", t.Type, "="
	case "a":
		field, val, op = "assignee", t.Assignee, "="
	case "p":
		field, val, op = "priority", t.Priority, "="
	case "l":
		field, op = "labels", ":"
		if l := t.LabelList(); len(l) > 0 {
			val = l[0]
		}
	default:
		m.notice = "f then t (type) a (assignee) p (priority) l (label); esc cancels"
		return
	}
	if strings.TrimSpace(val) == "" {
		m.notice = fmt.Sprintf("this card has no %s to filter by", field)
		return
	}
	expr := field + op + quoteValue(strings.TrimSpace(val))
	q, err := query.Parse(expr)
	if err != nil {
		m.notice = "filter: " + err.Error()
		return
	}
	m.filter, m.query, m.view = expr, q, ""
	m.remember(expr)
	m.row = 0
	m.clamp()
	m.notice = "filter " + expr + " (esc clears)"
}

// quoteValue quotes a filter value when it has spaces, commas or quotes.
func quoteValue(v string) string {
	if !strings.ContainsAny(v, " \t,\"") {
		return v
	}
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

// cycleView applies the next saved view (board.views), then none again.
func (m *Model) cycleView() {
	views := m.spec.Views
	if len(views) == 0 {
		m.notice = "no saved views: add board.views to the config"
		return
	}
	next := 0 // index+1 of the view to apply; 0 = clear
	for i, v := range views {
		if v.Name == m.view {
			next = i + 2
		}
	}
	if next == 0 {
		next = 1
	}
	if next > len(views) {
		m.filter, m.query, m.view = "", query.Query{}, ""
		m.row = 0
		m.clamp()
		m.notice = "view cleared"
		return
	}
	v := views[next-1]
	q, err := query.Parse(v.Query)
	if err == nil {
		err = q.Check(m.fields)
	}
	if err != nil {
		m.notice = fmt.Sprintf("view %s: %v", v.Name, err)
		return
	}
	m.filter, m.query, m.view = v.Query, q, v.Name
	m.row = 0
	m.clamp()
	m.notice = fmt.Sprintf("view %s (%d/%d): %s", v.Name, next, len(views), v.Query)
}

// pageSize is how many cards pgup/pgdn jump: as many as fit in the visible
// column starting at the cursor, using the real height of each card (4-6
// lines), at least one.
func (m Model) pageSize(down bool) int {
	cards := m.column(m.col)
	if len(cards) == 0 {
		return 1
	}
	room, n := m.cardsHeight(), 0
	for i := m.row; i >= 0 && i < len(cards); {
		h := lipgloss.Height(renderCard(cards[i], m.colW(), false, m.marked(cards[i]), m.deps.Blockers(cards[i]), "", metaSty, false, m.runMarker(cards[i], m.colW()-6)))
		if n > 0 && h > room {
			break
		}
		room -= h
		n++
		if down {
			i++
		} else {
			i--
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

// move shifts the selected card to the neighbouring status column.
func (m *Model) move(delta int) tea.Cmd {
	target := m.col + delta
	if target < 0 || target >= len(m.board.Columns) {
		return nil
	}
	return m.moveTo(target)
}

// moveSelectionTo (`m`) moves the marked cards, else the selected one, to column target.
func (m *Model) moveSelectionTo(target int) tea.Cmd {
	if len(m.markedTasks()) == 0 {
		return m.moveTo(target)
	}
	var ts []task.Task
	for _, t := range m.markedTasks() {
		if m.board.ColumnOf(t.Status) != target {
			ts = append(ts, t)
		}
	}
	switch len(ts) {
	case 0:
		m.notice = "marked cards are already in " + m.board.Columns[target].Name
		return nil
	case 1:
		m.marks = nil
		m.selectID(ts[0].Backend, ts[0].ID)
		return m.moveTo(target)
	}
	return m.moveMany(ts, target)
}

// moveTo moves the selected card to column target.
func (m *Model) moveTo(target int) tea.Cmd {
	t, ok := m.selected()
	if !ok {
		return nil
	}
	if m.board.ColumnOf(t.Status) == target {
		m.notice = fmt.Sprintf("#%s is already in %s", t.ID, m.board.Columns[target].Name)
		return nil
	}
	before := t
	t.Status = m.board.WriteValue(target)
	ctx := backend.WithOld(backend.WithNote(context.Background(), "moved to "+m.board.Columns[target].Name), before)
	be := m.be
	hidden := false
	if m.async != nil { // the card moves now; the write confirms it
		m.showLocal(t, false)
		hidden = !m.selectID(t.Backend, t.ID) && m.filter != ""
	}
	return m.write(func() error { return be.Update(ctx, t) }, func(m *Model, err error) {
		if err != nil {
			m.fail("error: " + err.Error())
			m.showLocal(before, false)
			return
		}
		m.pushUndo(undoOp{what: "move of #" + t.ID, prev: before})
		if m.async == nil {
			if m.reload() != nil {
				return
			}
			hidden = !m.selectID(t.Backend, t.ID) && m.filter != ""
		}
		m.ok(fmt.Sprintf("moved #%s to %s", t.ID, m.board.Columns[target].Name))
		if lim, over := m.board.Over(target, m.columnTotal(target)); over {
			m.notice += fmt.Sprintf(" (over its limit of %d)", lim)
		}
		if hidden {
			m.notice += hiddenHint
		}
	})
}

// selectID moves the cursor to the card; false if it is not visible (hidden by
// the filter or the board's unmapped setting).
func (m *Model) selectID(backend, id string) bool {
	for c := range m.board.Columns {
		for r, t := range m.column(c) {
			if t.ID == id && t.Backend == backend {
				m.col, m.row = c, r
				return true
			}
		}
	}
	return false
}

const hiddenHint = " (hidden by the filter, esc clears it)"

// startEdit opens the edit form for the selected card.
func (m *Model) startEdit() {
	t, ok := m.selected()
	if !ok {
		return
	}
	m.edit = editForm{orig: t, inputs: make([]textinput.Model, len(editFields)), loaded: make([]string, len(editFields))}
	for i, f := range editFields {
		in := textinput.New()
		in.CharLimit = 0 // no cap: a limit would truncate long descriptions/notes on save
		in.Width = m.editWidth()
		in.SetValue(*f.ptr(&t))
		m.edit.inputs[i] = in
		m.edit.loaded[i] = in.Value()
	}
	m.edit.inputs[0].Focus()
	m.mode = modeEdit
}

func (m Model) handleEdit(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	e := &m.edit
	wasArmed := e.armed
	e.armed = false
	switch k.String() {
	case "esc":
		if !wasArmed && e.dirty() {
			e.armed = true
			m.notice = "unsaved changes: esc again to discard, enter to save"
			return m, nil
		}
		m.mode = modeBoard
		return m, nil
	case "tab", "down", "shift+tab", "up":
		e.inputs[e.idx].Blur()
		step := 1
		if s := k.String(); s == "shift+tab" || s == "up" {
			step = len(e.inputs) - 1
		}
		e.idx = (e.idx + step) % len(e.inputs)
		e.inputs[e.idx].Focus()
		return m, nil
	case "enter":
		cmd := m.saveEdit()
		return m, cmd
	}
	var cmd tea.Cmd
	e.inputs[e.idx], cmd = e.inputs[e.idx].Update(k)
	return m, cmd
}

// dirty reports whether any field differs from what was loaded.
func (e editForm) dirty() bool {
	for i := range e.inputs {
		if e.inputs[i].Value() != e.loaded[i] {
			return true
		}
	}
	return false
}

// editWidth is the width of each input: what is left after the 12-column label
// and the prompt, capped at 50 so the form stays compact on wide terminals.
func (m Model) editWidth() int {
	w := m.width - 12 - 3
	if w > 50 {
		w = 50
	}
	if w < 10 {
		w = 10
	}
	return w
}

// saveEdit validates the form and writes the card through the backend. On a
// validation or backend error the form stays open with a notice.
func (m *Model) saveEdit() tea.Cmd {
	t := m.edit.orig.Clone()
	for i, f := range editFields {
		// Untouched fields keep their stored value; the input flattens newlines.
		if v := m.edit.inputs[i].Value(); v != m.edit.loaded[i] {
			*f.ptr(&t) = strings.TrimSpace(v)
		}
	}
	if t.Title == "" {
		m.fail("title cannot be empty")
		return nil
	}
	for name, v := range map[string]string{"due": t.Due, "start": t.Start} {
		if _, err := time.Parse("2006-01-02", v); v != "" && err != nil {
			m.fail(fmt.Sprintf("%s: use YYYY-MM-DD", name))
			return nil
		}
	}
	ctx := backend.WithOld(backend.WithNote(context.Background(), "edited"), m.edit.orig)
	be, orig := m.be, m.edit.orig
	if m.async != nil { // the card changes now and the form closes; the write confirms it
		m.showLocal(t, false)
		m.selectID(t.Backend, t.ID)
		m.mode = modeBoard
	}
	return m.write(func() error { return be.Update(ctx, t) }, func(m *Model, err error) {
		if err != nil {
			m.fail("error: " + err.Error())
			if m.async != nil { // give the user their edit back
				m.showLocal(orig, false)
				if m.mode == modeBoard {
					m.mode = modeEdit
				}
			}
			return
		}
		m.pushUndo(undoOp{what: "edit of #" + t.ID, prev: orig})
		m.mode = modeBoard
		if m.async == nil {
			if m.reload() != nil {
				return
			}
			m.selectID(t.Backend, t.ID)
		}
		m.ok("saved #" + t.ID)
	})
}

// diffHooks fires task.appeared / task.disappeared for what changed since
// the last list, leaving out the first load, the Set's own writes (their
// events fired when they were made) and backends that failed to load.
func (m *Model) diffHooks(tasks []task.Task, perr *backend.PartialError) {
	own := m.be.TakeWritten()
	// A backend that did not load completely misses tasks in that list; the list
	// where it recovers would show all of them as new. So a backend is left out
	// of the comparison when it was incomplete in either of the two lists.
	skip := map[string]bool{}
	for n := range m.partial {
		skip[n] = true
	}
	m.partial = map[string]bool{}
	if perr != nil {
		for _, n := range perr.Names {
			skip[n], m.partial[n] = true, true
		}
	}
	first := !m.loaded
	m.loaded = true
	if !m.hooks.Active() {
		return
	}
	if !first {
		m.hooks.Diff(m.tasks, tasks, own, skip)
	}
	m.hooks.Tick(tasks, time.Now())
}

// WithHooks gives the model the runner that starts the configured hooks.
func (m Model) WithHooks(r *hook.Runner) Model {
	m.hooks = r
	return m
}

// nextHelpPage steps through the help pages. Outside the board the keys page
// does not apply, so only syntax and fields turn.
func (m Model) nextHelpPage(step int) int {
	first := 0
	if m.helpDocs != nil {
		first = 1
	}
	n := 3 - first
	return first + ((m.helpPage-first+step)%n+n)%n
}

// openFilterHelp shows the syntax and fields of another view's filter (the
// audit trail or the runs); esc returns to back, with its prompt open.
func (m *Model) openFilterHelp(back mode, title string, docs []query.FieldDoc, search []string) {
	m.helpPage, m.helpBack, m.mode = 1, back, modeHelp
	m.helpDocs, m.helpTitle, m.helpSearch = docs, title, search
}

// deleteMany deletes the cards one by one in one background write. The ones
// that fail come back on the board (and stay marked); the others are one undo.
func (m *Model) deleteMany(ts []task.Task) tea.Cmd {
	be := m.be
	m.showLocalMany(ts, true)
	errs := make([]error, len(ts))
	return m.write(func() error {
		for i, t := range ts {
			errs[i] = be.Delete(context.Background(), t.Backend, t.ID)
		}
		return nil
	}, func(m *Model, _ error) {
		var gone, failed []task.Task
		var firstErr error
		marks := map[cardKey]bool{}
		for i, t := range ts {
			if errs[i] != nil {
				failed = append(failed, t)
				marks[keyOf(t)] = true
				if firstErr == nil {
					firstErr = fmt.Errorf("#%s: %w", t.ID, errs[i])
				}
				continue
			}
			gone = append(gone, t)
		}
		if len(marks) == 0 {
			marks = nil
		}
		m.marks = marks
		if len(failed) > 0 {
			m.showLocalMany(failed, false)
		}
		if len(gone) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("delete of %d cards", len(gone)), prev: gone[0], recreate: true, group: gone})
			if m.async == nil && m.reload() != nil {
				return
			}
		}
		if len(failed) > 0 {
			m.fail(fmt.Sprintf("deleted %d of %d cards; %v", len(gone), len(ts), firstErr))
			return
		}
		m.ok(fmt.Sprintf("deleted %d cards (u undoes)", len(gone)))
	})
}

// recreateGroup is the undo of deleteMany: it creates the cards again.
func (m *Model) recreateGroup(ctx context.Context, op undoOp) tea.Cmd {
	be := m.be
	made := make([]task.Task, len(op.group))
	errs := make([]error, len(op.group))
	return m.write(func() error {
		for i, t := range op.group {
			made[i], errs[i] = be.Create(ctx, t)
		}
		return nil
	}, func(m *Model, _ error) {
		var failed []task.Task
		renamed := false
		for i, t := range op.group {
			if errs[i] != nil {
				failed = append(failed, t)
				continue
			}
			if made[i].ID != t.ID {
				renamed = true
			}
			if m.async != nil {
				m.showLocal(made[i], false)
			}
		}
		if m.async == nil && m.reload() != nil {
			return
		}
		if len(failed) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("delete of %d cards", len(failed)), prev: failed[0], recreate: true, group: failed})
			m.fail(fmt.Sprintf("undo restored %d of %d cards: %v", len(op.group)-len(failed), len(op.group), errs[slices.IndexFunc(errs, func(e error) bool { return e != nil })]))
			return
		}
		m.ok("undid " + op.what)
		if renamed {
			m.notice += " (some got new IDs)"
		}
	})
}

// moveMany moves the cards to column target in one background write. The ones
// that fail go back (and stay marked); the others are one undo.
func (m *Model) moveMany(ts []task.Task, target int) tea.Cmd {
	be := m.be
	name := m.board.Columns[target].Name
	moved := make([]task.Task, len(ts))
	for i, t := range ts {
		moved[i] = t
		moved[i].Status = m.board.WriteValue(target)
	}
	m.showLocalMany(moved, false)
	errs := make([]error, len(ts))
	return m.write(func() error {
		for i := range ts {
			ctx := backend.WithOld(backend.WithNote(context.Background(), "moved to "+name), ts[i])
			errs[i] = be.Update(ctx, moved[i])
		}
		return nil
	}, func(m *Model, _ error) {
		var done, failed []task.Task
		var firstErr error
		marks := map[cardKey]bool{}
		for i, t := range ts {
			if errs[i] != nil {
				failed = append(failed, t)
				marks[keyOf(t)] = true
				if firstErr == nil {
					firstErr = fmt.Errorf("#%s: %w", t.ID, errs[i])
				}
				continue
			}
			done = append(done, t)
		}
		if len(marks) == 0 {
			marks = nil
		}
		m.marks = marks
		if len(failed) > 0 {
			m.showLocalMany(failed, false)
		}
		if len(done) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("move of %d cards", len(done)), prev: done[0], group: done})
			if m.async == nil && m.reload() != nil {
				return
			}
		}
		if len(failed) > 0 {
			m.fail(fmt.Sprintf("moved %d of %d cards to %s; %v", len(done), len(ts), name, firstErr))
			return
		}
		m.ok(fmt.Sprintf("moved %d cards to %s (u undoes)", len(done), name))
		if lim, over := m.board.Over(target, m.columnTotal(target)); over {
			m.notice += fmt.Sprintf(" (over its limit of %d)", lim)
		}
	})
}

// restoreGroup is the undo of moveMany: it writes the earlier versions back.
func (m *Model) restoreGroup(ctx context.Context, op undoOp) tea.Cmd {
	be := m.be
	if m.async != nil {
		m.showLocalMany(op.group, false)
	}
	errs := make([]error, len(op.group))
	return m.write(func() error {
		for i, t := range op.group {
			errs[i] = be.Update(ctx, t)
		}
		return nil
	}, func(m *Model, _ error) {
		var failed []task.Task
		var firstErr error
		for i, t := range op.group {
			if errs[i] != nil {
				failed = append(failed, t)
				if firstErr == nil {
					firstErr = fmt.Errorf("#%s: %w", t.ID, errs[i])
				}
			}
		}
		if len(failed) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("move of %d cards", len(failed)), prev: failed[0], group: failed})
			if m.async == nil {
				m.reload()
			}
			m.fail(fmt.Sprintf("undo restored %d of %d cards; %v", len(op.group)-len(failed), len(op.group), firstErr))
			return
		}
		if m.async == nil && m.reload() != nil {
			return
		}
		m.ok("undid " + op.what)
	})
}
