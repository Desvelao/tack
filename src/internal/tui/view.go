package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"tack/internal/query"
	"tack/internal/task"
)

const colWidth = 30 // outer width of a board column

var (
	accent    = lipgloss.AdaptiveColor{Light: "27", Dark: "75"}
	dim       = lipgloss.AdaptiveColor{Light: "245", Dark: "243"}
	titleSty  = lipgloss.NewStyle().Bold(true)
	headSty   = lipgloss.NewStyle().Bold(true).Foreground(dim)
	headSelSt = lipgloss.NewStyle().Bold(true).Foreground(accent)
	cardSty   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(dim).Padding(0, 1)
	cardSel   = cardSty.Border(lipgloss.ThickBorder()).BorderForeground(accent) // thick: still visible without colour
	metaSty   = lipgloss.NewStyle().Foreground(dim)
	okSty     = lipgloss.NewStyle().Foreground(pair("28", "114", "2", "10"))
	failSty   = lipgloss.NewStyle().Bold(true).Foreground(pair("160", "203", "1", "9"))
	noteSty   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "215"})
	helpSty   = lipgloss.NewStyle().Foreground(dim)
	// One style per header tag, so "loading", "filter" and "marked" never look alike.
	loadSty    = lipgloss.NewStyle().Foreground(pair("30", "80", "6", "14"))
	filterSty  = lipgloss.NewStyle().Foreground(pair("136", "221", "3", "11"))
	sortSty    = lipgloss.NewStyle().Foreground(pair("25", "111", "4", "12"))
	groupSty   = lipgloss.NewStyle().Foreground(pair("97", "140", "7", "7"))
	labelSty   = lipgloss.NewStyle().Foreground(pair("30", "73", "6", "6"))
	backendSty = lipgloss.NewStyle().Italic(true).Foreground(dim)
	blockedSty = lipgloss.NewStyle().Foreground(pair("166", "209", "1", "9"))
	keySty     = lipgloss.NewStyle().Foreground(accent)
	markSty    = lipgloss.NewStyle().Foreground(pair("127", "176", "5", "13"))
	idSty      = lipgloss.NewStyle().Bold(true).Foreground(pair("172", "214", "3", "11")) // the #ID before a title
	// agentColor marks the cards assigned to one of board.agents: their border and the bold @name.
	agentColor = lipgloss.AdaptiveColor{Light: "91", Dark: "177"}
)

// labelChip shows the first two labels, e.g. "[ui,api+1]"; "" when there are none.
func labelChip(t task.Task) string {
	l := t.LabelList()
	switch {
	case len(l) == 0:
		return ""
	case len(l) <= 2:
		return "[" + strings.Join(l, ",") + "]"
	}
	return fmt.Sprintf("[%s,%s+%d]", l[0], l[1], len(l)-2)
}

// View is the screen of the current mode. While background runs ended since R
// was last opened, every screen says so: the board in its title line, the other
// views at the start of their last line.
func (m Model) View() string {
	out := m.screen()
	if m.unseen == 0 || m.boardScreen() {
		return out
	}
	i := strings.LastIndex(out, "\n") + 1
	last := m.finishedTag() + out[i:]
	if m.width > 0 {
		last = ansi.Truncate(last, m.width, "…")
	}
	return out[:i] + last
}

// finishedTag is "N finished (R) · " while background runs ended unseen, else "".
func (m Model) finishedTag() string {
	if m.unseen == 0 {
		return ""
	}
	return okSty.Render(fmt.Sprintf("%d finished (R)", m.unseen)) + metaSty.Render(" · ")
}

// boardScreen reports whether the board is drawn (alone or under a prompt).
func (m Model) boardScreen() bool {
	switch m.mode {
	case modeBoard, modeNew, modeFilter, modeQuick, modeConfirmDelete, modeConfirmRelease, modeMoveTo:
		return true
	}
	return false
}

