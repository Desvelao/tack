package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/board"
	"tack/internal/outcome"
	"tack/internal/task"
)

func withActions(t *testing.T, m Model, dir string, specs ...action.Spec) Model {
	t.Helper()
	if err := action.ValidateAll(specs); err != nil {
		t.Fatal(err)
	}
	return m.WithActions(specs, dir)
}

// pressCmd is press for one key that also returns the command the model asked for.
func pressCmd(m Model, k string) (Model, tea.Cmd) {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	}
	nm, cmd := m.Update(msg)
	return nm.(Model), cmd
}

func finish(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command returned")
	}
	nm, _ := m.Update(cmd())
	return nm.(Model)
}

func TestActionRunsInBackgroundAndConfirms(t *testing.T) {
	m, _ := newModel(t)
	l, _ := audit.New("")
	m = m.WithAudit(l)
	dir := t.TempDir()
	m = withActions(t, m, dir, action.Spec{Name: "note", Key: "n", Command: []string{"sh", "-c", `echo "$1:$TACK_TASK_ID" > out.txt; echo done-line`, "sh", "{title}"}})

	m = press(m, "x")
	if m.mode != modeActions || !strings.Contains(m.View(), "run an action on #1  Alpha") {
		t.Fatalf("mode %v\n%s", m.mode, m.View())
	}
	m, cmd := pressCmd(m, "n")
	if m.mode != modeBoard || !strings.Contains(m.notice, "started note on #1") {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
	m = finish(t, m, cmd)
	if m.noticeKind != noticeOK || m.notice != "ran note on #1 in <1s: done-line" {
		t.Fatalf("notice %q kind %v", m.notice, m.noticeKind)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || strings.TrimSpace(string(got)) != "Alpha:1" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
	var found bool
	es, _ := l.Entries()
	for _, e := range es {
		found = found || (e.Op == audit.OpRun && e.ID == "1" && e.Result == audit.ResultOK && e.Details == "action note <1s exit 0 out 10 B err 0 B")
	}
	if !found {
		t.Fatalf("no run entry in %+v", es)
	}
}

func TestActionFailureShowsFail(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), action.Spec{Name: "bad", Command: []string{"sh", "-c", "echo nope >&2; exit 2"}})
	m = press(m, "x")
	m, cmd := pressCmd(m, "enter")
	m = finish(t, m, cmd)
	if m.noticeKind != noticeFail || !strings.Contains(m.notice, "nope") {
		t.Fatalf("notice %q kind %v", m.notice, m.noticeKind)
	}
}

func TestActionPickerNavigationAndCancel(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(),
		action.Spec{Name: "a", Command: []string{"true"}},
		action.Spec{Name: "b", Command: []string{"true"}})
	m = press(m, "x", "j")
	if m.actionIdx != 1 {
		t.Fatalf("idx %d", m.actionIdx)
	}
	m = press(m, "k", "k")
	if m.actionIdx != 0 {
		t.Fatalf("idx %d", m.actionIdx)
	}
	m = press(m, "esc")
	if m.mode != modeBoard {
		t.Fatalf("mode %v", m.mode)
	}
	// from the detail view the picker returns there
	m = press(m, "enter", "x")
	if m.mode != modeActions || m.actionBack != modeDetail {
		t.Fatalf("mode %v back %v", m.mode, m.actionBack)
	}
	m = press(m, "esc")
	if m.mode != modeDetail {
		t.Fatalf("mode %v", m.mode)
	}
}

func TestActionWithoutConfigExplains(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "x")
	if m.mode != modeBoard || !strings.Contains(m.notice, "no actions configured") {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
}

func TestInteractiveActionSuspendsTheTUI(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(),
		action.Spec{Name: "agent", Key: "c", Mode: action.ModeInteractive, Command: []string{"true"}},
		action.Spec{Name: "bg", Key: "b", Command: []string{"true"}, Stdin: action.StdinJSON})
	m = press(m, "x")
	if _, cmd := pressCmd(m, "c"); cmd == nil {
		t.Fatal("interactive action returned no command")
	}
	// `i` forces the terminal for a background action; one that reads stdin cannot
	m = press(m, "i")
	m, cmd := pressCmd(m, "b")
	if cmd != nil || m.noticeKind != noticeFail || !strings.Contains(m.notice, "stdin") {
		t.Fatalf("cmd %v notice %q kind %v", cmd != nil, m.notice, m.noticeKind)
	}
}

