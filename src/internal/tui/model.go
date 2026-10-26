// Package tui renders a backend.Backend as a kanban board with Bubble Tea.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/hook"
	"tack/internal/query"
	"tack/internal/task"
)

type mode int

const (
	modeBoard mode = iota
	modeDetail
	modeNew
	modeConfirmDelete
	modeConfirmRelease // `C` on a claim somebody else holds: asks before taking it away
	modeFilter
	modeHelp
	modeEdit
	modeQuick     // `f`: waiting for the field to filter by
	modeAudit     // `A`: the audit trail
	modeActions   // `x`: the action picker
	modeRuns      // `R`: the background runs
	modeRunOutput // output of one run
	modeWorkers   // `W`: agents and the tasks they hold
	modeMoveTo    // `m`: waiting for the column number to move the card(s) to
)

const pollEvery = time.Second

// liveEvery is the tick while a running action's output is on screen.
const liveEvery = 250 * time.Millisecond

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
	deps       board.Deps    // which cards are blocked (blocked_by), from the last load
	cycles     []board.Cycle // blocked_by cycles among the open cards, from the last load
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
	auditC       *auditCache
	auditY       int    // first visible line of the audit view
	auditEditing bool   // the audit filter prompt is open
	reloadErr    string // last reload failure recorded, to log a repeating one once

	allActions []action.Spec    // configured actions, of every backend (action.For picks a card's)
	actions    []action.Spec    // the `x` picker's list, for the cards it was opened on
	actionDir  string           // directory their relative workdir resolves against
	actionIdx  int              // highlighted action in the picker
	actionTTY  bool             // `i` in the picker: run the next action with the terminal
	actionBack mode             // mode the picker returns to (board or detail)
	lastAction action.Spec      // the action started last (`.` repeats it; Name "" = none yet)
	lastTTY    bool             // it ran with the terminal because of `i`
	actionWD   string           // `w` in the picker: workdir for the next action ("" = the configured one)
	actionWDIn *textinput.Model // the `w` prompt while it is open
	lastWD     string           // the workdir the last action ran in (`.` repeats it)
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

	workersIdx int // highlighted row of the workers view

	alog *actionlog.Log // optional detailed log of action runs (nil = off)

	pending       []func(*Model) // work an action run left for when the user leaves a form (see finishAction)
	relTarget     task.Task      // the card the open release confirmation is about
	deleteTargets []task.Task    // the cards the open delete confirmation is about (the marked ones, else the selected)

	hooks       *hook.Runner  // optional event hooks (nil = none)
	watch       *hook.Watcher // turns the lists into hook events (nil without hooks)
	loaded      bool          // a first load happened (its tasks are not "appeared")
	async       *asyncLoad    // background listing (NewAsync); nil = every load is synchronous
	writing     int           // backend writes running in the background (interactive board only)
	quitWarned  bool          // ctrl+c was held back by a pending write; the next one quits anyway
	wantRefresh string        // why a background list is waiting for the board to be idle ("" = none)
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
	return Model{be: be, name: name, spec: spec, input: in, width: 80, height: 24, groupBy: query.Normalize(spec.Group), auditC: &auditCache{}}
}

// WithAudit gives the model the audit trail shown by `A`.
func (m Model) WithAudit(l *audit.Log) Model {
	m.audit, m.auditC = l, &auditCache{}
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
		m.watch.Reload(nil)
		m.be.Audit(audit.OpReload, "", "", nil, fmt.Sprintf("%s: %d tasks", reason, len(m.tasks)))
		return
	}
	if err.Error() != m.reloadErr {
		m.reloadErr = err.Error()
		m.be.Audit(audit.OpReload, "", "", err, reason)
		m.watch.Reload(err)
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
	m.cycles = m.deps.Cycles()
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
	case modeWorkers:
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
		if m.mode == modeBoard || m.mode == modeWorkers {
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
		m.quitWarned = false
		msg.done(&m, msg.err)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		if m.writing > 0 && !m.quitWarned { // quitting now would drop the write
			m.quitWarned = true
			m.notice = "still saving, ctrl+c again to quit"
			return m, nil
		}
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
	case modeWorkers:
		return m.handleWorkersKey(k)
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
	case modeConfirmRelease:
		return m.handleConfirmRelease(key)
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
		if m.writing > 0 { // quitting now would drop the write
			m.notice = m.busyNotice()
			return m, nil
		}
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
	case "c":
		cmd := m.claimSelected()
		return m, cmd
	case "C":
		cmd := m.releaseSelected()
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
	case "W":
		m.openWorkers()
	case "?":
		m.helpPage, m.helpBack, m.mode, m.helpDocs = 0, modeBoard, modeHelp, nil
	}
	return m, nil
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
	// A view whose query does not parse, or names a field this backlog lacks, is
	// skipped (and said so) instead of stopping the cycle on it.
	var skipped []string
	for ; next <= len(views); next++ {
		v := views[next-1]
		q, err := query.Parse(v.Query)
		if err == nil {
			err = q.Check(m.fields)
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("view %s: %v", v.Name, err))
			continue
		}
		m.filter, m.query, m.view = v.Query, q, v.Name
		m.row = 0
		m.clamp()
		m.notice = fmt.Sprintf("view %s (%d/%d): %s", v.Name, next, len(views), v.Query)
		if len(skipped) > 0 {
			m.notice += " (skipped " + strings.Join(skipped, "; ") + ")"
		}
		return
	}
	m.filter, m.query, m.view = "", query.Query{}, ""
	m.row = 0
	m.clamp()
	m.notice = "view cleared"
	if len(skipped) > 0 {
		m.notice += " (skipped " + strings.Join(skipped, "; ") + ")"
	}
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
		m.pushUndo(undoOp{what: "move of #" + t.ID, prev: before, after: t})
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

// diffHooks fires task.appeared / task.disappeared for what changed since
// the last list, leaving out the first load, the Set's own writes (their
// events fired when they were made) and backends that failed to load.
func (m *Model) diffHooks(tasks []task.Task, perr *backend.PartialError) {
	own := m.be.TakeWritten()
	m.loaded = true
	m.watch.Observe(tasks, perr, own)
}

// WithHooks gives the model the runner that starts the configured hooks.
func (m Model) WithHooks(r *hook.Runner) Model {
	m.hooks = r
	m.watch = hook.NewWatcher(r, "tui")
	return m
}