func (m Model) screen() string {
	switch m.mode {
	case modeDetail:
		return m.detailView()
	case modeHelp:
		return m.helpView()
	case modeEdit:
		return m.editView()
	case modeAudit:
		return m.auditView()
	case modeActions:
		return m.actionsView()
	case modeRuns:
		return m.runsView()
	case modeRunOutput:
		return m.runOutputView()
	case modeWorkers:
		return m.workersView()
	}
	var b strings.Builder
	count := fmt.Sprintf("%d tasks", len(m.tasks))
	if m.busy() && !m.loaded && len(m.tasks) == 0 {
		count = "… tasks"
	}
	if m.filter != "" { // show how many the filter lets through
		shown := 0
		for c := range m.board.Columns {
			shown += len(m.column(c))
		}
		count = fmt.Sprintf("%d/%d tasks", shown, len(m.tasks))
	}
	var h strings.Builder
	h.WriteString(titleSty.Render("tack") + metaSty.Render(fmt.Sprintf("  %s · %s", m.name, count)))
	if tag := m.loadingTag(); tag != "" {
		h.WriteString(loadSty.Render("  " + tag))
	}
	if tag := m.failedTag(); tag != "" && !m.busy() {
		h.WriteString(failSty.Render("  " + tag))
	}
	if len(m.cycles) > 0 { // these cards can never become ready
		h.WriteString(failSty.Render("  blocked_by cycle: " + m.cycles[0].String()))
		if n := len(m.cycles) - 1; n > 0 {
			h.WriteString(failSty.Render(fmt.Sprintf(" (+%d)", n)))
		}
	}
	if n := len(m.marks); n > 0 {
		h.WriteString(markSty.Render(fmt.Sprintf("  %d marked", n)))
	}
	if m.unseen > 0 {
		h.WriteString(okSty.Render(fmt.Sprintf("  %d finished (R)", m.unseen)))
	}
	if keys := m.activeSort(); len(keys) > 0 {
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k.Field
			if k.Desc {
				parts[i] += "↓"
			} else {
				parts[i] += "↑"
			}
		}
		h.WriteString(sortSty.Render("  sort: " + strings.Join(parts, " ")))
	}
	if m.groupBy != "" {
		h.WriteString(groupSty.Render("  group: " + m.groupBy))
	}
	if m.filter != "" {
		if m.view != "" {
			h.WriteString(filterSty.Render("  view " + m.view + ": " + m.filter))
		} else {
			h.WriteString(filterSty.Render("  filter: " + m.filter))
		}
	}
	if n := len(m.board.Columns); n > m.visibleCols() {
		h.WriteString(metaSty.Render(fmt.Sprintf("  column %d/%d", m.col+1, n)))
	}
	title := h.String()
	if m.width > 0 {
		title = ansi.Truncate(title, m.width-1, "…") // many tags must not wrap the header
	}
	b.WriteString(title)
	b.WriteString("\n")
	if len(m.board.Columns) > m.visibleCols() {
		b.WriteString(m.columnStrip()) // replaces the blank line: columns are off-screen
	}
	b.WriteString("\n")
	b.WriteString(m.boardView())
	b.WriteString("\n")
	if strip := m.runsStrip(); len(strip) > 0 {
		b.WriteString(strings.Join(strip, "\n") + "\n")
	}
	switch m.mode {
	case modeNew:
		where := m.board.Columns[m.col].Name
		if names := m.be.Names(); len(names) > 1 {
			where += " @" + names[m.target]
		}
		b.WriteString(headSelSt.Render("new task in "+where+"> ") + m.input.View())
	case modeFilter:
		b.WriteString(filterSty.Render("filter> ") + m.input.View() + "\n")
		if m.notice != "" {
			b.WriteString(m.renderNotice())
		} else {
			b.WriteString(helpSty.Render("enter apply · empty clears · esc cancel · ? on an empty prompt: syntax help"))
		}
	case modeQuick:
		b.WriteString(filterSty.Render("filter by this card's: (t)ype  (a)ssignee  (p)riority  (l)abel   esc cancel"))
	case modeMoveTo:
		b.WriteString(filterSty.Render(m.moveToPrompt()))
	case modeConfirmDelete:
		if n := len(m.deleteTargets); n > 1 {
			b.WriteString(failSty.Render(fmt.Sprintf("delete the %d marked cards? (y/N)", n)))
			break
		}
		t := m.deleteTargets[0]
		b.WriteString(failSty.Render(fmt.Sprintf("delete %s:%s %q? (y/N)", t.Backend, t.ID, t.Title)))
	case modeConfirmRelease:
		t := m.relTarget
		b.WriteString(failSty.Render(fmt.Sprintf("take %s:%s from %s? (y/N)", t.Backend, t.ID, t.Assignee)))
	default:
		if m.notice != "" {
			b.WriteString(m.renderNotice())
		} else {
			b.WriteString(boardHelp(m.width - 1))
		}
	}
	return b.String()
}

