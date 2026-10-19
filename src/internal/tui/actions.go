package tui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tkan/internal/action"
	"tkan/internal/actionlog"
	"tkan/internal/audit"
	"tkan/internal/backend"
	"tkan/internal/claim"
	"tkan/internal/outcome"
	"tkan/internal/task"
)

// actionDoneMsg reports that an action finished (or could not start). run is
// the id of a background run (0 for an interactive one, which carries err).
type actionDoneMsg struct {
	run     int
	name    string
	task    task.Task
	err     error
	spec    action.Spec // interactive runs: what ran, for the records
	plan    action.Plan
	started time.Time
	batch   bool // one of the runs of m.batch
	claimed bool // the run claimed task (claim: true): release it at the end
}

// WithActions gives the model the configured actions (the `x` picker); dir is
// the config directory their relative cwd resolves against.
func (m Model) WithActions(specs []action.Spec, dir string) Model {
	m.allActions, m.actionDir = specs, dir
	m.runs = &runLog{}
	return m
}

// WithActionLimit sets how many runs of an action started on the marked cards
// go at once (action_max_parallel; below 1 means action.DefaultMaxParallel).
func (m Model) WithActionLimit(n int) Model {
	m.actionMax = n
	return m
}

// openActions shows the picker for the selected card (or the marked cards),
// remembering where to return.
func (m *Model) openActions() {
	if _, ok := m.selected(); !ok && len(m.marks) == 0 {
		return
	}
	if len(m.allActions) == 0 {
		m.notice = "no actions configured (add an `actions:` list; see docs/config.md)"
		return
	}
	m.actions = m.pickerActions()
	if len(m.actions) == 0 {
		m.notice = "no action applies to the tasks of " + strings.Join(m.targetBackends(), " and ") + " (actions of another backend only)"
		return
	}
	m.actionBack, m.mode = m.mode, modeActions
	m.actionIdx, m.actionTTY = 0, false
}

// targetBackends are the backends of the cards an action would run on (the
// marked ones, else the selected one), in board order.
func (m Model) targetBackends() []string {
	cards := m.markedTasks()
	if len(cards) == 0 {
		if t, ok := m.selected(); ok {
			cards = []task.Task{t}
		}
	}
	var out []string
	for _, t := range cards {
		if !slices.Contains(out, t.Backend) {
			out = append(out, t.Backend)
		}
	}
	return out
}

// pickerActions are the actions that apply to every target card's backend,
// as the first backend defines them (each card runs its own backend's; see
// actionFor).
func (m Model) pickerActions() []action.Spec {
	bs := m.targetBackends()
	if len(bs) == 0 {
		return nil
	}
	var out []action.Spec
	for _, s := range action.For(m.allActions, bs[0]) {
		if !slices.ContainsFunc(bs[1:], func(b string) bool { _, ok := m.actionFor(b, s.Name); return !ok }) {
			out = append(out, s)
		}
	}
	return out
}

// actionFor is the action called name as it applies to the tasks of backend.
func (m Model) actionFor(backend, name string) (action.Spec, bool) {
	return action.Find(action.For(m.allActions, backend), name)
}

func (m Model) handleActionsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	switch key {
	case "esc", "q", "x":
		m.mode = m.actionBack
		return m, nil
	case "up", "k":
		if m.actionIdx > 0 {
			m.actionIdx--
		}
		return m, nil
	case "down", "j":
		if m.actionIdx < len(m.actions)-1 {
			m.actionIdx++
		}
		return m, nil
	case "i": // the next action runs with the terminal
		m.actionTTY = !m.actionTTY
		return m, nil
	case "pgdown":
		m.actionIdx = min(m.actionIdx+m.listRoom(6), len(m.actions)-1)
		return m, nil
	case "pgup":
		m.actionIdx = max(m.actionIdx-m.listRoom(6), 0)
		return m, nil
	case "home":
		m.actionIdx = 0
		return m, nil
	case "end":
		m.actionIdx = len(m.actions) - 1
		return m, nil
	case "enter":
		return m.pick(m.actions[m.actionIdx])
	}
	for _, s := range m.actions {
		if s.Key != "" && strings.EqualFold(s.Key, key) {
			return m.pick(s)
		}
	}
	return m, nil
}