func TestShutdownKillsAndRecordsRunningActions(t *testing.T) {
	m, _ := newModel(t)
	l, _ := audit.New("")
	m = m.WithAudit(l)
	dir := t.TempDir()
	alog, err := actionlog.New(actionlog.Spec{File: "actions.jsonl"}, dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	m = withActions(t, m, dir, action.Spec{Name: "slow", Command: []string{"sh", "-c", "sleep 60"}}).WithActionLog(alog)
	m = press(m, "x")
	m, _ = pressCmd(m, "enter")
	if len(m.runs.runs) != 1 || m.runs.runs[0].done {
		t.Fatalf("runs: %+v", m.runs.runs)
	}
	job := m.runs.runs[0].job
	m.Shutdown(5 * time.Second)
	select {
	case <-job.Done():
	default:
		t.Fatal("the process is still running after Shutdown")
	}
	if !m.runs.runs[0].done {
		t.Error("run not marked done")
	}
	recs, err := alog.Records(true)
	if err != nil || len(recs) != 1 || recs[0].Result != actionlog.ResultError {
		t.Fatalf("action log: %v %+v", err, recs)
	}
	var audited bool
	es, _ := l.Entries()
	for _, e := range es {
		audited = audited || (e.Op == audit.OpRun && e.Result == audit.ResultError)
	}
	if !audited {
		t.Fatalf("no audit entry in %+v", es)
	}
	m.Shutdown(time.Second) // a second call has nothing left to do
	if recs, _ := alog.Records(true); len(recs) != 1 {
		t.Fatalf("recorded twice: %d", len(recs))
	}
}

// Every reserved key must be handled by the picker, and no other
// letter or digit may be: the two lists cannot drift apart.
func TestReservedKeysMatchThePicker(t *testing.T) {
	for _, c := range "abcdefghijklmnopqrstuvwxyz0123456789" {
		key := string(c)
		m, _ := newModel(t)
		m = m.WithActions([]action.Spec{{Name: "a", Key: key, Mode: action.ModeBackground, Stdin: action.StdinNone, Command: []string{"true"}}}, t.TempDir()) // not validated: reserved keys are tried on purpose
		m = press(m, "x")
		before := len(m.runs.runs)
		next, _ := m.handleActionsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{c}})
		started := len(next.(Model).runs.runs) > before
		if reserved := strings.ContainsRune(action.ReservedKeys(), c); reserved == started {
			t.Errorf("key %q: reserved=%v but starting the action=%v", key, reserved, started)
		}
	}
}

func noteAction(dir string) action.Spec {
	return action.Spec{Name: "note", Key: "n", Command: []string{"sh", "-c", "echo done-line"}}
}

func TestDeleteConfirmationKeepsItsTarget(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "d") // asks about #1 Alpha
	if !strings.Contains(m.View(), "delete csv1:1") {
		t.Fatalf("view:\n%s", m.View())
	}
	m.selectID("csv1", "2") // the cursor ends up on another card while the question is open
	if !strings.Contains(m.View(), "delete csv1:1") {
		t.Fatalf("the question must stay about #1:\n%s", m.View())
	}
	m = press(m, "y")
	left, _ := be.List(context.Background())
	var ids []string
	for _, x := range left {
		ids = append(ids, x.ID)
	}
	if strings.Join(ids, ",") != "2,3" {
		t.Fatalf("deleted the wrong card, left %v", ids)
	}
}

func TestActionFinishingDuringAFormWaitsForTheUser(t *testing.T) {
	m, be := newModel(t)
	m = withActions(t, m, t.TempDir(), noteAction(""))
	m, cmd := startRun(t, m, "n")
	m = press(m, "d") // confirming a delete of #1 when the run ends
	// The command changed the backlog meanwhile: #1 is gone and #2 moved.
	if err := be.Delete(context.Background(), "csv1", "1"); err != nil {
		t.Fatal(err)
	}
	cards := len(m.tasks)
	m = finish(t, m, cmd)
	if m.mode != modeConfirmDelete || len(m.tasks) != cards {
		t.Fatalf("the board was reloaded under the confirmation: mode %v, %d tasks", m.mode, len(m.tasks))
	}
	if strings.Contains(m.notice, "ran note") {
		t.Fatalf("notice overwrote the open prompt: %q", m.notice)
	}
	if len(m.runs.runs) != 1 || !m.runs.runs[0].done {
		t.Fatal("the run itself must be recorded as finished")
	}
	m = press(m, "n") // answer no
	if m.mode != modeBoard || len(m.tasks) != cards-1 || !strings.Contains(m.notice, "ran note on #1") {
		t.Fatalf("after the form: mode %v, %d tasks, notice %q", m.mode, len(m.tasks), m.notice)
	}
}

