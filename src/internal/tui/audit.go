package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tack/internal/audit"
)

// openAudit switches to the audit trail (newest entries first).
func (m *Model) openAudit() {
	if m.audit == nil {
		m.notice = "audit trail is not available"
		return
	}
	m.mode, m.auditY, m.auditEditing = modeAudit, 0, false
}

func (m Model) handleAuditKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if m.auditEditing {
		if key == "?" && m.input.Value() == "" {
			m.openFilterHelp(modeAudit, "Audit fields", audit.FieldDocs, audit.SearchFields)
			return m, nil
		}
		switch key {
		case "esc":
			m.auditEditing = false
			m.input.Blur()
		case "enter":
			val := strings.TrimSpace(m.input.Value())
			q, err := audit.Parse(val)
			if err != nil { // keep the prompt open and the previous filter
				m.notice = "filter: " + err.Error()
				return m, nil
			}
			m.auditQ, m.auditFilter, m.auditY = q, val, 0
			m.auditEditing = false
			m.input.Blur()
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(k)
			return m, cmd
		}
		return m, nil
	}
	switch key {
	case "esc":
		if m.auditFilter != "" {
			m.auditFilter, m.auditY = "", 0
			m.auditQ, _ = audit.Parse("")
		} else {
			m.mode = modeBoard
		}
	case "q", "A":
		m.mode = modeBoard
	case "/":
		m.auditEditing = true
		m.input.SetValue(m.auditFilter)
		m.input.CursorEnd()
		m.input.Prompt = "/"
		m.input.Focus()
	case "down", "j":
		m.auditY++
	case "up", "k":
		m.auditY--
	case "pgdown", " ":
		m.auditY += m.auditRoom()
	case "pgup":
		m.auditY -= m.auditRoom()
	case "home", "g":
		m.auditY = 0
	case "end", "G":
		m.auditY = 1 << 30
	}
	return m, nil
}

// auditRoom is how many lines fit between the header and the footer.
func (m Model) auditRoom() int {
	if r := m.height - 3; r > 3 {
		return r
	}
	return 3
}

// auditCache keeps the lines of the audit view until the trail, the filter or
// the width changes: sorting and rendering a trail that grows without bound on
// every keypress would be too much for a small machine. A pointer, because the
// model is copied on every update.
type auditCache struct {
	ok     bool
	gen    uint64
	filter string
	width  int
	lines  []string
	shown  int
	total  int
}

func (m Model) auditView() string {
	entries, gen, err := m.audit.EntriesVersion()
	c := m.auditC
	if c == nil {
		c = &auditCache{}
	}
	if !c.ok || c.gen != gen || c.filter != m.auditFilter || c.width != m.width {
		c.lines, c.shown, c.total = m.auditLines(entries)
		c.ok, c.gen, c.filter, c.width = true, gen, m.auditFilter, m.width
	}
	lines := c.lines

	where := "memory only (set audit.file to keep it)"
	if p := m.audit.Path(); p != "" {
		where = p
	}
	head := titleSty.Render("tack audit") + metaSty.Render(fmt.Sprintf("  %d/%d entries · %s", c.shown, c.total, where))
	if m.auditFilter != "" {
		head += filterSty.Render("  filter: " + m.auditFilter)
	}

	room := m.auditRoom()
	foot := "↑↓/pgup/pgdn scroll · / filter · esc back"
	if m.auditFilter != "" {
		foot = "↑↓/pgup/pgdn scroll · / filter · esc clear filter"
	}
	if len(lines) > room {
		y := m.auditY
		if y > len(lines)-room {
			y = len(lines) - room
		}
		if y < 0 {
			y = 0
		}
		foot += fmt.Sprintf(" (%d-%d of %d)", y+1, y+room, len(lines))
		lines = lines[y : y+room]
	}
	bottom := helpSty.Render(foot)
	switch {
	case m.auditEditing:
		bottom = m.input.View() + "  " + helpSty.Render("? fields and syntax · enter apply · esc cancel")
	case m.notice != "":
		bottom = m.renderNotice()
	case err != nil:
		bottom = failSty.Render("error: " + err.Error())
	}
	return lipgloss.JoinVertical(lipgloss.Left, head, "", strings.Join(lines, "\n"), bottom)
}

// auditLines renders the entries that pass the filter, newest first, grouped by
// session; shown and total count the entries with and without the filter.
func (m Model) auditLines(entries []audit.Entry) (lines []string, shown, total int) {
	sess := m.audit.SessionsOf(entries)
	total = len(entries)
	entries = sess.Filter(entries, m.auditQ)
	opW := 6 // wide enough for the longest op shown (hook:EVENT ops are long)
	for _, e := range entries {
		opW = max(opW, min(len(e.Op), 28))
	}
	last := ""
	for i := len(entries) - 1; i >= 0; i-- { // newest first
		e := entries[i]
		if e.Session != last {
			if last != "" {
				lines = append(lines, "")
			}
			lines = append(lines, headSty.Render(sess.Label(e.Session)))
			last = e.Session
		}
		target := e.Backend
		if e.ID != "" {
			target += " #" + e.ID
		}
		line := fmt.Sprintf("%s  %-*s %-6s %-14s %s", e.Time.Local().Format("01-02 15:04:05"), opW, e.Op, e.Result, trim(target, 14), e.Details)
		line = trim(line, m.width-1)
		if e.Result == audit.ResultError {
			line = failSty.Render(line)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = []string{metaSty.Render("no entries")}
	}
	return lines, len(entries), total
}

// trim cuts s to w terminal cells (at least one).
func trim(s string, w int) string { return cut(s, max(w, 1)) }
