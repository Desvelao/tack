package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"tkan/internal/board"
	"tkan/internal/query"
	"tkan/internal/task"
)

const colWidth = 30 // outer width of a board column

var (
	accent    = lipgloss.AdaptiveColor{Light: "27", Dark: "75"}
	dim       = lipgloss.AdaptiveColor{Light: "245", Dark: "243"}
	titleSty  = lipgloss.NewStyle().Bold(true)
	headSty   = lipgloss.NewStyle().Bold(true).Foreground(dim)
	headSelSt = lipgloss.NewStyle().Bold(true).Foreground(accent)
	cardSty   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(dim).Padding(0, 1)
	cardSel   = cardSty.BorderForeground(accent)
	metaSty   = lipgloss.NewStyle().Foreground(dim)
	okSty     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "114"})
	failSty   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"})
	noteSty   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "215"})
	helpSty   = lipgloss.NewStyle().Foreground(dim)
)

// priorityColorOf colours the priority word on a card from its rank (see
// task.PriorityRank), so every spelling of a level looks alike; unknown values
// stay grey.
func priorityColorOf(p string) lipgloss.Color {
	switch r := task.PriorityRank(p); {
	case r == 0:
		return "196"
	case r == 1:
		return "203"
	case r == 2:
		return "221"
	case r <= 4:
		return "114"
	}
	return "245"
}

// typeColors is the palette task types are coloured from.
var typeColors = []lipgloss.Color{"141", "80", "180", "174", "110", "150"}

// typeColor gives every type a stable colour (hash of its lower-case name), so
// "bug" looks the same on every card and in every run.
func typeColor(typ string) lipgloss.Color {
	h := 0
	for _, r := range strings.ToLower(strings.TrimSpace(typ)) {
		h = (h*31 + int(r)) % 9973
	}
	return typeColors[h%len(typeColors)]
}

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