// pick starts the chosen action: on the marked cards when there are any,
// otherwise on the selected one.
func (m Model) pick(s action.Spec) (tea.Model, tea.Cmd) {
	if len(m.marks) > 0 {
		return m.startBatch(s)
	}
	return m.startAction(s)
}

// repeatAction (`.`) starts the last action again on the selected card, with
// the terminal if it had it because of `i`.
func (m Model) repeatAction() (tea.Model, tea.Cmd) {
	if m.lastAction.Name == "" {
		m.notice = "no action run yet (x picks one)"
		return m, nil
	}
	if _, ok := m.selected(); !ok {
		return m, nil
	}
	m.actionBack, m.actionTTY = m.mode, m.lastTTY
	return m.startAction(m.lastAction)
}

// startAction runs s on the selected card: with the terminal (the TUI is
// suspended) when the action says so or `i` was pressed, otherwise in the
// background.
func (m Model) startAction(s action.Spec) (tea.Model, tea.Cmd) {
	t, ok := m.selected()
	m.mode = m.actionBack
	if !ok {
		return m, nil
	}
	m.lastAction, m.lastTTY = s, m.actionTTY
	s, ok = m.actionFor(t.Backend, s.Name) // `.` may repeat it on a card of another backend
	if !ok {
		m.fail(fmt.Sprintf("error: %s does not apply to the tasks of %s", m.lastAction.Name, t.Backend))
		return m, nil
	}
	interactive := m.actionTTY || s.Mode == action.ModeInteractive
	s = s.WithRun(actionlog.NewRunID(), m.alog.Path())
	if !m.confirmDuplicate(s, t, interactive) {
		return m, nil
	}
	t, claimed, err := m.claimForRun(s, t, interactive)
	if err != nil {
		m.fail("error: " + err.Error())
		return m, nil
	}
	if interactive {
		cmd, err := s.Cmd(t, m.actionDir)
		if err != nil {
			m.releaseAfterRun(t, claimed)
			m.fail("error: " + err.Error())
			return m, nil
		}
		m.hooks.FireAction(t, s.Name, "tui", nil)
		plan, started := s.Expand(t, m.actionDir), m.clock()
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
			if err != nil {
				err = fmt.Errorf("%s: %w", s.Name, err)
			}
			return actionDoneMsg{name: s.Name, task: t, err: err, spec: s, plan: plan, started: started, claimed: claimed}
		})
	}
	m.notice = fmt.Sprintf("started %s on #%s (R: runs)", s.Name, t.ID)
	return m, m.startBackground(s, t, false, claimed)
}

// claimAgent is who the board claims tasks as for s: $TKAN_AGENT, else the
// action's name (so the fleet view tells the agents apart), else "tui".
func claimAgent(s action.Spec) string {
	if a := strings.TrimSpace(os.Getenv("TKAN_AGENT")); a != "" {
		return a
	}
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return "tui"
}

// claimForRun claims t for a run of s when s has claim: true (for as long as
// the run may take); claimed says whether it did. An error (somebody else
// holds the card, or its backend cannot store the claim) means: do not start.
func (m *Model) claimForRun(s action.Spec, t task.Task, interactive bool) (task.Task, bool, error) {
	if !s.Claim {
		return t, false, nil
	}
	r := claim.Request{Agent: claimAgent(s), Lease: s.ClaimLease(interactive)}
	if err := claim.Check(m.be, t, r); err != nil {
		return t, false, err
	}
	c, err := claim.Take(context.Background(), m.be, t, r, m.clock())
	if err != nil {
		return t, false, err
	}
	return c, true, nil
}

// releaseAfterRun gives back the claim claimForRun took; one taken over by
// somebody else meanwhile is left alone.
func (m *Model) releaseAfterRun(t task.Task, claimed bool) {
	if !claimed {
		return
	}
	if _, _, err := claim.Give(context.Background(), m.be, t, t.Assignee, false, ""); err != nil {
		m.fail("error: release #" + t.ID + ": " + err.Error())
	}
}

// dupWindow is how long a warning about starting a running action again
// stays armed: starting it once more within it runs it a second time.
const dupWindow = 10 * time.Second

