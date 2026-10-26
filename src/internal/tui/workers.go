package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"tack/internal/workers"
)

// animEvery is the tick of the workers view: a busy agent's avatar changes
// frame at this pace (and the elapsed times move).
const animEvery = 500 * time.Millisecond

// leaseSoon is how little lease time left is drawn as a warning.
const leaseSoon = 5 * time.Minute

var (
	busySty    = okSty.Bold(true)
	expiredSty = failSty
)

// openWorkers switches to the workers view (`W`).
func (m *Model) openWorkers() {
	m.mode, m.workersIdx = modeWorkers, 0
}

// workersRows is the claims on the cards now (agents and people), busiest first.
func (m Model) workersRows() []workers.Row {
	return workers.Build(m.tasks, m.spec.Agents, m.clock())
}

func (m Model) handleWorkersKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.workersRows()
	switch k.String() {
	case "esc", "q", "W":
		m.mode = modeBoard
	case "up", "k":
		m.workersIdx--
	case "down", "j":
		m.workersIdx++
	case "pgup":
		m.workersIdx -= 5
	case "pgdown":
		m.workersIdx += 5
	case "home", "g":
		m.workersIdx = 0
	case "end", "G":
		m.workersIdx = len(rows) - 1
	case "r":
		m.reloadNow("manual reload")
	case "C":
		i := min(max(m.workersIdx, 0), len(rows)-1)
		if i >= 0 && rows[i].Task != nil {
			t := *rows[i].Task
			if m.selectID(t.Backend, t.ID) {
				cmd := m.releaseCard(t)
				return m, cmd
			}
			m.notice = "#" + t.ID + " is hidden by the filter (esc on the board clears it)"
		}
	case "enter":
		i := min(max(m.workersIdx, 0), len(rows)-1)
		if i >= 0 && rows[i].Task != nil {
			t := rows[i].Task
			if !m.selectID(t.Backend, t.ID) {
				m.notice = "#" + t.ID + " is hidden by the filter (esc on the board clears it)"
			}
			m.mode = modeBoard
		}
	}
	m.workersIdx = min(max(m.workersIdx, 0), max(len(rows)-1, 0))
	return m, nil
}

// workersView draws one line per agent and task: avatar, agent, state, the task,
// when the claim started, how long it has run and how much lease is left.
func (m Model) workersView() string {
	now := m.clock()
	rows := m.workersRows()
	busy := 0
	for _, r := range rows {
		if r.State == workers.Busy {
			busy++
		}
	}
	var b strings.Builder
	b.WriteString(titleSty.Render("tack workers") + metaSty.Render(fmt.Sprintf("  %d busy · %d rows · %s", busy, len(rows), m.name)) + "\n\n")

	const nameW, stateW, tailW = 10, 8, 28 // tail: since 11 + elapsed 7 + left 8, with the blanks
	taskW := max(m.width-(3+nameW+1+stateW+1)-tailW-3, 8)
	frame := int(now.UnixMilli() / int64(animEvery/time.Millisecond))
	var lines []string
	for _, r := range rows {
		task, since, elapsed, left := "-", "", "", ""
		if r.Task != nil {
			task = "#" + r.Task.ID + " " + r.Task.Title
			if len(m.be.Names()) > 1 {
				task = r.Task.Backend + ":" + task[1:]
			}
			if !r.Since.IsZero() {
				since, elapsed = r.Since.Local().Format("01-02 15:04"), workers.Duration(r.Elapsed(now))
			}
			switch {
			case r.State == workers.Expired:
				left = "expired"
			case !r.Until.IsZero():
				left = workers.Duration(r.Left(now)) + " left"
			}
		}
		avatar := workers.Avatar(r, m.spec.Avatars, frame)
		avatar += strings.Repeat(" ", max(2-ansi.StringWidth(avatar), 0))
		state := string(r.State)
		switch r.State {
		case workers.Busy:
			state = busySty.Render(fmt.Sprintf("%-*s", stateW, state))
		case workers.Expired:
			state = expiredSty.Render(fmt.Sprintf("%-*s", stateW, state))
		default:
			state = metaSty.Render(fmt.Sprintf("%-*s", stateW, state))
		}
		name := padCut(agentLabel(r), nameW)
		nameSty := lipgloss.NewStyle().Foreground(agentColor)
		if r.State == workers.Busy {
			nameSty = nameSty.Bold(true)
		}
		name = nameSty.Render(name)
		leftSty := metaSty
		switch {
		case r.State == workers.Expired:
			leftSty = overdueSty
		case !r.Until.IsZero() && r.Left(now) < leaseSoon:
			leftSty = soonSty
		}
		line := fmt.Sprintf("%s %s %s %s %s%s", avatar, name, state, padCut(task, taskW), metaSty.Render(fmt.Sprintf("%-11s %-7s ", since, elapsed)), leftSty.Render(left))
		lines = append(lines, ansi.Truncate(line, max(m.width-2, 1), "…"))
	}
	if len(lines) == 0 {
		lines = []string{metaSty.Render("  nobody holds a task; claim: true on an action or `tack claim` puts agents here")}
	}
	idx := min(max(m.workersIdx, 0), max(len(lines)-1, 0))
	room := m.listRoom(2 + 2)
	if m.notice != "" {
		room--
	}
	start, end := window(len(lines), idx, room)
	for i := start; i < end; i++ {
		if len(rows) > 0 && i == idx {
			b.WriteString(headSelSt.Render(">") + " " + lines[i] + "\n")
		} else {
			b.WriteString("  " + lines[i] + "\n")
		}
	}
	b.WriteString("\n" + helpSty.Render("↑↓ choose · enter card · C release · r reload · esc back"))
	if m.notice != "" {
		b.WriteString("\n" + m.renderNotice())
	}
	return b.String()
}

// padCut cuts s to w cells and pads it with spaces to exactly w.
func padCut(s string, w int) string {
	s = cut(s, w)
	return s + strings.Repeat(" ", max(w-ansi.StringWidth(s), 0))
}

// agentLabel is the agent's name, marked when the claim lives in a local overlay.
func agentLabel(r workers.Row) string {
	if r.Task != nil {
		return r.Task.Marked("assignee", r.Agent)
	}
	return r.Agent
}