// columnStrip is one line naming every column with its card count and the
// number key that jumps to it; the focused column is highlighted. When it would
// not fit the width, columns other than the focused one shrink to "N:count".
func (m Model) columnStrip() string {
	n := len(m.board.Columns)
	full := make([]string, n)
	short := make([]string, n)
	plain := 0
	for c, col := range m.board.Columns {
		cnt := len(m.column(c))
		full[c] = fmt.Sprintf("%d %s %d", c+1, col.Name, cnt)
		short[c] = fmt.Sprintf("%d:%d", c+1, cnt)
		if c == m.col {
			short[c] = full[c]
		}
		plain += len([]rune(full[c])) + 3
	}
	items := full
	if m.width > 0 && plain > m.width {
		items = short
	}
	out := make([]string, n)
	for c, it := range items {
		if c == m.col {
			out[c] = headSelSt.Render(it)
		} else {
			out[c] = metaSty.Render(it)
		}
	}
	line := strings.Join(out, metaSty.Render(" · "))
	if m.width > 0 {
		line = lipgloss.NewStyle().MaxWidth(m.width).Render(line)
	}
	return line
}

// soonDays is how close a due date must be to be flagged amber.
const soonDays = 3

type dueLevel int

const (
	dueNormal dueLevel = iota
	dueSoon
	dueOverdue
)

// dueLevelOf classifies a card's due date: overdue, within soonDays, or
// neither. Cards in finished columns (Done, Closed...) are never flagged.
func (m Model) dueLevelOf(t task.Task, c int) dueLevel {
	if m.board.Columns[c].Done {
		return dueNormal
	}
	now := m.clock()
	if t.Overdue(now) {
		return dueOverdue
	}
	if due, ok := t.DueTime(); ok {
		y, mo, d := now.Date()
		today := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
		if due.Sub(today) <= soonDays*24*time.Hour {
			return dueSoon
		}
	}
	return dueNormal
}

// dueStyle colours a card's due date: red when overdue, amber when soon.
func (m Model) dueStyle(t task.Task, c int) lipgloss.Style {
	switch m.dueLevelOf(t, c) {
	case dueOverdue:
		return overdueSty
	case dueSoon:
		return soonSty
	}
	return metaSty
}

// renderNotice draws the notice by kind: "OK" in green after a successful
// write, "FAIL" in bold red after a failed one (the words carry the meaning
// without colour), anything else in the plain note colour. One line, cut with an
// ellipsis to the terminal width.
func (m Model) renderNotice() string {
	tag, text := "", m.notice
	switch m.noticeKind {
	case noticeOK:
		tag = "OK    "
	case noticeFail:
		tag, text = "FAIL  ", strings.TrimPrefix(text, "error: ")
	}
	if m.width > 0 && ansi.StringWidth(tag+text) > m.width { // cells, not runes: wide characters take two
		if n := ansi.StringWidth(tag); m.width <= n {
			tag, text = ansi.Truncate(tag, m.width, "…"), ""
		} else {
			text = ansi.Truncate(text, m.width-n, "…")
		}
	}
	switch m.noticeKind {
	case noticeOK:
		return okSty.Bold(true).Render(tag) + text
	case noticeFail:
		return failSty.Render(tag + text)
	}
	return noteSty.Render(text)
}