// dupStart is the card and action of the last duplicate-start warning.
type dupStart struct {
	card   cardKey
	action string
	at     time.Time
}

// confirmDuplicate reports whether s may start on t. When s is already running
// on the card, the first start only warns; the same start again (x or .)
// within dupWindow goes ahead. An `exclusive` background action is refused.
func (m *Model) confirmDuplicate(s action.Spec, t task.Task, interactive bool) bool {
	n := m.runs.runningOn(keyOf(t), s.Name)
	if n == 0 {
		return true
	}
	if s.Exclusive && !interactive {
		m.fail(fmt.Sprintf("error: %s is already running on #%s (exclusive; R: runs, c cancels)", s.Name, t.ID))
		return false
	}
	now := m.clock()
	if a := m.dupArm; a.card == keyOf(t) && a.action == s.Name && now.Sub(a.at) < dupWindow {
		m.dupArm = dupStart{}
		return true
	}
	m.dupArm = dupStart{card: keyOf(t), action: s.Name, at: now}
	m.notice = fmt.Sprintf("%s is already running on #%s (%d): start it again (x or .) to run it once more", s.Name, t.ID, n)
	return false
}

// startBackground starts s on t without the terminal; the command it returns
// reports the end.
func (m *Model) startBackground(s action.Spec, t task.Task, inBatch, claimed bool) tea.Cmd {
	m.hooks.FireAction(t, s.Name, "tui", nil)
	s = s.WithRun(actionlog.NewRunID(), m.alog.Path())
	j := s.Start(context.Background(), t, m.actionDir)
	r := m.runs.add(j, m.clock())
	return func() tea.Msg {
		<-j.Done()
		return actionDoneMsg{run: r.id, name: s.Name, task: t, batch: inBatch, claimed: claimed}
	}
}

// cardKey identifies a card across reloads (IDs are unique within a backend).
type cardKey struct{ backend, id string }

func keyOf(t task.Task) cardKey { return cardKey{t.Backend, t.ID} }

func (m Model) marked(t task.Task) bool { return m.marks[keyOf(t)] }

// toggleMark marks or unmarks the selected card. The map is copied, not
// changed in place: Bubble Tea's earlier copies of the Model keep theirs.
func (m *Model) toggleMark() {
	t, ok := m.selected()
	if !ok {
		return
	}
	marks := make(map[cardKey]bool, len(m.marks)+1)
	for k := range m.marks {
		marks[k] = true
	}
	if marks[keyOf(t)] {
		delete(marks, keyOf(t))
	} else {
		marks[keyOf(t)] = true
	}
	m.marks = marks
	if len(marks) == 0 {
		m.notice = "no cards marked"
		return
	}
	m.notice = fmt.Sprintf("%d marked: x runs an action, m moves, d deletes them, esc unmarks all", len(marks))
}

// pruneMarks drops the marks of cards that are gone after a reload. A
// backend that did not load completely (perr) keeps its marks: its cards
// are missing from the list, not deleted.
func (m *Model) pruneMarks(perr *backend.PartialError) {
	if len(m.marks) == 0 {
		return
	}
	skip := map[string]bool{}
	if perr != nil {
		for _, n := range perr.Names {
			skip[n] = true
		}
	}
	present := map[cardKey]bool{}
	for _, t := range m.tasks {
		present[keyOf(t)] = true
	}
	var kept map[cardKey]bool
	for k := range m.marks {
		if present[k] || skip[k.backend] {
			if kept == nil {
				kept = map[cardKey]bool{}
			}
			kept[k] = true
		}
	}
	m.marks = kept
}

// markedTasks are the marked cards in board order (whether the filter shows them or not).
func (m Model) markedTasks() []task.Task {
	var out []task.Task
	for _, t := range m.tasks {
		if m.marked(t) {
			out = append(out, t)
		}
	}
	return out
}

// batch is an action started on the marked cards: at most Model.actionMax
// runs go at once, the other cards wait in queue, and one notice sums up when
// the last run ends.
type batch struct {
	spec   action.Spec
	queue  []task.Task // cards still to start, in board order
	active int         // runs going
	total  int
	failed []string // "#ID" of the runs that failed
	gone   []string // "#ID" of the cards that disappeared before their turn
	busy   []string // "#ID" of the cards skipped: the action was already running on them
}

