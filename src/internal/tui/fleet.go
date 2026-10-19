package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"tkan/internal/fleet"
)

// animEvery is the tick of the fleet view: a busy agent's avatar changes
// frame at this pace (and the elapsed times move).
const animEvery = 500 * time.Millisecond

// leaseSoon is how little lease time left is drawn as a warning.
const leaseSoon = 5 * time.Minute

var (
	busySty    = okSty.Bold(true)
	expiredSty = failSty
)

// openFleet switches to the fleet view (`F`).
func (m *Model) openFleet() {
	m.mode, m.fleetIdx = modeFleet, 0
}

// fleetRows is the claims on the cards now (agents and people), busiest first.
func (m Model) fleetRows() []fleet.Row {
	return fleet.Build(m.tasks, m.spec.Agents, m.clock())
}

func (m Model) handleFleetKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.fleetRows()
	switch k.String() {
	case "esc", "q", "F":
		m.mode = modeBoard
	case "up", "k":
		m.fleetIdx--
	case "down", "j":
		m.fleetIdx++
	case "pgup":
		m.fleetIdx -= 5
	case "pgdown":
		m.fleetIdx += 5
	case "home", "g":
		m.fleetIdx = 0
	case "end", "G":
		m.fleetIdx = len(rows) - 1
	case "r":
		m.reloadNow("manual reload")
	case "enter":
		i := min(max(m.fleetIdx, 0), len(rows)-1)
		if i >= 0 && rows[i].Task != nil {
			t := rows[i].Task
			if !m.selectID(t.Backend, t.ID) {
				m.notice = "#" + t.ID + " is hidden by the filter (esc on the board clears it)"
			}
			m.mode = modeBoard
		}
	}
	m.fleetIdx = min(max(m.fleetIdx, 0), max(len(rows)-1, 0))
	return m, nil
}

// fleetView draws one line per agent and task: avatar, agent, state, the task,
// when the claim started, how long it has run and how much lease is left.
func (m Model) fleetView() string {
	now := m.clock()
	rows := m.fleetRows()
	busy := 0
	for _, r := range rows {
		if r.State == fleet.Busy {
			busy++
		}
	}
	var b strings.Builder
	b.WriteString(titleSty.Render("tkan fleet") + metaSty.Render(fmt.Sprintf("  %d busy · %d rows · %s", busy, len(rows), m.name)) + "\n\n")

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
				since, elapsed = r.Since.Local().Format("01-02 15:04"), fleet.Duration(r.Elapsed(now))
			}
			switch {
			case r.State == fleet.Expired:
				left = "expired"
			case !r.Until.IsZero():
				left = fleet.Duration(r.Left(now)) + " left"
			}
		}
		avatar := fleet.Avatar(r, m.spec.Avatars, frame)
		avatar += strings.Repeat(" ", max(2-ansi.StringWidth(avatar), 0))
		state := string(r.State)
		switch r.State {
		case fleet.Busy:
			state = busySty.Render(fmt.Sprintf("%-*s", stateW, state))
		case fleet.Expired:
			state = expiredSty.Render(fmt.Sprintf("%-*s", stateW, state))
		default:
			state = metaSty.Render(fmt.Sprintf("%-*s", stateW, state))
		}
		name := padCut(r.Agent, nameW)
		nameSty := lipgloss.NewStyle().Foreground(agentColor)
		if r.State == fleet.Busy {
			nameSty = nameSty.Bold(true)
		}
		name = nameSty.Render(name)
		leftSty := metaSty
		switch {
		case r.State == fleet.Expired:
			leftSty = overdueSty
		case !r.Until.IsZero() && r.Left(now) < leaseSoon:
			leftSty = soonSty
		}
		line := fmt.Sprintf("%s %s %s %s %s%s", avatar, name, state, padCut(task, taskW), metaSty.Render(fmt.Sprintf("%-11s %-7s ", since, elapsed)), leftSty.Render(left))
		lines = append(lines, ansi.Truncate(line, max(m.width-2, 1), "…"))
	}
	if len(lines) == 0 {
		lines = []string{metaSty.Render("  nobody holds a task; claim: true on an action or `tkan claim` puts agents here")}
	}
	idx := min(max(m.fleetIdx, 0), max(len(lines)-1, 0))
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
	b.WriteString("\n" + helpSty.Render("↑↓ choose · enter go to the card · r reload · esc back"))
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