// clock is the current time; tests replace Model.now.
func (m Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// colW is the outer width of a board column on this terminal.
func (m Model) colW() int {
	if m.width > 0 && m.width < colWidth {
		return m.width
	}
	return colWidth
}

// visibleCols is how many columns fit side by side.
func (m Model) visibleCols() int {
	if v := m.width / m.colW(); v > 1 {
		return v
	}
	return 1
}

// cardsHeight is the number of lines available for cards in a column: the
// screen minus title + blank, column header, both "more" markers and the footer
// (two lines in filter mode).
func (m Model) cardsHeight() int {
	footer := 1
	if m.mode == modeFilter {
		footer = 2
	}
	h := m.height - 5 - footer - len(m.runsStrip())
	if h < 4 {
		h = 4
	}
	return h
}

func (m Model) boardView() string {
	cw := m.colW()
	visible := m.visibleCols()
	first := (m.col / visible) * visible
	last := first + visible
	if last > len(m.board.Columns) {
		last = len(m.board.Columns)
	}
	cardsH := m.cardsHeight()

	cols := make([]string, 0, visible)
	for c := first; c < last; c++ {
		cols = append(cols, m.columnView(c, cw, cardsH))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cols...)
}

func (m Model) columnView(c, cw, cardsH int) string {
	tasks := m.column(c)
	sel := c == m.col
	headStyle := headSty
	if sel {
		headStyle = headSelSt
	}
	if col := m.board.Columns[c].Color; col != "" { // configured colour; the focused column is underlined
		headStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(col)).Underline(sel)
	}
	title := fmt.Sprintf("%s (%d)", strings.ToUpper(m.board.Columns[c].Name), len(tasks))
	if m.filter != "" {
		title = fmt.Sprintf("%s (%d/%d)", strings.ToUpper(m.board.Columns[c].Name), len(tasks), m.columnTotal(c))
	}
	if lim := m.board.Columns[c].Limit; lim > 0 { // WIP limit: show the real column size against it
		n := m.columnTotal(c)
		title = fmt.Sprintf("%s (%d/%d)", strings.ToUpper(m.board.Columns[c].Name), n, lim)
		if _, over := m.board.Over(c, n); over {
			headStyle = failSty
			title += " !"
		}
	}
	if sel { // a text cue too: colour and underline are lost under NO_COLOR
		title = "▸ " + title
	}
	head := headStyle.Render(title)

	// Cards are drawn on demand: a column can hold hundreds and only the ones in
	// the window are needed (drawing them all made every redraw take ~100 ms).
	drawn := map[int]string{}
	card := func(i int) string {
		if s, ok := drawn[i]; ok {
			return s
		}
		t := tasks[i]
		s := renderCard(t, cw, sel && i == m.row, m.marked(t), m.deps.Blockers(t), m.board.Columns[c].Color, m.dueStyle(t, c),
			m.isAgent(t.Assignee), m.runMarker(t, cw-6))
		drawn[i] = s
		return s
	}

	// Group sub-headers ("-- bug (3) --") sit above the first card of each group
	// and above the first visible card, so a scrolled column still names its group.
	groups := make([]string, len(tasks))
	counts := map[string]int{}
	if m.groupBy != "" {
		for i, t := range tasks {
			groups[i] = m.groupLabel(t, m.board.Columns[c].Name)
			counts[groups[i]]++
		}
	}
	hdr := func(i, top int) int { // lines of sub-header drawn above card i
		if m.groupBy != "" && (i == top || groups[i] != groups[i-1]) {
			return 1
		}
		return 0
	}

	// Scroll so the selected card is visible in the focused column.
	top := 0
	if sel && m.row < len(tasks) {
		// The first card from which everything down to the selection fits, found
		// walking up from the selection so the cards above the window are not drawn.
		// The first card always has a sub-header (hdr), the others only at a group change.
		h := lipgloss.Height(card(m.row))
		for top = m.row; top > 0; top-- {
			next := h + lipgloss.Height(card(top-1))
			if m.groupBy != "" && groups[top] != groups[top-1] {
				next++ // the sub-header above card top
			}
			if next+hdr(top-1, top-1) > cardsH {
				break
			}
			h = next
		}
	}
	var out []string
	out = append(out, head)
	if len(tasks) == 0 && c == m.col && m.busy() && !m.loaded {
		out = append(out, metaSty.Render("loading…"))
	}
	used := 0
	if top > 0 {
		out = append(out, metaSty.Render(fmt.Sprintf("↑ %d more", top)))
	}
	for i := top; i < len(tasks); i++ {
		hh := hdr(i, top)
		h := lipgloss.Height(card(i)) + hh
		// Always draw the first card, even if taller than the budget, so the
		// selection never disappears on very small terminals.
		if used+h > cardsH && i > top {
			out = append(out, metaSty.Render(fmt.Sprintf("↓ %d more", len(tasks)-i)))
			break
		}
		if hh > 0 {
			out = append(out, groupHeadStyle(m.groupBy, groups[i]).Render(fmt.Sprintf("-- %s (%d) --", groups[i], counts[groups[i]])))
		}
		out = append(out, card(i))
		used += h
	}
	return lipgloss.NewStyle().Width(cw).Render(strings.Join(out, "\n"))
}