// startBatch starts s on the marked cards in the background and clears the marks.
func (m Model) startBatch(s action.Spec) (tea.Model, tea.Cmd) {
	m.mode = m.actionBack
	interactive := slices.ContainsFunc(m.markedTasks(), func(t task.Task) bool {
		r, ok := m.actionFor(t.Backend, s.Name)
		return ok && r.Mode == action.ModeInteractive
	})
	if m.actionTTY || s.Mode == action.ModeInteractive || interactive {
		m.fail(fmt.Sprintf("%s runs with the terminal, one card at a time: unmark the cards (esc) to run it on the selected one", s.Name))
		return m, nil
	}
	if m.batch != nil {
		m.fail(fmt.Sprintf("%s is still going over marked cards (R: runs); wait for it to end", m.batch.spec.Name))
		return m, nil
	}
	cards := m.markedTasks()
	m.batch = &batch{spec: s, queue: cards, total: len(cards)}
	m.marks = nil
	m.lastAction, m.lastTTY = s, false
	m.notice = fmt.Sprintf("started %s on %d cards, %d at a time (R: runs)", s.Name, len(cards), m.batchLimit())
	return m, m.fillBatch()
}

func (m Model) batchLimit() int {
	if m.actionMax < 1 {
		return action.DefaultMaxParallel
	}
	return m.actionMax
}

// fillBatch starts queued cards until the limit is reached, taking each card
// as it is now (a card that is gone is skipped), and sums up when nothing is
// left to run.
func (m *Model) fillBatch() tea.Cmd {
	b := m.batch
	if b == nil {
		return nil
	}
	var cmds []tea.Cmd
	for b.active < m.batchLimit() && len(b.queue) > 0 {
		t := b.queue[0]
		b.queue = b.queue[1:]
		cur, ok := m.taskOf(keyOf(t))
		if !ok {
			b.gone = append(b.gone, "#"+t.ID)
			continue
		}
		if m.runs.runningOn(keyOf(cur), b.spec.Name) > 0 { // no second run of it on the same card
			b.busy = append(b.busy, "#"+t.ID)
			continue
		}
		spec, ok := m.actionFor(cur.Backend, b.spec.Name)
		if !ok { // the picker offered it for every marked card's backend
			spec = b.spec
		}
		cur, claimed, err := m.claimForRun(spec, cur, false)
		if err != nil { // claimed by somebody else: like a card that is busy
			b.busy = append(b.busy, "#"+t.ID)
			continue
		}
		b.active++
		cmds = append(cmds, m.startBackground(spec, cur, true, claimed))
	}
	if b.active == 0 && len(b.queue) == 0 {
		m.batch = nil
		m.whenFree(func(m *Model) { m.batchSummary(b) })
	}
	return tea.Batch(cmds...)
}

// taskOf finds a loaded card by backend and ID.
func (m Model) taskOf(k cardKey) (task.Task, bool) {
	for _, t := range m.tasks {
		if keyOf(t) == k {
			return t, true
		}
	}
	return task.Task{}, false
}

// batchSummary is the one notice of a batch: OK when every run succeeded.
func (m *Model) batchSummary(b *batch) {
	ran := b.total - len(b.gone) - len(b.busy)
	text := fmt.Sprintf("ran %s on %d cards: %d ok", b.spec.Name, ran, ran-len(b.failed))
	if len(b.gone)+len(b.busy) > 0 {
		text = fmt.Sprintf("ran %s on %d of %d cards: %d ok", b.spec.Name, ran, b.total, ran-len(b.failed))
	}
	if len(b.failed) > 0 {
		text += fmt.Sprintf(", %d failed (%s)", len(b.failed), strings.Join(b.failed, " "))
	}
	if len(b.gone) > 0 {
		text += fmt.Sprintf(", %d gone (%s)", len(b.gone), strings.Join(b.gone, " "))
	}
	if len(b.busy) > 0 {
		text += fmt.Sprintf(", %d skipped, already running (%s)", len(b.busy), strings.Join(b.busy, " "))
	}
	m.notify(b.spec, text)
	if len(b.failed) > 0 || len(b.gone) > 0 || len(b.busy) > 0 {
		m.fail("error: " + text + " (R: runs)")
		return
	}
	m.ok(text)
}

