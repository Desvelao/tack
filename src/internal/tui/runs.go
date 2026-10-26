package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/query"
	"tack/internal/task"
)

const (
	// resultLifetime is how long a finished background run stays in the strip.
	resultLifetime = 8 * time.Second
	maxStrip       = 3   // lines the strip takes at most
	maxRuns        = 50  // runs remembered for the runs view
	runsLoad       = 200 // stored runs read from the action log on R (A reads all)
)

// run is one background action started from the board. Times come from the
// model's clock (tests replace it); the Job holds the process and its output.
type run struct {
	id      int
	job     *action.Job
	started time.Time
	ended   time.Time // set when the UI handled the end
	done    bool
}

// runLog is shared by the copies Bubble Tea makes of the Model.
type runLog struct {
	runs []*run // oldest first
	next int
}

func (l *runLog) add(j *action.Job, now time.Time) *run {
	l.next++
	r := &run{id: l.next, job: j, started: now}
	l.runs = append(l.runs, r)
	for len(l.runs) > maxRuns { // drop the oldest finished one
		dropped := false
		for i, old := range l.runs {
			if old.done {
				l.runs = append(l.runs[:i], l.runs[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			break
		}
	}
	return r
}

func (l *runLog) get(id int) *run {
	if l == nil {
		return nil
	}
	for _, r := range l.runs {
		if r.id == id {
			return r
		}
	}
	return nil
}

// newest returns the runs, newest first.
func (l *runLog) newest() []*run {
	if l == nil {
		return nil
	}
	out := make([]*run, len(l.runs))
	for i, r := range l.runs {
		out[len(l.runs)-1-i] = r
	}
	return out
}

// forCard is the (backend, id) -> runs lookup: the runs of this session on
// the card k, newest first.
func (l *runLog) forCard(k cardKey) []*run {
	if l == nil {
		return nil
	}
	var out []*run
	for i := len(l.runs) - 1; i >= 0; i-- {
		if r := l.runs[i]; keyOf(r.job.Task) == k {
			out = append(out, r)
		}
	}
	return out
}

// runningOn counts the runs of the action called name still going on the card k.
func (l *runLog) runningOn(k cardKey, name string) int {
	n := 0
	for _, r := range l.forCard(k) {
		if !r.done && r.job.Spec.Name == name {
			n++
		}
	}
	return n
}

// cardRuns are the runs of the card to show on it and in its details: the
// ones still going and the ones that ended less than resultLifetime ago.
func (m Model) cardRuns(t task.Task) (going, ended []*run) {
	now := m.clock()
	for _, r := range m.runs.forCard(keyOf(t)) {
		switch {
		case !r.done:
			going = append(going, r)
		case now.Sub(r.ended) < resultLifetime:
			ended = append(ended, r)
		}
	}
	return going, ended
}

// runResult is "OK name 3s" / "FAIL name 3s" for a run that ended.
func runResult(r *run) string {
	tag := okSty.Bold(true).Render("OK")
	if r.job.Snapshot().Err != nil {
		tag = failSty.Render("FAIL")
	}
	return tag + " " + r.job.Spec.Name + metaSty.Render(" "+formatDur(r.ended.Sub(r.started)))
}

// runMarker is the card's run line: the newest action going on it with its
// elapsed time ("⟳ review 12s +1" when more are going), else the result of
// one that just ended; "" when neither. It is cut to width cells.
func (m Model) runMarker(t task.Task, width int) string {
	going, ended := m.cardRuns(t)
	var line string
	switch {
	case len(going) > 0:
		r := going[0]
		line = loadSty.Render("⟳ "+r.job.Spec.Name) + metaSty.Render(" "+formatDur(m.clock().Sub(r.started)))
		if len(going) > 1 {
			line += loadSty.Render(fmt.Sprintf(" +%d", len(going)-1))
		}
	case len(ended) > 0:
		line = runResult(ended[0])
	default:
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}

// cancelAll kills every run still going (tack is quitting).
func (l *runLog) cancelAll() {
	if l == nil {
		return
	}
	for _, r := range l.runs {
		if !r.done {
			r.job.Cancel()
		}
	}
}

// formatDur is a short duration: "<1s", "7s", "1m12s".
func formatDur(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Round(time.Second).String()
}

func (r *run) label() string {
	t := r.job.Task
	return fmt.Sprintf("%s #%s %s", r.job.Spec.Name, t.ID, t.Title)
}

// runsStrip is the lines above the footer: runs in progress with their elapsed
// time, and runs that ended less than resultLifetime ago with their result.
func (m Model) runsStrip() []string {
	var lines []string
	now, more := m.clock(), 0
	for _, r := range m.runs.newest() {
		if r.done && now.Sub(r.ended) >= resultLifetime {
			continue
		}
		if len(lines) == maxStrip {
			more++
			continue
		}
		var line string
		switch snap := r.job.Snapshot(); {
		case !r.done:
			line = loadSty.Render("⟳ running") + " " + cut(r.label(), m.width-24) + metaSty.Render("  "+formatDur(now.Sub(r.started)))
		case snap.Err != nil:
			line = failSty.Render("FAIL") + "  " + cut(r.label(), m.width-14) + metaSty.Render("  "+formatDur(r.ended.Sub(r.started)))
		default:
			line = okSty.Bold(true).Render("OK") + "    " + cut(r.label(), m.width-14) + metaSty.Render("  "+formatDur(r.ended.Sub(r.started)))
		}
		lines = append(lines, line)
	}
	if more > 0 {
		lines[len(lines)-1] += metaSty.Render(fmt.Sprintf("  (+%d more, R)", more))
	}
	return lines
}

// runItem is one row of the runs view: a run of this session (live) or one
// read from the action log (earlier sessions).
type runItem struct {
	live *run
	rec  *actionlog.Record
}

// WithActionLog gives the model the optional action log: runs are stored in
// it when they end, and the runs view lists the earlier ones.
func (m Model) WithActionLog(l *actionlog.Log) Model {
	m.alog = l
	return m
}

// runItems are the rows of the runs view, newest first: this session's runs,
// then the stored ones, minus what the view's filter rejects.
func (m Model) runItems() []runItem {
	var items []runItem
	for _, r := range m.runs.newest() {
		items = append(items, runItem{live: r})
	}
	for i := range m.runStored {
		items = append(items, runItem{rec: &m.runStored[i]})
	}
	if m.runQ.Empty() {
		return items
	}
	var out []runItem
	for _, it := range items {
		if rec, _ := m.recordOf(it); m.runQ.Match(actionlog.Lookup(rec), actionlog.SearchFields) {
			out = append(out, it)
		}
	}
	return out
}

// recordOf is the full record of an item. A run still going has result
// "running" and the time so far.
func (m Model) recordOf(it runItem) (rec actionlog.Record, running bool) {
	if it.rec != nil {
		return *it.rec, false
	}
	r := it.live
	rec = actionlog.FromJob(r.job, "tui")
	rec.Session = m.alog.Session()
	if r.done {
		rec.DurationMS = r.ended.Sub(r.started).Milliseconds()
		return rec, false
	}
	rec.Result, rec.Error, rec.ExitCode = "running", "", nil
	rec.DurationMS = m.clock().Sub(r.started).Milliseconds()
	return rec, true
}

// loadStored reads the newest limit stored runs (0 = all) and counts the older ones left out.
func (m *Model) loadStored(limit int) bool {
	m.runStored, m.runOlder = nil, 0
	if !m.alog.Enabled() {
		return true
	}
	recs, older, err := m.alog.Recent(false, limit)
	if err != nil {
		m.fail("error: " + err.Error())
		return false
	}
	for i := len(recs) - 1; i >= 0; i-- { // newest first
		m.runStored = append(m.runStored, recs[i])
	}
	m.runOlder = older
	return true
}

// openRuns shows the list of runs; esc returns to the current mode.
func (m *Model) openRuns() {
	if !m.loadStored(runsLoad) {
		return
	}
	if len(m.runItems()) == 0 && m.runQ.Empty() {
		if m.alog.Enabled() {
			m.notice = "no action has run yet (x starts one)"
		} else {
			m.notice = "no background action has run yet (x starts one)"
		}
		return
	}
	m.runBack, m.mode, m.runIdx, m.runEditing = m.mode, modeRuns, 0, false
	m.unseen = 0
}

// openRunOutput shows the output of an item; esc returns to the mode back.
func (m *Model) openRunOutput(it runItem, back mode) {
	m.runCur, m.runBackOut, m.runFollow, m.runY, m.mode = it, back, true, 0, modeRunOutput
}

func (m Model) handleRunsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if m.runEditing {
		if key == "?" && m.input.Value() == "" {
			m.openFilterHelp(modeRuns, "Action run fields", actionlog.FieldDocs, actionlog.SearchFields)
			return m, nil
		}
		switch key {
		case "esc":
			m.runEditing = false
			m.input.Blur()
		case "enter":
			val := strings.TrimSpace(m.input.Value())
			q, err := actionlog.Parse(val)
			if err != nil { // keep the prompt open and the previous filter
				m.notice = "filter: " + err.Error()
				return m, nil
			}
			m.runQ, m.runFilter, m.runIdx, m.runEditing = q, val, 0, false
			m.input.Blur()
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(k)
			return m, cmd
		}
		return m, nil
	}
	items := m.runItems()
	m.runIdx = m.clampRunIdx(len(items)) // the list may have shrunk under a filter
	switch key {
	case "esc":
		if m.runFilter != "" {
			m.runFilter, m.runQ, m.runIdx = "", query.Query{}, 0
		} else {
			m.mode = m.runBack
		}
	case "q", "R":
		m.mode = m.runBack
	case "A":
		if m.runOlder > 0 {
			if m.loadStored(0) {
				m.notice = "loaded all stored runs"
			}
		}
	case "/":
		m.runEditing = true
		m.input.SetValue(m.runFilter)
		m.input.CursorEnd()
		m.input.Prompt = "/"
		m.input.Focus()
	case "up", "k":
		if m.runIdx > 0 {
			m.runIdx--
		}
	case "down", "j":
		if m.runIdx < len(items)-1 {
			m.runIdx++
		}
	case "pgdown":
		m.runIdx = min(m.runIdx+m.listRoom(6), len(items)-1)
	case "pgup":
		m.runIdx = max(m.runIdx-m.listRoom(6), 0)
	case "home", "g":
		m.runIdx = 0
	case "end", "G":
		m.runIdx = max(len(items)-1, 0)
	case "enter":
		if m.runIdx < len(items) {
			m.openRunOutput(items[m.runIdx], modeRuns)
		}
	case "c":
		if m.runIdx < len(items) {
			if r := items[m.runIdx].live; r == nil || r.done {
				m.notice = "not running"
			} else {
				r.job.Cancel()
				m.notice = "canceling " + r.job.Spec.Name + "…"
			}
		}
	}
	return m, nil
}

func (m Model) runsView() string {
	var b strings.Builder
	items := m.runItems()
	where := "this session"
	if m.alog.Enabled() {
		where = "this session + " + m.alog.Path()
	}
	head := titleSty.Render("tack runs") + metaSty.Render(fmt.Sprintf("  %d runs · %s", len(items), where))
	if m.runOlder > 0 {
		head += noteSty.Render(fmt.Sprintf("  +%d older, A loads", m.runOlder))
	}
	if m.runFilter != "" {
		head += filterSty.Render("  filter: " + m.runFilter)
	}
	b.WriteString(head + "\n\n")
	// One row per run, plus the separator before the stored ones; only the rows
	// around the selection are drawn, so the header and the footer stay on screen.
	type row struct {
		text string
		item int // index in items; -1 for the separator
	}
	var rows []row
	sel := 0
	if len(items) == 0 {
		rows = append(rows, row{"  (nothing matches)", -1})
	}
	earlier := false
	runIdx := m.clampRunIdx(len(items))
	for i, it := range items {
		if it.rec != nil && !earlier {
			earlier = true
			rows = append(rows, row{headSty.Render("earlier sessions (action_log)"), -1})
		}
		rec, running := m.recordOf(it)
		state := strings.ToUpper(rec.Result)
		if running {
			state = "running"
		}
		line := fmt.Sprintf("%-8s %s  %-6s %s %s #%s %s", state, rec.Time.Local().Format("01-02 15:04:05"), actionlog.FormatDuration(rec.Duration()), runName(rec), rec.Backend, rec.ID, rec.Title)
		line = cut(line, m.width-3)
		if i == runIdx {
			line = titleSty.Render("> " + line)
			sel = len(rows)
		} else {
			line = "  " + line
		}
		rows = append(rows, row{line, i})
	}
	room := m.listRoom(2 + 2) // the header and its blank line, the blank line and the footer
	if m.notice != "" {
		room--
	}
	start, end := window(len(rows), sel, room)
	pos := ""
	if first, last := -1, -1; len(rows) > room {
		for _, r := range rows[start:end] {
			if r.item >= 0 {
				if first < 0 {
					first = r.item
				}
				last = r.item
			}
		}
		if first >= 0 {
			pos = fmt.Sprintf(" (%d-%d of %d)", first+1, last+1, len(items))
		}
	}
	for _, r := range rows[start:end] {
		b.WriteString(r.text + "\n")
	}
	if m.runEditing {
		b.WriteString("\n" + m.input.View() + "  " + helpSty.Render("? fields and syntax · enter apply · esc cancel"))
	} else {
		b.WriteString("\n" + helpSty.Render("↑↓/pgup/pgdn choose · enter output · / filter · c cancel · esc back"+pos))
	}
	if m.notice != "" {
		b.WriteString("\n" + m.renderNotice())
	}
	return b.String()
}

// runName is the action of a run; a hook run shows its event too: "name [task.moved]".
func runName(rec actionlog.Record) string {
	if rec.Event == "" {
		return rec.Action
	}
	return rec.Action + " [" + rec.Event + "]"
}

// runOutputLines is the body of the output view, wrapped to the width: the
// command, the stdin, then stdout and stderr.
func (m Model) runOutputLines(rec actionlog.Record) []string {
	w := m.width - 4
	if w < 20 {
		w = 20
	}
	wrap := lipgloss.NewStyle().Width(w)
	var text strings.Builder
	text.WriteString("--- command (in " + sanitize(rec.Workdir) + ") ---\n" + sanitize(action.Plan{Argv: rec.Command}.Line()))
	if rec.Stdin != "" {
		text.WriteString("\n\n--- stdin ---\n" + strings.TrimRight(sanitize(rec.Stdin), "\n"))
	}
	if rec.Mode == action.ModeInteractive {
		text.WriteString("\n\n(ran with the terminal: output was not captured)")
	} else {
		out := strings.TrimRight(sanitize(rec.Stdout), "\n")
		if out == "" {
			out = "(no output yet)"
			if rec.Result != "running" {
				out = "(none)"
			}
		}
		text.WriteString("\n\n--- stdout ---\n" + out)
		if e := strings.TrimRight(sanitize(rec.Stderr), "\n"); e != "" {
			text.WriteString("\n\n--- stderr ---\n" + e)
		}
	}
	if rec.Truncated {
		text.WriteString("\n[output truncated]")
	}
	return strings.Split(wrap.Render(text.String()), "\n")
}

// runRoom is how many output lines fit under the two header lines and the blank one.
func (m Model) runRoom() int {
	if r := m.detailRoom() - 1; r > 2 {
		return r
	}
	return 2
}

// runYNow is the first visible line: the end of the output while following.
func (m Model) runYNow(lines []string) int {
	room := m.runRoom()
	last := len(lines) - room
	if last < 0 {
		last = 0
	}
	if m.runFollow || m.runY > last {
		return last
	}
	if m.runY < 0 {
		return 0
	}
	return m.runY
}

func (m Model) handleRunOutputKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	rec, running := m.recordOf(m.runCur)
	lines := m.runOutputLines(rec)
	y, room := m.runYNow(lines), m.runRoom()
	last := len(lines) - room
	if last < 0 {
		last = 0
	}
	switch k.String() {
	case "esc", "q":
		m.mode = m.runBackOut
		return m, nil
	case "down", "j":
		y++
	case "up", "k":
		y--
	case "pgdown", " ":
		y += room
	case "pgup":
		y -= room
	case "home", "g":
		y = 0
	case "end", "G":
		y = last
	}
	if y < 0 {
		y = 0
	}
	m.runY = y
	m.runFollow = y >= last && running // reaching the end of a running command follows it
	return m, nil
}

func (m Model) runOutputView() string {
	rec, running := m.recordOf(m.runCur)
	var state string
	switch {
	case running:
		state = "running " + formatDur(rec.Duration())
	case rec.Result != actionlog.ResultOK:
		state = failSty.Render("FAIL") + " in " + formatDur(rec.Duration()) + ": " + cut(rec.Error, m.width-20)
	default:
		state = okSty.Bold(true).Render("OK") + " in " + formatDur(rec.Duration())
	}
	if rec.Mode != action.ModeInteractive && rec.ExitCode != nil {
		state += fmt.Sprintf(" · exit %d", *rec.ExitCode)
	}
	state += metaSty.Render(" · started " + rec.Time.Local().Format("2006-01-02 15:04:05"))
	head := titleSty.Render(cut(fmt.Sprintf("%s on %s:%s", runName(rec), rec.Backend, rec.ID), m.width-2)) + "\n" + state

	lines := m.runOutputLines(rec)
	room := m.runRoom()
	y := m.runYNow(lines)
	end := y + room
	if end > len(lines) {
		end = len(lines)
	}
	foot := "↑↓/pgup/pgdn scroll · G follows the end · esc back"
	if len(lines) > room {
		foot += fmt.Sprintf(" (%d-%d of %d)", y+1, end, len(lines))
	}
	return head + "\n\n" + strings.Join(lines[y:end], "\n") + "\n" + helpSty.Render(foot)
}

// clampRunIdx keeps the selection inside a list of n rows. The list is newest
// first and the view cannot start runs, so the rows only change when a filter
// is applied; the selection then stays on its position.
func (m Model) clampRunIdx(n int) int {
	if m.runIdx >= n {
		return max(n-1, 0)
	}
	return max(m.runIdx, 0)
}

// listRoom is how many rows of a list fit on the screen next to chrome lines
// of other content (never fewer than 3).
func (m Model) listRoom(chrome int) int {
	return max(m.height-chrome, 3)
}

// window picks the rows [start, end) of n to draw, room of them, with row sel
// in the middle when the list is longer than the room.
func window(n, sel, room int) (start, end int) {
	if n <= room {
		return 0, n
	}
	start = min(max(sel-room/2, 0), n-room)
	return start, start + room
}