// groupHeadStyle colours a group sub-header: by the type colour when grouping by
// type, else bold dim, so it reads differently from the "↑ N more" markers.
func groupHeadStyle(groupBy, label string) lipgloss.Style {
	if groupBy == "type" {
		return lipgloss.NewStyle().Bold(true).Foreground(typeColor(label))
	}
	return headSty
}

// markSign is drawn before the title of a card marked with space.
const markSign = "✓ "

// blockers are the open ids of t's blocked_by (board.Deps); agent says the
// assignee is one of board.agents; runLine is runMarker's line ("" = none).
func renderCard(t task.Task, cw int, selected, marked bool, blockers []string, color string, due lipgloss.Style, agent bool, runLine string) string {
	style := cardSty
	if agent {
		style = style.BorderForeground(agentColor)
	}
	if selected {
		style = cardSel
		if color != "" {
			style = style.BorderForeground(lipgloss.Color(color))
		}
	}
	title := idSty.Render("#"+t.ID) + " " + t.Title
	if marked {
		title = markSty.Bold(true).Render(markSign) + title
	}
	body := lipgloss.NewStyle().MaxHeight(3).Render(title)
	body += "\n" + cardMeta(t, cw-6, due, agent)
	// On its own line: in the meta line it would be the first part dropped
	// when backend and priority fill it.
	if len(blockers) > 0 {
		body += "\n" + blockedSty.MaxWidth(cw-6).Render("blocked by #"+strings.Join(blockers, ",#"))
	}
	if runLine != "" {
		body += "\n" + runLine
	}
	// Width counts padding but not the border.
	return style.Width(cw - 2).Render(body)
}