// whenFree runs f now, or once the user leaves the form they are typing in.
func (m *Model) whenFree(f func(*Model)) {
	if m.formOpen() {
		m.pending = append(m.pending, f)
		return
	}
	f(m)
}

// finishAction audits the run, reloads (an agent may have changed the backlog)
// and confirms with OK / FAIL and the duration. A run whose action says
// `show: output` opens its output unless the user is typing somewhere.
func (m *Model) finishAction(msg actionDoneMsg) {
	var (
		r     *run
		rec   actionlog.Record
		first string
		dur   time.Duration
	)
	err := msg.err
	if msg.run != 0 {
		if r = m.runs.get(msg.run); r == nil {
			return
		}
		r.ended, r.done = m.clock(), true
		if m.mode != modeRuns { // the runs view shows the end itself
			m.unseen++
		}
		rec = actionlog.FromJob(r.job, "tui")
		err, dur = r.job.Snapshot().Err, r.ended.Sub(r.started)
		first = oneLine(rec.Stdout)
	} else {
		rec = actionlog.Interactive(msg.spec, msg.task, msg.plan, msg.started, m.clock(), err, "tui")
	}
	if msg.batch && m.batch != nil {
		m.batch.active--
		if err != nil {
			m.batch.failed = append(m.batch.failed, "#"+msg.task.ID)
		}
	}
	if r != nil && !msg.batch { // a batch notifies once, when its last run ends
		res := "OK"
		if err != nil {
			res = "FAIL"
		}
		m.notify(r.job.Spec, fmt.Sprintf("%s on #%s: %s", msg.name, msg.task.ID, res))
	}
	logErr := m.logRun(msg.task, rec)
	m.hooks.FireAction(msg.task, msg.name, "tui", &rec)
	m.releaseAfterRun(msg.task, msg.claimed)
	spec, canceled := msg.spec, false
	if r != nil {
		spec, canceled = r.job.Spec, r.job.Snapshot().Canceled
	}
	var outcomeErr error
	if outcome.Wants(spec) {
		_, outcomeErr = outcome.Apply(context.Background(), m.be, m.board, spec, msg.task, rec, canceled)
	}
	// The command may have changed the card behind an older copy of it: undoing a
	// move or edit of it now would put that copy back over the changes.
	m.dropUndoFor(msg.task)
	settle := func(m *Model) {
		m.reloadNow("action " + msg.name)
		if m.async == nil && m.noticeKind == noticeFail { // the reload failed; keep that message
			return
		}
		switch {
		case msg.batch: // the batch sums up once, when its last run ends
		case err != nil:
			m.fail("error: " + strings.TrimPrefix(err.Error(), msg.name+": "))
		default:
			text := fmt.Sprintf("ran %s on #%s", msg.name, msg.task.ID)
			if r != nil {
				text += " in " + formatDur(dur)
			}
			if first = strings.TrimSpace(first); first != "" {
				text += ": " + first
			}
			m.ok(text)
		}
		if logErr != nil {
			m.fail("error: " + logErr.Error())
		}
		if outcomeErr != nil {
			m.fail("error: " + outcomeErr.Error())
		}
		if r != nil && r.job.Spec.Show == action.ShowOutput && !msg.batch {
			if m.mode == modeBoard || m.mode == modeDetail {
				m.openRunOutput(runItem{live: r}, m.mode)
			}
		}
	}
	// Reloading or writing a notice now would pull the card or the text from under the user.
	m.whenFree(settle)
}

// formOpen reports whether the user is in the middle of something that works on
// the cards as they are: typing, editing or confirming a delete.
func (m Model) formOpen() bool {
	switch m.mode {
	case modeNew, modeEdit, modeFilter, modeQuick, modeConfirmDelete, modeMoveTo:
		return true
	}
	return false
}

// flushPending runs what finishAction left for when the user is done typing.
func (m *Model) flushPending() {
	if m.formOpen() || len(m.pending) == 0 {
		return
	}
	pending := m.pending
	m.pending = nil
	for _, f := range pending {
		f(m)
	}
}