func (m Model) View() string {
	switch m.mode {
	case modeDetail:
		return m.detailView()
	case modeHelp:
		return m.helpView()
	case modeEdit:
		return m.editView()
	case modeAudit:
		return m.auditView()
	}
	var b strings.Builder
	count := fmt.Sprintf("%d tasks", len(m.tasks))
	if m.filter != "" { // show how many the filter lets through
		shown := 0
		for c := range m.board.Columns {
			shown += len(m.column(c))
		}
		count = fmt.Sprintf("%d/%d tasks", shown, len(m.tasks))
	}
	b.WriteString(titleSty.Render("tkan") + metaSty.Render(fmt.Sprintf("  %s · %s", m.name, count)))
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
		b.WriteString(metaSty.Render("  sort: " + strings.Join(parts, " ")))
	}
	if m.groupBy != "" {
		b.WriteString(metaSty.Render("  group: " + m.groupBy))
	}
	if m.filter != "" {
		if m.view != "" {
			b.WriteString(noteSty.Render("  view " + m.view + ": " + m.filter))
		} else {
			b.WriteString(noteSty.Render("  filter: " + m.filter))
		}
	}
	if n := len(m.board.Columns); n > m.visibleCols() {
		b.WriteString(metaSty.Render(fmt.Sprintf("  column %d/%d", m.col+1, n)))
	}
	b.WriteString("\n")
	if len(m.board.Columns) > m.visibleCols() {
		b.WriteString(m.columnStrip()) // replaces the blank line: columns are off-screen
	}
	b.WriteString("\n")
	b.WriteString(m.boardView())
	b.WriteString("\n")
	switch m.mode {
	case modeNew:
		where := m.board.Columns[m.col].Name
		if names := m.be.Names(); len(names) > 1 {
			where += " @" + names[m.target]
		}
		b.WriteString("new task in " + where + "> " + m.input.View())
	case modeFilter:
		b.WriteString("filter> " + m.input.View() + "\n")
		if m.notice != "" {
			b.WriteString(m.renderNotice())
		} else {
			b.WriteString(helpSty.Render("enter apply · empty clears · esc cancel · ? on an empty prompt: syntax help"))
		}
	case modeQuick:
		b.WriteString(noteSty.Render("filter by this card's: (t)ype  (a)ssignee  (p)riority  (l)abel   esc cancel"))
	case modeConfirmDelete:
		if t, ok := m.selected(); ok {
			b.WriteString(noteSty.Render(fmt.Sprintf("delete %s:%s %q? (y/N)", t.Backend, t.ID, t.Title)))
		}
	default:
		if m.notice != "" {
			b.WriteString(m.renderNotice())
		} else {
			b.WriteString(helpSty.Render("←→ column  ↑↓ card  H/L move  enter open  e edit  n new  d delete  / filter  r reload  ? help  q quit"))
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

var (
	overdueSty = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
	soonSty    = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
)

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
	if board.Finished(m.board.Columns[c].Name) {
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
	if r := []rune(tag + text); m.width > 0 && len(r) > m.width {
		r = append(r[:m.width-1], '…')
		if n := len([]rune(tag)); len(r) <= n {
			tag, text = string(r), ""
		} else {
			text = string(r[n:])
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
	h := m.height - 5 - footer
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
			headStyle = noteSty.Bold(true)
			title += " !"
		}
	}
	head := headStyle.Render(title)

	cards := make([]string, len(tasks))
	for i, t := range tasks {
		cards[i] = renderCard(t, cw, sel && i == m.row, m.board.Columns[c].Color, m.dueStyle(t, c))
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
	if sel {
		for top < m.row {
			h := 0
			for i := top; i <= m.row; i++ {
				h += lipgloss.Height(cards[i]) + hdr(i, top)
			}
			if h <= cardsH {
				break
			}
			top++
		}
	}
	var out []string
	out = append(out, head)
	used := 0
	if top > 0 {
		out = append(out, metaSty.Render(fmt.Sprintf("↑ %d more", top)))
	}
	for i := top; i < len(cards); i++ {
		hh := hdr(i, top)
		h := lipgloss.Height(cards[i]) + hh
		// Always draw the first card, even if taller than the budget, so the
		// selection never disappears on very small terminals.
		if used+h > cardsH && i > top {
			out = append(out, metaSty.Render(fmt.Sprintf("↓ %d more", len(cards)-i)))
			break
		}
		if hh > 0 {
			out = append(out, metaSty.Render(fmt.Sprintf("-- %s (%d) --", groups[i], counts[groups[i]])))
		}
		out = append(out, cards[i])
		used += h
	}
	return lipgloss.NewStyle().Width(cw).Render(strings.Join(out, "\n"))
}

func renderCard(t task.Task, cw int, selected bool, color string, due lipgloss.Style) string {
	style := cardSty
	if selected {
		style = cardSel
		if color != "" {
			style = style.BorderForeground(lipgloss.Color(color))
		}
	}
	title := fmt.Sprintf("#%s %s", t.ID, t.Title)
	body := lipgloss.NewStyle().MaxHeight(3).Render(title)
	body += "\n" + cardMeta(t, cw-6, due)
	// Width counts padding but not the border.
	return style.Width(cw - 2).Render(body)
}

// cardMeta is the card's second line: the owning backend, then priority and type.
// width is the room available inside the card.
func cardMeta(t task.Task, width int, dueStyle lipgloss.Style) string {
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
		parts = append(parts, part{"backend", string(name), lipgloss.NewStyle().Foreground(accent)})
	}
	if t.Priority != "" {
		col := priorityColorOf(t.Priority)
		parts = append(parts, part{"priority", t.Priority, lipgloss.NewStyle().Foreground(col)})
	}
	if t.Type != "" {
		parts = append(parts, part{"type", t.Type, lipgloss.NewStyle().Foreground(typeColor(t.Type))})
	}
	if t.Assignee != "" {
		parts = append(parts, part{"assignee", "@" + t.Assignee, metaSty})
	}
	if t.Due != "" {
		parts = append(parts, part{"due", "due " + t.Due, dueStyle})
	}
	if chip := labelChip(t); chip != "" {
		parts = append(parts, part{"labels", chip, metaSty})
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
	b.WriteString(titleSty.Render(fmt.Sprintf("#%s  %s", t.ID, t.Title)) + "\n")
	b.WriteString(metaSty.Render(strings.Join(nonEmpty(t.Backend, t.Status, t.Priority, t.Type, atName(t.Assignee), dateLabel("due", t.Due), dateLabel("start", t.Start), t.Labels), " · ")) + "\n\n")
	if t.Description != "" {
		b.WriteString(wrap.Render(t.Description) + "\n\n")
	}
	if t.Notes != "" {
		b.WriteString(headSty.Render("Notes") + "\n" + wrap.Render(t.Notes) + "\n\n")
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
	foot := "e edit · esc/enter back"
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
	if m.helpPage == 1 {
		return titleSty.Render("tkan help 2/2") + "\n\n" +
			query.Help(m.fields, m.spec.Search) + "\n" +
			helpSty.Render("tab/←→ other page · esc back")
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
		{"n", "new task in the current column"},
		{"b", "cycle the backend new tasks go to"},
		{"d", "delete card (asks to confirm)"},
		{"u", "undo the last move, edit or delete (a deleted card gets a new ID)"},
		{"/", "filter: text or field:value (esc clears; up/down recall earlier filters)"},
		{"f", "filter by the selected card: then t type, a assignee, p priority, l label"},
		{"v", "cycle the saved views (board.views), then none"},
		{"z", "group cards by priority / type / assignee / labels (cycle)"},
		{"s / S", "sort cards: next field / reverse (display only)"},
		{"r / A", "reload from the backend / audit trail of reloads and writes"},
		{"?", "this help (tab: filter syntax)"},
		{"q / ctrl+c", "quit"},
	}
	var b strings.Builder
	b.WriteString(titleSty.Render("tkan help 1/2: keys") + "\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %-18s %s\n", r[0], r[1]))
	}
	b.WriteString("\n" + helpSty.Render("tab/←→ filter syntax · esc back"))
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