// cardMeta is the card's second line: the owning backend, then priority, the
// type and so on. width is the room available inside the card; agent shows the
// assignee in bold agentColor.
func cardMeta(t task.Task, width int, dueStyle lipgloss.Style, agent bool) string {
	type part struct {
		kind, text string
		style      lipgloss.Style
	}
	var parts []part
	if t.Backend != "" {
		name := []rune(t.Backend)
		if max := width / 2; max > 1 && len(name) > max {
			name = append(name[:max-1], '…')
		}
		parts = append(parts, part{"backend", string(name), backendSty})
	}
	if t.Priority != "" {
		col := priorityColorOf(t.Priority)
		parts = append(parts, part{"priority", t.Marked("priority", t.Priority), lipgloss.NewStyle().Foreground(col)})
	}
	if t.Type != "" {
		parts = append(parts, part{"type", t.Marked("type", t.Type), lipgloss.NewStyle().Foreground(typeColor(t.Type))})
	}
	if t.Assignee != "" {
		sty := metaSty
		if agent {
			sty = lipgloss.NewStyle().Bold(true).Foreground(agentColor)
		}
		parts = append(parts, part{"assignee", t.Marked("assignee", "@"+t.Assignee), sty})
	}
	if t.Due != "" {
		parts = append(parts, part{"due", t.Marked("due", "due "+t.Due), dueStyle})
	}
	if chip := labelChip(t); chip != "" {
		parts = append(parts, part{"labels", t.Marked("labels", chip), labelSty})
	}
	render := func() string {
		out := make([]string, len(parts))
		for i, p := range parts {
			out[i] = p.style.Render(p.text)
		}
		return strings.Join(out, metaSty.Render(" · "))
	}
	// The line must stay on one row (a wrapped meta line makes cards taller than
	// the layout budgets for): drop the least important parts first.
	for _, drop := range []string{"labels", "due", "type", "assignee"} {
		if lipgloss.Width(render()) <= width {
			break
		}
		for i, p := range parts {
			if p.kind == drop {
				parts = append(parts[:i], parts[i+1:]...)
				break
			}
		}
	}
	// What is left (backend, priority) is cut off at the card width.
	return lipgloss.NewStyle().MaxWidth(width).Render(render())
}

// detailMeta is the line under the title, coloured like the card: backend,
// status, priority, type, assignee, dates and labels.
func (m Model) detailMeta(t task.Task) string {
	type part struct {
		text string
		sty  lipgloss.Style
	}
	assignee := metaSty
	if m.isAgent(t.Assignee) {
		assignee = lipgloss.NewStyle().Bold(true).Foreground(agentColor)
	}
	due := metaSty
	if c := m.board.Columns; m.col < len(c) {
		due = m.dueStyle(t, m.col)
	}
	var parts []string
	for _, p := range []part{
		{t.Backend, backendSty}, {t.Marked("status", t.Status), metaSty},
		{t.Marked("priority", t.Priority), lipgloss.NewStyle().Foreground(priorityColorOf(t.Priority))},
		{t.Marked("type", t.Type), lipgloss.NewStyle().Foreground(typeColor(t.Type))},
		{t.Marked("assignee", atName(t.Assignee)), assignee}, {t.Marked("due", dateLabel("due", t.Due)), due}, {t.Marked("start", dateLabel("start", t.Start)), metaSty},
		{t.Marked("labels", t.Labels), labelSty},
	} {
		if p.text != "" {
			parts = append(parts, p.sty.Render(p.text))
		}
	}
	return strings.Join(parts, metaSty.Render(" · "))
}