// dropUndoFor forgets the move and edit undos of t (a delete's undo creates the
// card again and stays valid).
func (m *Model) dropUndoFor(t task.Task) {
	kept := m.undo[:0:0]
	for _, op := range m.undo {
		if !op.recreate && (op.prev.Backend == t.Backend && op.prev.ID == t.ID ||
			slices.ContainsFunc(op.group, func(g task.Task) bool { return keyOf(g) == keyOf(t) })) {
			continue
		}
		kept = append(kept, op)
	}
	m.undo = kept
}

// blockedWarning is the picker's warning about blocked cards ("" if none):
// they can still be run, the picker only says that they are not ready.
func (m Model) blockedWarning(sel task.Task) string {
	if len(m.marks) == 0 {
		if open := m.deps.Blockers(sel); len(open) > 0 {
			return "blocked by #" + strings.Join(open, ", #") + ": not ready (running it anyway is allowed)"
		}
		return ""
	}
	n := 0
	for _, t := range m.tasks {
		if m.marked(t) && m.deps.Blocked(t) {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d of the marked cards are blocked (blocked_by not finished); they run too", n)
}

func (m Model) actionsView() string {
	t, _ := m.selected()
	head := fmt.Sprintf("run an action on #%s  %s", t.ID, t.Title)
	if n := len(m.marks); n > 0 {
		head = fmt.Sprintf("run an action on the %d marked cards (in the background, %d at a time)", n, m.batchLimit())
	}
	var b strings.Builder
	b.WriteString(titleSty.Render(cut(head, m.width-1)) + "\n")
	if w := m.blockedWarning(t); w != "" { // in the blank line under the title
		b.WriteString(blockedSty.Render(cut(w, m.width-1)))
	}
	b.WriteString("\n")
	// With many actions only the ones around the highlighted one are drawn.
	room := m.listRoom(6) // title, blank, blank, terminal note, help line, spare
	start, end := window(len(m.actions), m.actionIdx, room)
	for i := start; i < end; i++ {
		s := m.actions[i]
		key := " "
		if s.Key != "" {
			key = s.Key
		}
		mode := ""
		if s.Mode == action.ModeInteractive {
			mode = "  (terminal)"
		}
		line := cut(fmt.Sprintf("  %s  %s%s", key, s.Title(), mode), m.width-1)
		if i == m.actionIdx {
			line = titleSty.Render("> " + strings.TrimPrefix(line, "  "))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	if m.actionTTY {
		b.WriteString(noteSty.Render("next action runs with the terminal (i toggles)") + "\n")
	}
	pos := ""
	if len(m.actions) > room {
		pos = fmt.Sprintf(" (%d-%d of %d)", start+1, end, len(m.actions))
	}
	b.WriteString(helpSty.Render("key or enter run · ↑↓ choose · i run with the terminal · esc cancel" + pos))
	return b.String()
}

// logRun records a finished run: a summary in the audit trail, the details in
// the action log (when configured).
func (m *Model) logRun(t task.Task, rec actionlog.Record) error {
	if m.audit != nil {
		res := audit.ResultOK
		if rec.Result != actionlog.ResultOK {
			res = audit.ResultError
		}
		m.audit.Record(audit.Entry{Op: audit.OpRun, Backend: t.Backend, ID: t.ID, Result: res, Details: rec.Summary()})
	}
	return m.alog.Append(rec)
}

// Shutdown ends the background runs still going, whatever made the board quit
// (a key, SIGTERM, SIGHUP, an error): it kills them, waits up to wait for the
// processes to die and records each as canceled, so none is left running or
// missing from the audit trail and the action log. Call it after the program returned.
func (m Model) Shutdown(wait time.Duration) {
	if m.runs == nil {
		return
	}
	m.runs.cancelAll()
	for _, r := range m.runs.runs {
		if !r.done {
			wait = max(wait, r.job.Spec.Grace()+2*time.Second) // SIGTERM first, SIGKILL after the grace
		}
	}
	deadline := time.After(wait)
	for _, r := range m.runs.runs {
		if r.done {
			continue
		}
		select {
		case <-r.job.Done():
		case <-deadline:
			continue // would not die in time; nothing reliable to record
		}
		r.ended, r.done = m.clock(), true
		_ = m.logRun(r.job.Task, actionlog.FromJob(r.job, "tui"))
	}
}