func TestActionFinishingKeepsAnOpenEditFormText(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), noteAction(""))
	m, cmd := startRun(t, m, "n")
	m = press(m, "e")
	if m.mode != modeEdit {
		t.Fatal("no edit form")
	}
	m = finish(t, m, cmd)
	if m.mode != modeEdit || strings.Contains(m.notice, "ran note") {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
	m = press(m, "esc")
	if m.mode != modeBoard || !strings.Contains(m.notice, "ran note") {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
}

func TestUndoOfAMoveIsDroppedWhenAnActionRanOnThatCard(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), noteAction(""))
	m = press(m, "L") // moves #1; u would put the old copy back
	if len(m.undo) != 1 {
		t.Fatalf("undo stack %d", len(m.undo))
	}
	m, cmd := startRun(t, m, "n")
	m = finish(t, m, cmd)
	if len(m.undo) != 0 {
		t.Fatalf("the action may have changed the card; the undo must go: %+v", m.undo)
	}
	m = press(m, "u")
	if !strings.Contains(m.notice, "nothing to undo") {
		t.Fatalf("notice %q", m.notice)
	}
}

// A run that is still going is never evicted from the runs list, however many
// finished ones come after it, so its end is always recorded.
func TestRunStillGoingSurvivesEvictionAndIsRecorded(t *testing.T) {
	m, _ := newModel(t)
	l, _ := audit.New("")
	m = m.WithAudit(l)
	m = withActions(t, m, t.TempDir(), noteAction(""))
	m, cmd := startRun(t, m, "n")
	pending := m.runs.runs[0]
	for i := 0; i < maxRuns+20; i++ { // many runs that ended
		spec := action.Spec{Name: fmt.Sprintf("old%d", i), Command: []string{"true"}}
		spec.Normalize()
		j := spec.Start(context.Background(), task.Task{ID: "9", Title: "old"}, t.TempDir())
		<-j.Done()
		m.runs.add(j, time.Now()).done = true
	}
	if m.runs.get(pending.id) == nil {
		t.Fatal("the run still going was evicted")
	}
	m = finish(t, m, cmd)
	var recorded bool
	es, _ := l.Entries()
	for _, e := range es {
		recorded = recorded || (e.Op == audit.OpRun && e.ID == "1" && e.Result == audit.ResultOK)
	}
	if !recorded || !pending.done {
		t.Fatalf("not recorded: %+v", es)
	}
	if len(m.runs.runs) > maxRuns+1 {
		t.Errorf("%d runs kept", len(m.runs.runs))
	}
}

func TestIToggleRunsTheNextActionWithTheTerminal(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), noteAction(""))
	m = press(m, "x", "i")
	if !m.actionTTY || !strings.Contains(m.View(), "next action runs with the terminal") {
		t.Fatalf("toggle not shown:\n%s", m.View())
	}
	m, cmd := pressCmd(m, "n")
	if cmd == nil || len(m.runs.runs) != 0 || m.mode != modeBoard {
		t.Fatalf("a background job started instead of handing over the terminal: cmd=%v runs=%d mode=%v", cmd != nil, len(m.runs.runs), m.mode)
	}
	m = press(m, "x", "i", "i") // toggling twice cancels it
	if m.actionTTY {
		t.Fatal("second i must switch it off")
	}
}