func (m Model) detailView() string {
	t, ok := m.selected()
	if !ok {
		return ""
	}
	w := m.width - 6
	if w < 20 {
		w = 20
	}
	wrap := lipgloss.NewStyle().Width(w)
	var b strings.Builder
	b.WriteString(idSty.Render("#"+t.ID) + "  " + titleSty.Render(t.Title) + "\n")
	b.WriteString(m.detailMeta(t))
	if m.isAgent(t.Assignee) {
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(agentColor).Render("  agent"))
	}
	if len(t.Overlaid) > 0 {
		b.WriteString("\n" + metaSty.Render(task.OverlayMark+" local overlay (kept in a local file, not in "+t.Backend+")"))
	}
	b.WriteString("\n\n")
	if going, ended := m.cardRuns(t); len(going)+len(ended) > 0 {
		for _, r := range going {
			b.WriteString(loadSty.Render("⟳ running "+r.job.Spec.Name) + metaSty.Render(" "+formatDur(m.clock().Sub(r.started))) + "\n")
		}
		for _, r := range ended {
			b.WriteString(runResult(r) + "\n")
		}
		b.WriteString("\n")
	}
	if ids := t.BlockedByList(); len(ids) > 0 {
		line := "blocked by #" + strings.Join(ids, ", #")
		if open := m.deps.Blockers(t); len(open) > 0 {
			if c, ok := m.deps.CycleOf(t); ok {
				line += "  circular: " + c.String()
			}
			b.WriteString(blockedSty.Render(line+"  (open: #"+strings.Join(open, ", #")+")") + "\n\n")
		} else {
			b.WriteString(okSty.Render(line+"  (all finished: ready)") + "\n\n")
		}
	}
	if t.Description != "" {
		b.WriteString(wrap.Render(t.Description) + "\n\n")
	}
	if t.Notes != "" {
		b.WriteString(headSty.Render(t.Marked("notes", "Notes")) + "\n" + wrap.Render(t.Notes) + "\n\n")
	}
	keys := make([]string, 0, len(t.Extra))
	for k, v := range t.Extra {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(metaSty.Render(k+": ") + t.Extra[k] + "\n")
	}
	// Scroll long cards: only detailRoom() lines fit between the border and the footer.
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	room := m.detailRoom()
	foot := "e edit · x actions · . repeat · R runs · esc/enter back"
	if len(lines) > room {
		y := m.detailY
		if y > len(lines)-room {
			y = len(lines) - room
		}
		if y < 0 {
			y = 0
		}
		lines = lines[y : y+room]
		foot += fmt.Sprintf(" · ↑↓/pgup/pgdn scroll (%d-%d of %d)", y+1, y+room, len(strings.Split(strings.TrimRight(b.String(), "\n"), "\n")))
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(strings.Join(lines, "\n"))
	return box + "\n" + helpSty.Render(foot)
}

// detailRoom is how many content lines the detail box can show: the screen
// minus the box border (2) and the footer line.
func (m Model) detailRoom() int {
	if r := m.height - 3; r > 3 {
		return r
	}
	return 3
}

