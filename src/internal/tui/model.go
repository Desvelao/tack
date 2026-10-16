// Package tui renders a backend.Backend as a kanban board with Bubble Tea.
package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tkan/internal/audit"
	"tkan/internal/backend"
	"tkan/internal/board"
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
	modeQuick // `f`: waiting for the field to filter by
	modeAudit // `A`: the audit trail
)

const pollEvery = time.Second

// undoOp reverses one change: restore the task as it was (move, edit), or
// create it again (delete, which may give it a new ID).
type undoOp struct {
	what     string
	prev     task.Task
	recreate bool
}

const maxUndo = 30

func (m *Model) pushUndo(op undoOp) {
	m.undo = append(m.undo, op)
	if len(m.undo) > maxUndo {
		m.undo = m.undo[1:]
	}
}

// undoLast reverses the most recent move, edit or delete.
func (m *Model) undoLast() {
	if len(m.undo) == 0 {
		m.notice = "nothing to undo"
		return
	}
	op := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	ctx := backend.WithNote(context.Background(), "undo of "+op.what)
	if op.recreate {
		t, err := m.be.Create(ctx, op.prev)
		if err != nil {
			m.notice = "undo failed: " + err.Error()
			return
		}
		m.reload()
		m.selectID(t.Backend, t.ID)
		m.notice = "undid " + op.what
		if t.ID != op.prev.ID {
			m.notice += fmt.Sprintf(" (restored as #%s, the old #%s is gone)", t.ID, op.prev.ID)
		}
		return
	}
	if err := m.be.Update(ctx, op.prev); err != nil {
		m.notice = "undo failed: " + err.Error()
		return
	}
	m.reload()
	if !m.selectID(op.prev.Backend, op.prev.ID) && m.filter != "" {
		m.notice = "undid " + op.what + hiddenHint
		return
	}
	m.notice = "undid " + op.what
}

type tickMsg time.Time

// Model is the Bubble Tea model. Backend calls are synchronous: fine for
// local files; an API backend should cache inside its implementation.
type Model struct {
	be       *backend.Set
	name     string
	target   int             // index of the backend new tasks are created in
	sortKeys []board.SortKey // session sort set with s/S; nil = use spec.Sort
	sortIdx  int             // position of the s-cycle in m.fields (0 = configured sort)
	tasks    []task.Task
	spec     board.Spec
	board    board.Board
	col, row int
	mode     mode
	input    textinput.Model
	filter   string      // text of the active filter ("" = none)
	query    query.Query // parsed filter
	fields   []string    // field names usable in a filter (for validation and help)
	helpPage int         // 0 = keys, 1 = filter syntax
	helpBack mode        // mode to return to when the help closes
	width    int
	height   int
	notice   string
	edit     editForm
	detailY  int              // first visible line of the detail view
	groupBy  string           // field the cards of a column are grouped by ("" = none)
	now      func() time.Time // clock for due-date flags; nil = time.Now
	undo     []undoOp         // newest last; u pops one
	history  []string         // filters applied this session, newest last (up/down in the / prompt)
	histPos  int              // position while browsing history; len(history) = not browsing
	view     string           // name of the saved view behind the current filter ("" = none)

	audit        *audit.Log  // nil = no audit view
	auditQ       query.Query // filter of the audit view
	auditFilter  string
	auditY       int    // first visible line of the audit view
	auditEditing bool   // the audit filter prompt is open
	reloadErr    string // last reload failure recorded, to log a repeating one once
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
	{"notes", func(t *task.Task) *string { return &t.Notes }},
}

// New builds a model for the backends in be; name is shown in the header and spec defines the columns.
func New(be *backend.Set, name string, spec board.Spec) Model {
	in := textinput.New()
	in.CharLimit = 200
	m := Model{be: be, name: name, spec: spec, input: in, width: 80, height: 24, groupBy: query.Normalize(spec.Group)}
	m.reload()
	return m
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
		m.be.Audit(audit.OpReload, "", "", nil, fmt.Sprintf("%s: %d tasks", reason, len(m.tasks)))
		return
	}
	if err.Error() != m.reloadErr {
		m.reloadErr = err.Error()
		m.be.Audit(audit.OpReload, "", "", err, reason)
	}
}