// What the ExecProcess callback delivers when an interactive action ends.
func TestInteractiveActionEndIsAuditedLoggedAndReported(t *testing.T) {
	m, _ := newModel(t)
	l, _ := audit.New("")
	m = m.WithAudit(l)
	dir := t.TempDir()
	alog, err := actionlog.New(actionlog.Spec{File: "actions.jsonl"}, dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	spec := action.Spec{Name: "agent", Mode: action.ModeInteractive, Command: []string{"claude"}}
	spec.Normalize()
	m = withActions(t, m, dir, spec).WithActionLog(alog)
	tk, _ := m.selected()
	plan := spec.Expand(tk, dir)
	started := time.Now().Add(-3 * time.Second)

	next, _ := m.Update(actionDoneMsg{name: "agent", task: tk, spec: spec, plan: plan, started: started})
	m = next.(Model)
	if m.noticeKind != noticeOK || !strings.HasPrefix(m.notice, "ran agent on #1") {
		t.Fatalf("notice %q kind %v", m.notice, m.noticeKind)
	}
	next, _ = m.Update(actionDoneMsg{name: "agent", task: tk, spec: spec, plan: plan, started: started, err: errors.New("agent: exit status 2")})
	m = next.(Model)
	if m.noticeKind != noticeFail || !strings.Contains(m.notice, "exit status 2") {
		t.Fatalf("notice %q kind %v", m.notice, m.noticeKind)
	}
	recs, err := alog.Records(true)
	if err != nil || len(recs) != 2 || recs[0].Mode != action.ModeInteractive || recs[0].Result != actionlog.ResultOK || recs[1].Result != actionlog.ResultError {
		t.Fatalf("records %v %+v", err, recs)
	}
	var runs int
	es, _ := l.Entries()
	for _, e := range es {
		if e.Op == audit.OpRun {
			runs++
		}
	}
	if runs != 2 {
		t.Fatalf("%d audit entries for runs", runs)
	}
}

// runAll delivers what cmd produces to the model, and what the model asks for
// in turn, until nothing is left; check sees the model after every message.
func runAll(t *testing.T, m Model, cmd tea.Cmd, check func(Model)) Model {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case actionDoneMsg:
			nm, next := m.Update(msg)
			m = nm.(Model)
			queue = append(queue, next)
			if check != nil {
				check(m)
			}
		}
	}
	return m
}

func running(m Model) int {
	n := 0
	for _, r := range m.runs.runs {
		if !r.done {
			n++
		}
	}
	return n
}

func TestMarksSurviveFilterAndReloadAndDropWithTheCard(t *testing.T) {
	m, be := newModel(t)
	m = press(m, " ", "j", " ") // marks #1 and #2
	if len(m.marks) != 2 || !strings.Contains(m.notice, "2 marked") {
		t.Fatalf("marks %v notice %q", m.marks, m.notice)
	}
	if v := m.View(); !strings.Contains(v, "2 marked") || strings.Count(v, markSign) != 2 {
		t.Fatalf("marks not drawn:\n%s", v)
	}
	m = press(m, "/")
	m = typeText(m, "Beta")
	m = press(m, "enter") // #1 is hidden but stays marked
	if len(m.marks) != 2 || strings.Count(m.View(), markSign) != 1 {
		t.Fatalf("filter changed the marks: %v\n%s", m.marks, m.View())
	}
	m = press(m, "esc") // clears the filter only
	if m.filter != "" || len(m.marks) != 2 {
		t.Fatalf("filter %q marks %v", m.filter, m.marks)
	}
	m = press(m, "r")
	if len(m.marks) != 2 {
		t.Fatalf("reload dropped marks: %v", m.marks)
	}
	if err := be.Delete(context.Background(), "csv1", "2"); err != nil {
		t.Fatal(err)
	}
	m = press(m, "r")
	if len(m.marks) != 1 || !m.marks[cardKey{"csv1", "1"}] {
		t.Fatalf("the deleted card must lose its mark: %v", m.marks)
	}
	m = press(m, "g", " ") // unmark #1
	if len(m.marks) != 0 {
		t.Fatalf("toggle: %v", m.marks)
	}
	m = press(m, " ", "esc") // esc with no filter unmarks all
	if len(m.marks) != 0 || m.notice != "marks cleared" {
		t.Fatalf("esc: %v %q", m.marks, m.notice)
	}
}