func nonEmpty(s ...string) []string {
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (m Model) helpView() string {
	docs, title, search := m.helpDocs, m.helpTitle, m.helpSearch
	pages, page := 3, m.helpPage+1
	if docs == nil { // the board's own filter
		docs, title, search = query.TaskFieldDocs(m.fields), "Filter fields", m.spec.Search
	} else {
		pages, page = 2, m.helpPage
	}
	foot := helpSty.Render("tab/←→ other page · esc back")
	switch m.helpPage {
	case 1:
		return titleSty.Render(fmt.Sprintf("tack help %d/%d", page, pages)) + "\n\n" + query.Help(search) + "\n" + foot
	case 2:
		return titleSty.Render(fmt.Sprintf("tack help %d/%d", page, pages)) + "\n\n" + query.FieldsHelp(title, docs, search) + "\n" + foot
	}
	return keysHelp()
}

func keysHelp() string {
	rows := [][2]string{
		{"← → / h l", "previous / next column"},
		{"1-9", "jump to column N (a strip under the title lists them when they don't all fit)"},
		{"↑ ↓ / k j", "previous / next card"},
		{"g G / home end", "first / last card of the column"},
		{"pgup pgdn", "jump a screen of cards"},
		{"H L / shift+← →", "move card to previous / next status"},
		{"enter", "open card details"},
		{"e", "edit the card (tab next field, enter save, esc cancel)"},
		{"n / b", "new task in the current column / cycle the backend new tasks go to"},
		{"x / .", "run an action on the card (or marked cards) / repeat it"},
		{"space", "mark / unmark the card (esc with no filter unmarks all)"},
		{"m", "move the card, or the marked cards, to column 1-9"},
		{"d", "delete card, or the marked cards (asks to confirm)"},
		{"u", "undo the last move, edit or delete (a deleted card gets a new ID)"},
		{"c / C", "claim the card for you ($TACK_AGENT or $USER) / release its claim (asks first for another's)"},
		{"/", "filter: text or field:value (esc clears; up/down recall earlier filters)"},
		{"f", "filter by the selected card: then t type, a assignee, p priority, l label"},
		{"v", "cycle the saved views (board.views), then none"},
		{"z", "group cards by priority / type / assignee / labels (cycle)"},
		{"s / S", "sort cards: next field / reverse (display only)"},
		{"r / A / R", "reload from the backend / audit trail / background action runs"},
		{"W", "workers: the agents, the task each holds, since when, lease left"},
		{"?", "this help (tab: filter syntax, then what each field means)"},
		{"q / ctrl+c", "quit"},
	}
	var b strings.Builder
	b.WriteString(titleSty.Render("tack help 1/3: keys") + "\n\n")
	for _, r := range rows {
		b.WriteString("  " + keySty.Render(fmt.Sprintf("%-18s", r[0])) + " " + r[1] + "\n")
	}
	b.WriteString("\n" + helpSty.Render("tab/←→ filter syntax and fields · esc back"))
	return b.String()
}

func atName(s string) string {
	if s == "" {
		return ""
	}
	return "@" + s
}

func dateLabel(label, d string) string {
	if d == "" {
		return ""
	}
	return label + " " + d
}

func (m Model) editView() string {
	var b strings.Builder
	b.WriteString(titleSty.Render(fmt.Sprintf("edit #%s", m.edit.orig.ID)) + "\n\n")
	for i, f := range editFields {
		label := fmt.Sprintf("%-12s", f.name)
		if i == m.edit.idx {
			label = headSelSt.Render(label)
		} else {
			label = headSty.Render(label)
		}
		b.WriteString(label + m.edit.inputs[i].View() + "\n")
	}
	b.WriteString("\n")
	if m.notice != "" {
		b.WriteString(m.renderNotice())
	} else {
		b.WriteString(helpSty.Render("tab/↓ next · shift+tab/↑ prev · enter save · esc cancel · dates: YYYY-MM-DD"))
	}
	return b.String()
}

// boardHelp is the board's key hint, cut to fit one line of width cells: the
// least important entries go first (the full list is behind ?), the way out
// and the help key stay.
func boardHelp(width int) string {
	items := []string{"←→ column", "↑↓ card", "H/L move", "enter open", "e edit", "n new", "d delete", "m move to", "x actions", ". repeat", "space mark", "c claim", "C release", "R runs", "/ filter", "r reload", "? help", "q quit"}
	drop := []string{"c claim", "C release", "space mark", ". repeat", "r reload", "R runs", "m move to", "d delete", "H/L move", "n new", "e edit", "x actions", "/ filter", "↑↓ card", "←→ column", "enter open"}
	for ansi.StringWidth(strings.Join(items, "  ")) > width && len(drop) > 0 {
		for i, it := range items {
			if it == drop[0] {
				items = append(items[:i], items[i+1:]...)
				break
			}
		}
		drop = drop[1:]
	}
	line := ansi.Truncate(strings.Join(items, "  "), max(width, 1), "…")
	// Style after the cut, so the width maths above stays on plain text.
	parts := strings.Split(line, "  ")
	for i, p := range parts {
		key, rest, _ := strings.Cut(p, " ")
		parts[i] = keySty.Render(key) + helpSty.Render(" "+rest)
		if rest == "" {
			parts[i] = keySty.Render(key)
		}
	}
	return strings.Join(parts, helpSty.Render("  "))
}

// moveToPrompt is the footer while `m` waits for a column number.
func (m Model) moveToPrompt() string {
	p := "move to:"
	if n := len(m.marks); n > 0 {
		p = fmt.Sprintf("move %d marked cards to:", n)
	}
	for i, c := range m.board.Columns {
		if i == 9 {
			break
		}
		p += fmt.Sprintf("  %d %s", i+1, c.Name)
	}
	return ansi.Truncate(p+"   esc cancel", m.width-1, "…")
}
