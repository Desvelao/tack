package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/query"
	"tack/internal/task"
)

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