func TestBatchRespectsTheLimitAndSumsUpOnce(t *testing.T) {
	m, _ := newModel(t)
	l, _ := audit.New("")
	m = m.WithAudit(l)
	m = withActions(t, m, t.TempDir(), noteAction("")).WithActionLimit(2)
	m = press(m, " ", "j", " ", "3", " ") // #1, #2 (pending) and #3 (done)
	m = press(m, "x")
	if !strings.Contains(m.View(), "3 marked cards") {
		t.Fatalf("picker title:\n%s", m.View())
	}
	m, cmd := pressCmd(m, "n")
	if running(m) != 2 || m.batch == nil || len(m.batch.queue) != 1 || len(m.marks) != 0 {
		t.Fatalf("running %d, batch %+v, marks %v", running(m), m.batch, m.marks)
	}
	if !strings.Contains(m.notice, "started note on 3 cards, 2 at a time") {
		t.Fatalf("notice %q", m.notice)
	}
	peak, notices := 0, 0
	m = runAll(t, m, cmd, func(m Model) {
		peak = max(peak, running(m))
		if strings.Contains(m.notice, "ran note") {
			notices++
		}
	})
	if peak > 2 {
		t.Fatalf("%d runs at once, limit 2", peak)
	}
	if m.batch != nil || len(m.runs.runs) != 3 || notices != 1 {
		t.Fatalf("batch %+v, %d runs, %d notices", m.batch, len(m.runs.runs), notices)
	}
	if m.noticeKind != noticeOK || m.notice != "ran note on 3 cards: 3 ok" {
		t.Fatalf("summary %q kind %v", m.notice, m.noticeKind)
	}
	ids := map[string]bool{}
	es, _ := l.Entries()
	for _, e := range es {
		if e.Op == audit.OpRun && e.Result == audit.ResultOK {
			ids[e.ID] = true
		}
	}
	if len(ids) != 3 {
		t.Fatalf("audited runs: %v", ids)
	}
	if m.lastAction.Name != "note" {
		t.Fatalf("last action %q", m.lastAction.Name)
	}
}

func TestBatchSkipsACardThatDisappearedBeforeItsTurn(t *testing.T) {
	m, be := newModel(t)
	m = withActions(t, m, t.TempDir(), noteAction("")).WithActionLimit(1)
	m = press(m, " ", "j", " ", "3", " ")
	m = press(m, "x")
	m, cmd := pressCmd(m, "n")
	if running(m) != 1 {
		t.Fatalf("%d running, limit 1", running(m))
	}
	if err := be.Delete(context.Background(), "csv1", "3"); err != nil { // gone before its turn
		t.Fatal(err)
	}
	m = runAll(t, m, cmd, nil)
	if len(m.runs.runs) != 2 || m.batch != nil {
		t.Fatalf("%d runs, batch %+v", len(m.runs.runs), m.batch)
	}
	if m.noticeKind != noticeFail || !strings.Contains(m.notice, "ran note on 2 of 3 cards: 2 ok, 1 gone (#3)") {
		t.Fatalf("summary %q kind %v", m.notice, m.noticeKind)
	}
}

func TestBatchCountsFailuresAndRefusesTerminalActions(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(),
		action.Spec{Name: "bad", Key: "b", Command: []string{"sh", "-c", `[ "$TACK_TASK_ID" != 2 ]`}},
		action.Spec{Name: "agent", Key: "a", Mode: action.ModeInteractive, Command: []string{"true"}})
	m = press(m, " ", "j", " ", "x")
	m, cmd := pressCmd(m, "a")
	if cmd != nil || m.noticeKind != noticeFail || !strings.Contains(m.notice, "runs with the terminal") || len(m.marks) != 2 {
		t.Fatalf("interactive on marks: cmd %v notice %q marks %v", cmd != nil, m.notice, m.marks)
	}
	m = press(m, "x")
	m, cmd = pressCmd(m, "b")
	m = runAll(t, m, cmd, nil)
	if m.noticeKind != noticeFail || !strings.Contains(m.notice, "ran bad on 2 cards: 1 ok, 1 failed (#2)") {
		t.Fatalf("summary %q kind %v", m.notice, m.noticeKind)
	}
}