func (m *Model) reload() error {
	tasks, err := m.be.List(context.Background())
	var perr *backend.PartialError
	loadErr := err
	switch {
	case errors.As(err, &perr): // show what loaded, say what did not
		m.notice = "error: " + err.Error()
	case err != nil:
		m.notice = "error: " + err.Error()
		return err
	}
	b, err := board.New(m.spec, m.be.Statuses())
	if err != nil {
		m.notice = "error: " + err.Error()
		return err
	}
	prev, hadSel := m.selected()
	m.tasks = tasks
	m.board = b
	m.fields = query.KnownFields(tasks)
	m.clamp()
	// Keep the cursor on the same card when the data changed underneath it.
	if hadSel {
		m.selectID(prev.Backend, prev.ID)
	}
	return loadErr
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
		if !m.query.Empty() && !m.query.Match(query.TaskLookup(t, colName), m.spec.Search) {
			continue
		}
		out = append(out, t)
	}
	if keys := m.sortFor(c); len(keys) > 0 {
		sort.SliceStable(out, func(i, j int) bool {
			return query.CompareBy(query.TaskLookup(out[i], colName), query.TaskLookup(out[j], colName), keys) < 0
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
	v, _ := query.TaskLookup(t, colName)(m.groupBy)
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

func (m Model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(pollEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		for i := range m.edit.inputs {
			m.edit.inputs[i].Width = m.editWidth()
		}
	case tickMsg:
		if m.mode == modeBoard {
			changed, err := m.be.Changed()
			if err != nil {
				if m.notice == "" {
					m.notice = "error: " + err.Error()
				}
				m.auditReload("auto: checking for changes", err)
			}
			if changed {
				m.refresh("auto: file changed")
			}
		}
		return m, tick()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	m.notice = ""

	switch m.mode {
	case modeNew, modeFilter:
		return m.handleInput(k)
	case modeEdit:
		return m.handleEdit(k)
	case modeAudit:
		return m.handleAuditKey(k)
	case modeQuick:
		m.mode = modeBoard
		if key != "esc" {
			m.quickFilter(key)
		}
		return m, nil
	case modeConfirmDelete:
		if key == "y" || key == "Y" {
			if t, ok := m.selected(); ok {
				if err := m.be.Delete(context.Background(), t.Backend, t.ID); err != nil {
					m.notice = "error: " + err.Error()
				} else {
					m.notice = "deleted #" + t.ID + " (u undoes)"
					m.pushUndo(undoOp{what: "delete of #" + t.ID, prev: t, recreate: true})
				}
				m.reload()
			}
		}
		m.mode = modeBoard
		return m, nil
	case modeDetail:
		switch key {
		case "esc", "enter", "q", "?":
			m.mode = modeBoard
		case "e":
			m.startEdit()
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
			if m.mode == modeFilter {
				m.input.Focus()
			}
		case "tab", "shift+tab", "left", "right", "h", "l":
			m.helpPage = 1 - m.helpPage
		}
		return m, nil
	}

	switch key {
	case "q":
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
		m.move(-1)
	case "L", "shift+right":
		m.move(+1)
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
		m.undoLast()
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
	case "d":
		if _, ok := m.selected(); ok {
			m.mode = modeConfirmDelete
		}
	case "r":
		if err := m.be.Reload(); err != nil {
			m.notice = "error: " + err.Error()
		}
		m.refresh("manual")
		if m.notice == "" { // reload sets an error notice on failure
			m.notice = "reloaded"
		}
	case "esc":
		if m.filter != "" {
			m.filter, m.query, m.view = "", query.Query{}, ""
			m.clamp()
		}
	case "A":
		m.openAudit()
	case "?":
		m.helpPage, m.helpBack, m.mode = 0, modeBoard, modeHelp
	}
	return m, nil
}

func (m Model) handleInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "?" && m.mode == modeFilter && m.input.Value() == "" {
		m.helpPage, m.helpBack, m.mode = 1, modeFilter, modeHelp
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
			t, err := m.be.Create(context.Background(), task.Task{Title: val, Status: m.board.WriteValue(m.col), Backend: m.be.Names()[m.target]})
			if err != nil {
				m.notice = "error: " + err.Error()
			} else {
				m.notice = "created #" + t.ID
				m.reload()
				if !m.selectID(t.Backend, t.ID) && m.filter != "" {
					m.notice += hiddenHint
				}
			}
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
		h := lipgloss.Height(renderCard(cards[i], m.colW(), false, "", metaSty))
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
func (m *Model) move(delta int) {
	t, ok := m.selected()
	target := m.col + delta
	if !ok || target < 0 || target >= len(m.board.Columns) {
		return
	}
	before := t
	t.Status = m.board.WriteValue(target)
	ctx := backend.WithNote(context.Background(), "moved to "+m.board.Columns[target].Name)
	if err := m.be.Update(ctx, t); err != nil {
		m.notice = "error: " + err.Error()
		return
	}
	m.pushUndo(undoOp{what: "move of #" + t.ID, prev: before})
	m.reload()
	m.notice = fmt.Sprintf("moved #%s to %s", t.ID, m.board.Columns[target].Name)
	if lim, over := m.board.Over(target, m.columnTotal(target)); over {
		m.notice += fmt.Sprintf(" (over its limit of %d)", lim)
	}
	if !m.selectID(t.Backend, t.ID) && m.filter != "" {
		m.notice += hiddenHint
	}
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
		m.saveEdit()
		return m, nil
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
func (m *Model) saveEdit() {
	t := m.edit.orig.Clone()
	for i, f := range editFields {
		// Untouched fields keep their stored value; the input flattens newlines.
		if v := m.edit.inputs[i].Value(); v != m.edit.loaded[i] {
			*f.ptr(&t) = strings.TrimSpace(v)
		}
	}
	if t.Title == "" {
		m.notice = "title cannot be empty"
		return
	}
	for name, v := range map[string]string{"due": t.Due, "start": t.Start} {
		if _, err := time.Parse("2006-01-02", v); v != "" && err != nil {
			m.notice = fmt.Sprintf("%s: use YYYY-MM-DD", name)
			return
		}
	}
	if err := m.be.Update(backend.WithNote(context.Background(), "edited"), t); err != nil {
		m.notice = "error: " + err.Error()
		return
	}
	m.pushUndo(undoOp{what: "edit of #" + t.ID, prev: m.edit.orig})
	m.mode = modeBoard
	m.reload()
	m.selectID(t.Backend, t.ID)
	m.notice = "saved #" + t.ID
}
