package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/backend"
	"tack/internal/task"
)

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
		m.pushUndo(undoOp{what: "edit of #" + t.ID, prev: orig, after: t})
		m.mode = modeBoard
		if m.async == nil {
			if m.reload() != nil {
				return
			}
			m.selectID(t.Backend, t.ID)
		}
		msg := "saved #" + t.ID
		if len(t.Overlaid) > 0 {
			msg += " (" + task.OverlayMark + " " + strings.Join(t.Overlaid, ", ") + " kept locally)"
		}
		m.ok(msg)
	})
}