func TestDotRepeatsTheLastActionOnTheSelectedCard(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(),
		action.Spec{Name: "first", Key: "f", Command: []string{"true"}},
		noteAction(""))
	m = press(m, ".")
	if !strings.Contains(m.notice, "no action run yet") {
		t.Fatalf("notice %q", m.notice)
	}
	m, cmd := startRun(t, m, "n")
	m = finish(t, m, cmd)
	m = press(m, "j")
	m, cmd = pressCmd(m, ".")
	if cmd == nil || m.mode != modeBoard || !strings.Contains(m.notice, "started note on #2") {
		t.Fatalf("repeat: cmd %v mode %v notice %q", cmd != nil, m.mode, m.notice)
	}
	m = finish(t, m, cmd)
	// from the card details too, returning there; `i` is remembered
	m = press(m, "enter", "x", "i")
	m, cmd = pressCmd(m, "f")
	if cmd == nil || !m.lastTTY || m.mode != modeDetail {
		t.Fatalf("tty run: cmd %v lastTTY %v mode %v", cmd != nil, m.lastTTY, m.mode)
	}
	m, cmd = pressCmd(m, ".")
	if cmd == nil || len(m.runs.runs) != 2 || m.mode != modeDetail {
		t.Fatalf("repeat with the terminal must not start a background run: %d runs, mode %v", len(m.runs.runs), m.mode)
	}
}

// A card whose blocked_by is not finished says so on the card, in the card
// details and in the action picker (which still runs it: a human decides).
func TestBlockedCardIsShownAndWarnedInThePicker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	// A priority fills the meta line: the blocker must still show.
	data := "id,title,status,priority,blocked_by\n1,Alpha,pending,medium,\"2,3\"\n2,Beta,pending,,\n3,Gamma,done,,\n4,Delta,pending,,3\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = withActions(t, nm.(Model), t.TempDir(), action.Spec{Name: "note", Key: "n", Command: []string{"true"}})
	if v := m.View(); !strings.Contains(v, "blocked by #2") || strings.Contains(v, "blocked by #3") {
		t.Fatalf("card lacks its open blocker (or shows a finished one):\n%s", v)
	}
	if d := press(m, "enter").View(); !strings.Contains(d, "blocked by #2, #3  (open: #2)") {
		t.Fatalf("details:\n%s", d)
	}
	m = press(m, "x")
	if v := m.View(); m.mode != modeActions || !strings.Contains(v, "blocked by #2: not ready") {
		t.Fatalf("picker lacks the warning:\n%s", v)
	}
	m = press(press(m, "esc"), "j") // Beta: not blocked
	if v := press(m, "x").View(); strings.Contains(v, "not ready") {
		t.Fatalf("warning on a ready card:\n%s", v)
	}
	m = press(press(m, " "), "k")
	m = press(press(m, " "), "x") // Alpha and Beta marked
	if v := m.View(); !strings.Contains(v, "1 of the marked cards are blocked") {
		t.Fatalf("batch picker lacks the warning:\n%s", v)
	}
}

// The picker lists the actions of the card's backend: its own replace the
// top-level ones of the same name; with marked cards of several backends only
// the actions all of them have.
func TestPickerShowsBackendActions(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, f := range []string{"a.csv", "b.csv"} {
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, []byte("id,title,status\n1,"+f+",pending\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	m := New(openSet(t, paths...), "t", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)
	top := []action.Spec{{Name: "show", Label: "top show", Command: []string{"true"}}}
	own := []action.Spec{{Name: "show", Label: "csv2 show", Command: []string{"true"}}, {Name: "only", Label: "csv2 only", Command: []string{"true"}}}
	if err := action.ValidateAll(top); err != nil {
		t.Fatal(err)
	}
	if err := action.ValidateFor(own, "csv2"); err != nil {
		t.Fatal(err)
	}
	m = m.WithActions(append(top, own...), dir)
	v := press(m, "x").View()
	if !strings.Contains(v, "top show") || strings.Contains(v, "csv2") {
		t.Fatalf("csv1 card:\n%s", v)
	}
	v = press(m, "j", "x").View()
	if !strings.Contains(v, "csv2 show") || !strings.Contains(v, "csv2 only") || strings.Contains(v, "top show") {
		t.Fatalf("csv2 card:\n%s", v)
	}
	v = press(m, " ", "j", " ", "x").View()
	if !strings.Contains(v, "show") || strings.Contains(v, "csv2 only") {
		t.Fatalf("both marked:\n%s", v)
	}
}

func TestClaimActionClaimsTheCardForTheRun(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status,assignee,lease\n1,Alpha,pending,,\n2,Beta,pending,zed,\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	be := openSet(t, p)
	nm, _ := New(be, "t.csv", board.DefaultSpec()).Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m := nm.(Model)
	m.now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
	t.Setenv("TACK_AGENT", "")
	dir := t.TempDir()
	m = withActions(t, m, dir, action.Spec{Name: "look", Key: "l", Claim: true, Command: []string{"sh", "-c", "cat " + p + " > seen.csv"}})

	m, cmd := pressCmd(press(m, "x"), "l")
	m = finish(t, m, cmd)
	if m.noticeKind != noticeOK {
		t.Fatalf("notice %q", m.notice)
	}
	seen, _ := os.ReadFile(filepath.Join(dir, "seen.csv"))
	if !strings.Contains(string(seen), "1,Alpha,pending,look,2026-10-18T12:00:15Z") {
		t.Fatalf("not claimed during the run:\n%s", seen)
	}
	if after, _ := os.ReadFile(p); !strings.Contains(string(after), "1,Alpha,pending,,,\n") {
		t.Fatalf("not released after the run:\n%s", after)
	}
	// a card somebody else holds does not start
	m = press(m, "j")
	m, cmd = pressCmd(press(m, "x"), "l")
	if cmd != nil || m.noticeKind != noticeFail || !strings.Contains(m.notice, "claimed by zed") {
		t.Fatalf("claimed card: cmd %v notice %q", cmd != nil, m.notice)
	}
}

func TestActionCapturesOutputAndMovesTheCard(t *testing.T) {
	m, be := newModel(t)
	m = withActions(t, m, t.TempDir(),
		action.Spec{Name: "ship", Capture: action.CaptureNotes, OnSuccess: action.Outcome{Status: "Done"}, Command: []string{"sh", "-c", `echo "id=$TACK_RUN_ID"`}},
		action.Spec{Name: "bad", OnFailure: action.Outcome{Status: "pending"}, Command: []string{"sh", "-c", "exit 3"}})
	m = press(m, "x")
	m, cmd := pressCmd(m, "enter") // first action: ship
	m = finish(t, m, cmd)
	got, err := be.Find(context.Background(), "", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || !strings.Contains(got.Notes, "[run ") || !strings.Contains(got.Notes, "id=") {
		t.Fatalf("%+v", got)
	}
}

func TestOutcomeLeavesCanceledRunsAlone(t *testing.T) {
	m, be := newModel(t)
	tk, _ := be.Find(context.Background(), "", "1")
	s := action.Spec{Name: "x", OnSuccess: action.Outcome{Status: "Done"}, Capture: action.CaptureNotes}
	if after, err := outcome.Apply(context.Background(), be, m.board, s, tk, actionlog.Record{Result: actionlog.ResultOK, Stdout: "hi"}, true, ""); after != "" || err != nil {
		t.Fatalf("%q %v", after, err)
	}
	if now, _ := be.Find(context.Background(), "", "1"); now.Status != tk.Status || now.Notes != tk.Notes {
		t.Fatalf("canceled run changed the task: %+v", now)
	}
}

func TestPickerWorkdirPrompt(t *testing.T) {
	m, _ := newModel(t)
	m = m.WithActions([]action.Spec{{Name: "a", Key: "a", Mode: action.ModeBackground, Stdin: action.StdinNone, Command: []string{"true"}}}, t.TempDir())
	m = press(m, "x", "w")
	m = typeText(m, "/srv/x")
	if m.actionWDIn == nil || !strings.Contains(m.View(), "workdir: /srv/x") {
		t.Fatalf("prompt not shown:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.actionWD != "/srv/x" || m.actionWDIn != nil || !strings.Contains(m.View(), "workdir: /srv/x (w changes)") {
		t.Fatalf("workdir = %q\n%s", m.actionWD, m.View())
	}
	m = press(m, "w", "esc") // cancel keeps it
	if m.actionWD != "/srv/x" || m.mode != modeActions {
		t.Fatalf("esc: %q mode %v", m.actionWD, m.mode)
	}
	m = press(m, "enter")
	if m.lastWD != "/srv/x" {
		t.Errorf("lastWD = %q", m.lastWD)
	}
	m = press(m, "x")
	if m.actionWD != "" {
		t.Errorf("override survived reopening: %q", m.actionWD)
	}
}
