package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/board"
)

// clocked gives the model a clock the test moves by hand.
func clocked(m Model) (Model, *time.Time) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return m, &now
}

// startRun runs the action named by key on the selected card and returns the wait command.
func startRun(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	m = press(m, "x")
	m, cmd := pressCmd(m, key)
	if cmd == nil {
		t.Fatal("no command: action did not start")
	}
	return m, cmd
}

func slowSpec(show string) action.Spec {
	return action.Spec{Name: "slow", Key: "s", Show: show, Command: []string{"sh", "-c", "echo first; sleep 30"}}
}

func waitOutput(t *testing.T, m Model, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range m.runs.newest() {
			if strings.Contains(r.job.Snapshot().Stdout, want) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %q in output", want)
}

func TestStripShowsRunningThenResultThenFades(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), action.Spec{Name: "quick", Key: "a", Command: []string{"sh", "-c", "echo hi"}}, slowSpec(""))
	m, _ = clocked(m)

	// a run that ends: strip shows OK for resultLifetime, then nothing
	m, cmd := startRun(t, m, "a")
	if !strings.Contains(m.View(), "⟳ running quick #1 Alpha") {
		t.Fatalf("no running line:\n%s", m.View())
	}
	m = finish(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "OK") || !strings.Contains(v, "quick #1 Alpha") || strings.Contains(v, "⟳") {
		t.Fatalf("no result line:\n%s", v)
	}
	clock := m.clock()
	m.now = func() time.Time { return clock.Add(resultLifetime - time.Second) }
	if !strings.Contains(m.View(), "quick #1 Alpha") {
		t.Fatal("result vanished too early")
	}
	m.now = func() time.Time { return clock.Add(resultLifetime + time.Second) }
	if strings.Contains(m.View(), "quick #1 Alpha") {
		t.Fatal("result still shown after the lifetime")
	}
}

func TestStripCountsElapsedTimeAndKeepsBoardWithinScreen(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), slowSpec(""))
	m, now := clocked(m)
	m, _ = startRun(t, m, "s")
	t.Cleanup(m.runs.cancelAll)
	*now = now.Add(72 * time.Second)
	v := m.View()
	if !strings.Contains(v, "⟳ running slow #1 Alpha") || !strings.Contains(v, "1m12s") {
		t.Fatalf("elapsed missing:\n%s", v)
	}
	m.height = 24
	if lines := strings.Count(m.View(), "\n") + 1; lines > 24 {
		t.Fatalf("view has %d lines on a 24-line terminal", lines)
	}
}

func TestRunsViewOutputLiveAndCancel(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), slowSpec(""))
	m, cmd := startRun(t, m, "s")
	t.Cleanup(m.runs.cancelAll)
	waitOutput(t, m, "first")

	m = press(m, "R")
	if m.mode != modeRuns || !strings.Contains(m.View(), "running") || !strings.Contains(m.View(), "slow csv1 #1 Alpha") {
		t.Fatalf("runs view:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.mode != modeRunOutput || !strings.Contains(m.View(), "first") || !strings.Contains(m.View(), "running") {
		t.Fatalf("output view:\n%s", m.View())
	}
	m = press(m, "esc")
	if m.mode != modeRuns {
		t.Fatalf("mode %v", m.mode)
	}
	m = press(m, "c")
	m = finish(t, m, cmd) // the cancelled command ends and reports
	if m.noticeKind != noticeFail || !strings.Contains(m.notice, "canceled") {
		t.Fatalf("notice %q kind %v", m.notice, m.noticeKind)
	}
	m = press(m, "c")
	if m.notice != "not running" {
		t.Fatalf("notice %q", m.notice)
	}
	m = press(m, "esc")
	if m.mode != modeBoard {
		t.Fatalf("mode %v", m.mode)
	}
}

func TestRunOutputShowsStderrSectionAndScrolls(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), action.Spec{Name: "chatty", Key: "c", Command: []string{"sh", "-c", "seq 1 200; echo oops >&2"}})
	m, cmd := startRun(t, m, "c")
	m = finish(t, m, cmd)
	m = press(m, "R", "enter")
	v := m.View()
	if !strings.Contains(v, "oops") || !strings.Contains(v, "--- stderr ---") || !strings.Contains(v, "OK") {
		t.Fatalf("not at the end:\n%s", v)
	}
	m = press(m, "g")
	if v = m.View(); !strings.Contains(v, "--- command") || strings.Contains(v, "--- stderr ---") || !strings.Contains(v, "scroll") {
		t.Fatalf("not at the top:\n%s", v)
	}
	m = press(m, "G")
	if !strings.Contains(m.View(), "oops") {
		t.Fatal("G did not go to the end")
	}
}

func TestShowOutputOpensOnFinishUnlessTyping(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), action.Spec{Name: "show", Key: "s", Show: action.ShowOutput, Command: []string{"echo", "result-line"}})
	m, cmd := startRun(t, m, "s")
	m = finish(t, m, cmd)
	if m.mode != modeRunOutput || !strings.Contains(m.View(), "result-line") {
		t.Fatalf("mode %v\n%s", m.mode, m.View())
	}
	if m = press(m, "esc"); m.mode != modeBoard {
		t.Fatalf("esc went to mode %v, want the board", m.mode)
	}

	// while typing a new task the output must not take over
	m, cmd = startRun(t, m, "s")
	m = press(m, "n")
	if m.mode != modeNew {
		t.Fatalf("mode %v", m.mode)
	}
	m = finish(t, m, cmd)
	if m.mode != modeNew {
		t.Fatalf("output view interrupted typing: mode %v", m.mode)
	}
}

func TestRunsViewWithoutRunsAndQuitCancels(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), slowSpec(""))
	if m = press(m, "R"); m.mode != modeBoard || !strings.Contains(m.notice, "no background action") {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
	m, _ = startRun(t, m, "s")
	r := m.runs.newest()[0]
	m = press(m, "q")
	select {
	case <-r.job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("quit did not cancel the running command")
	}
}

func TestOutputViewShowsCommandAndStdinAsInput(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), action.Spec{Name: "feed", Key: "f", Stdin: action.StdinMarkdown, Command: []string{"cat"}})
	m, cmd := startRun(t, m, "f")
	m = finish(t, m, cmd)
	m = press(m, "R", "enter")
	v := m.View()
	for _, want := range []string{"--- command", "cat", "--- stdin ---", "# Alpha", "--- stdout ---", "exit 0"} {
		if !strings.Contains(v, want) {
			t.Errorf("output view lacks %q:\n%s", want, v)
		}
	}
}

func TestRunIsStoredInTheActionLogAndListedLater(t *testing.T) {
	dir := t.TempDir()
	newBoard := func() (Model, *actionlog.Log) {
		m, _ := newModel(t)
		l, err := actionlog.New(actionlog.Spec{File: "actions.jsonl"}, dir, "sess-"+t.Name()+fmtNow())
		if err != nil {
			t.Fatal(err)
		}
		m = withActions(t, m, dir, action.Spec{Name: "note", Key: "n", Env: map[string]string{"TOKEN": "s3cret"}, Command: []string{"sh", "-c", "echo stored-line"}})
		return m.WithActionLog(l), l
	}

	m, l := newBoard()
	m, cmd := startRun(t, m, "n")
	m = finish(t, m, cmd)
	recs, err := l.Records(true)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records %v %v", recs, err)
	}
	r := recs[0]
	if r.Action != "note" || r.ID != "1" || r.Source != "tui" || r.Stdout != "stored-line\n" || strings.Join(r.Command, " ") != "sh -c echo stored-line" || r.Result != "ok" {
		t.Fatalf("%+v", r)
	}
	if data, _ := os.ReadFile(l.Path()); strings.Contains(string(data), "s3cret") {
		t.Fatal("env value stored")
	}

	// a later session (new model, new session id) lists it under "earlier sessions" and can open it
	m2, _ := newBoard()
	m2 = press(m2, "R")
	v := m2.View()
	if m2.mode != modeRuns || !strings.Contains(v, "earlier sessions") || !strings.Contains(v, "note csv1 #1 Alpha") {
		t.Fatalf("earlier run not listed (mode %v):\n%s", m2.mode, v)
	}
	m2 = press(m2, "enter")
	if v = m2.View(); !strings.Contains(v, "stored-line") || !strings.Contains(v, "--- command") {
		t.Fatalf("stored output:\n%s", v)
	}
	// the runs filter narrows the list
	m2 = press(m2, "esc", "/")
	for _, ch := range "result=error" {
		m2 = press(m2, string(ch))
	}
	m2 = press(m2, "enter")
	if v = m2.View(); !strings.Contains(v, "nothing matches") {
		t.Fatalf("filter:\n%s", v)
	}
	m2 = press(m2, "esc")
	if m2.runFilter != "" || !strings.Contains(m2.View(), "note csv1") {
		t.Fatalf("esc must clear the filter first:\n%s", m2.View())
	}
}

func TestRunsViewOpensWithOnlyStoredRuns(t *testing.T) {
	dir := t.TempDir()
	l, _ := actionlog.New(actionlog.Spec{Dir: "runs"}, dir, "now")
	old, _ := actionlog.New(actionlog.Spec{Dir: "runs"}, dir, "before")
	if err := old.Append(actionlog.Record{Time: time.Now().Add(-time.Hour), Action: "x", ID: "9", Backend: "csv1", Result: "ok", Command: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	m, _ := newModel(t)
	m = withActions(t, m, dir, action.Spec{Name: "x", Command: []string{"true"}}).WithActionLog(l)
	if m = press(m, "R"); m.mode != modeRuns || !strings.Contains(m.View(), "x csv1 #9") {
		t.Fatalf("mode %v\n%s", m.mode, m.View())
	}
}

func fmtNow() string { return fmt.Sprint(time.Now().UnixNano()) }

func smallScreen(m Model) Model {
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return nm.(Model)
}

func TestRunsViewScrollsAt80x24(t *testing.T) {
	m, _ := newModel(t)
	m = smallScreen(m)
	m.runs = &runLog{}
	now := time.Now()
	for i := 0; i < 60; i++ { // earlier sessions, as read from the action log
		m.runStored = append(m.runStored, actionlog.Record{Time: now.Add(-time.Duration(i) * time.Minute), Action: "act", Backend: "csv1",
			ID: fmt.Sprint(i + 1), Title: fmt.Sprintf("task %d", i+1), Result: actionlog.ResultOK, Command: []string{"x"}})
	}
	m.mode, m.runBack = modeRuns, modeBoard
	check := func(step string, wantRow string) {
		t.Helper()
		v := m.View()
		if n := strings.Count(v, "\n") + 1; n > 24 {
			t.Fatalf("%s: %d lines on a 24 line screen:\n%s", step, n, v)
		}
		if !strings.Contains(v, "tack runs") {
			t.Fatalf("%s: header scrolled off:\n%s", step, v)
		}
		if !selectedLineHas(v, wantRow) {
			t.Fatalf("%s: selected row %q not visible:\n%s", step, wantRow, v)
		}
		if !strings.Contains(v, "of 60)") {
			t.Fatalf("%s: no position in the footer:\n%s", step, v)
		}
	}
	check("top", "#1 task 1")
	for i := 0; i < 40; i++ {
		m = press(m, "down")
	}
	check("40 down", "#41 task 41")
	m = press(m, "end")
	check("end", "#60 task 60")
	m = press(m, "pgup")
	if m.runIdx >= 59 || m.runIdx < 30 {
		t.Fatalf("pgup moved to %d", m.runIdx)
	}
	check("pgup", fmt.Sprintf("#%d task %d", m.runIdx+1, m.runIdx+1))
	m = press(m, "home")
	check("home", "#1 task 1")
}

func TestActionPickerScrollsAt80x24(t *testing.T) {
	m, _ := newModel(t)
	m = smallScreen(m)
	var specs []action.Spec
	for i := 0; i < 40; i++ {
		specs = append(specs, action.Spec{Name: fmt.Sprintf("act%02d", i), Command: []string{"true"}})
	}
	m = withActions(t, m, t.TempDir(), specs...)
	m = press(m, "x")
	for i := 0; i < 30; i++ {
		m = press(m, "down")
	}
	v := m.View()
	if n := strings.Count(v, "\n") + 1; n > 24 {
		t.Fatalf("%d lines on a 24 line screen:\n%s", n, v)
	}
	if !strings.Contains(v, "run an action on") || !selectedLineHas(v, "act30") || !strings.Contains(v, "of 40)") {
		t.Fatalf("picker:\n%s", v)
	}
}

// selectedLineHas reports whether the highlighted line ("> ...") of a view contains want.
func selectedLineHas(view, want string) bool {
	for _, l := range strings.Split(view, "\n") {
		if strings.HasPrefix(l, "> ") && strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func TestSanitizeStripsEscapesAndControls(t *testing.T) {
	cases := map[string]string{
		"\x1b[31mred\x1b[0m text":            "red text",
		"\x1b[2J\x1b[Hscreen":                "screen",
		"\x1b]0;title\x07after":              "after",
		"10%\r50%\r100%\ndone":               "100%\ndone",
		"line\r\nnext\r\n":                   "line\nnext\n",
		"a\tb":                               "a b",
		"bell\x07 nul\x00 del\x7f c1\u0085.": "bell nul del c1.",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCutCountsCellsNotRunes(t *testing.T) {
	for _, s := range []string{"日本語のタイトルがとても長い", "emoji 😀😀😀😀😀😀😀😀", "ééééééééé", "plain ascii text that is long"} {
		for _, w := range []int{1, 5, 10, 19} {
			if got := cut(s, w); lipgloss.Width(got) > w {
				t.Errorf("cut(%q, %d) = %q is %d cells", s, w, got, lipgloss.Width(got))
			}
		}
	}
	if got := cut("anything", 0); got != "" {
		t.Errorf("cut with no room = %q", got)
	}
	if got := cut("first\nsecond", 20); got != "first" {
		t.Errorf("multi-line = %q", got)
	}
}

func TestActionOutputAndWideTitlesStayInsideTheScreen(t *testing.T) {
	m, _ := newModel(t)
	m = smallScreen(m)
	dir := t.TempDir()
	script := `printf '\033[31mred\033[0m\r\033[2Kprogress 50%%\rprogress 100%%\n'; printf 'wide: 日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語\n'; printf 'bad: \033[?1049h\n' >&2; exit 4`
	m = withActions(t, m, dir, action.Spec{Name: "noisy", Key: "n", Command: []string{"sh", "-c", script}})
	m, cmd := pressCmd(m, "x")
	m, cmd = pressCmd(m, "n")
	m = finish(t, m, cmd)
	if m.noticeKind != noticeFail {
		t.Fatalf("notice %q", m.notice)
	}
	check := func(step string) {
		t.Helper()
		v := m.View()
		if strings.ContainsAny(v, "\r\x07") || strings.Contains(v, "\x1b[?1049h") || strings.Contains(v, "\x1b[2J") {
			t.Fatalf("%s: raw escape from the command reached the screen: %q", step, v)
		}
		for i, l := range strings.Split(v, "\n") {
			if w := lipgloss.Width(l); w > 80 {
				t.Errorf("%s: line %d is %d cells: %q", step, i, w, l)
			}
		}
		if n := strings.Count(v, "\n") + 1; n > 24 {
			t.Errorf("%s: %d lines", step, n)
		}
	}
	check("board with the FAIL notice")
	m = press(m, "R")
	check("runs list")
	m = press(m, "enter")
	check("output")
	v := m.View()
	_, out, _ := strings.Cut(v, "--- stdout ---") // the command line above shows the script as written
	if !strings.Contains(out, "progress 100%") || strings.Contains(out, "progress 50%") || strings.Contains(v, "\x1b") {
		t.Errorf("output not cleaned:\n%s", v)
	}
}

func TestBoardFitsAt80x24WithStripNoticeAndFooter(t *testing.T) {
	m, _ := newModel(t)
	m = smallScreen(m)
	m, _ = clocked(m)
	m = withActions(t, m, t.TempDir(), slowSpec(""))
	m, _ = startRun(t, m, "s")
	for i := 0; i < 2; i++ { // three runs in the strip: the same action again needs a confirmation
		m = press(m, "x", "s")
		if m, _ = pressCmd(m, "."); len(m.runs.runs) != i+2 {
			t.Fatalf("run %d did not start", i+2)
		}
	}
	t.Cleanup(m.runs.cancelAll)
	m.fail("error: something went wrong with a rather long explanation that goes on and on past the edge of the screen")
	for _, step := range []struct {
		name string
		keys []string
	}{{"board", nil}, {"filter prompt", []string{"/"}}, {"new prompt", []string{"esc", "n"}}} {
		m = press(m, step.keys...)
		v := m.View()
		if h := lipgloss.Height(v); h > 24 {
			t.Fatalf("%s: %d lines on a 24 line screen:\n%s", step.name, h, v)
		}
		for i, l := range strings.Split(v, "\n") {
			if w := lipgloss.Width(l); w > 80 {
				t.Errorf("%s: line %d is %d cells: %q", step.name, i, w, l)
			}
		}
		if !strings.Contains(v, "tack") {
			t.Fatalf("%s: the title scrolled off:\n%s", step.name, v)
		}
	}
	m = press(m, "esc")
	m.clearNotice()
	v := m.View()
	if !strings.Contains(v, "? help") || !strings.Contains(v, "q quit") || lipgloss.Height(v) > 24 {
		t.Fatalf("footer:\n%s", v)
	}
	for _, w := range []int{40, 60, 80, 120} {
		if got := boardHelp(w - 1); lipgloss.Width(got) > w-1 || !strings.Contains(got, "? help") && w >= 60 {
			t.Errorf("width %d: %q", w, got)
		}
	}
}

func TestRunsPromptHelpListsFieldsWithMeanings(t *testing.T) {
	m, _ := newModel(t)
	m = smallScreen(m)
	m.runs = &runLog{}
	m.runStored = []actionlog.Record{{Time: time.Now(), Action: "a", ID: "1", Result: actionlog.ResultOK, Command: []string{"x"}}}
	m.mode, m.runBack = modeRuns, modeBoard
	m = press(m, "/", "?", "tab")
	v := m.View()
	for _, f := range actionlog.FieldDocs {
		if !strings.Contains(v, f.Name) || !strings.Contains(v, f.Desc) {
			t.Errorf("field %q or its meaning missing:\n%s", f.Name, v)
		}
	}
	if !strings.Contains(v, "Action run fields") || strings.Count(v, "\n") > 24 {
		t.Errorf("fields page:\n%s", v)
	}
	m = press(m, "esc")
	if m.mode != modeRuns || !m.runEditing {
		t.Fatalf("mode %v editing %v", m.mode, m.runEditing)
	}
}

func TestRunsLoadsNewestStoredAndALoadsAll(t *testing.T) {
	m, _ := newModel(t)
	m = smallScreen(m)
	m.runs = &runLog{}
	dir := t.TempDir()
	old, err := actionlog.New(actionlog.Spec{File: "a.jsonl"}, dir, "old")
	if err != nil {
		t.Fatal(err)
	}
	l, _ := actionlog.New(actionlog.Spec{File: "a.jsonl"}, dir, "me")
	base := time.Now().Add(-time.Hour)
	for i := 0; i < runsLoad+5; i++ {
		if err := old.Append(actionlog.Record{Time: base.Add(time.Duration(i) * time.Second), Action: "a", ID: fmt.Sprint(i), Result: actionlog.ResultOK}); err != nil {
			t.Fatal(err)
		}
	}
	m = m.WithActionLog(l)
	m = press(m, "R")
	if m.mode != modeRuns || len(m.runStored) != runsLoad || m.runOlder != 5 {
		t.Fatalf("mode %v stored %d older %d", m.mode, len(m.runStored), m.runOlder)
	}
	if !strings.Contains(m.View(), "+5 older, A loads") {
		t.Fatalf("no hint:\n%s", m.View())
	}
	m = press(m, "A")
	if len(m.runStored) != runsLoad+5 || m.runOlder != 0 || strings.Contains(m.View(), "older, A loads") {
		t.Fatalf("stored %d older %d", len(m.runStored), m.runOlder)
	}
}

func TestTickIsFastOnlyWhileARunIsVisible(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), slowSpec(""))
	if m.tickEvery() != pollEvery {
		t.Fatal("board must use the slow tick")
	}
	m, _ = startRun(t, m, "s")
	m.mode = modeRuns
	if m.tickEvery() != liveEvery {
		t.Fatal("runs list with a running job must tick fast")
	}
	m.openRunOutput(runItem{live: m.runs.newest()[0]}, modeRuns)
	if m.tickEvery() != liveEvery {
		t.Fatal("output of a running job must tick fast")
	}
	m.runs.cancelAll()
	m.runs.newest()[0].done = true
	if m.tickEvery() != pollEvery {
		t.Fatal("finished run must go back to the slow tick")
	}
}

func TestRunsViewShowsHookEventsWithin80Columns(t *testing.T) {
	m, _ := newModel(t)
	m = smallScreen(m)
	m.runs = &runLog{}
	long := strings.Repeat("t", 30)
	m.runStored = []actionlog.Record{
		{Time: time.Now(), Action: "notify", Source: "hook", Event: "task.moved", ID: "1", Backend: "csv1", Title: "Alpha", Result: actionlog.ResultOK, Command: []string{"x"}},
		{Time: time.Now(), Action: long, Source: "hook", Event: "reload.fail", ID: "2", Backend: "csv1", Title: strings.Repeat("w", 60), Result: actionlog.ResultError, Command: []string{"x"}},
		{Time: time.Now(), Action: "plain", Source: "cli", ID: "3", Backend: "csv1", Result: actionlog.ResultOK, Command: []string{"x"}},
	}
	m.mode, m.runBack = modeRuns, modeBoard
	v := m.View()
	if !strings.Contains(v, "notify [task.moved] csv1 #1") || strings.Contains(v, "plain [") {
		t.Fatalf("rows:\n%s", v)
	}
	for _, line := range strings.Split(v, "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("line of %d cells: %q", w, line)
		}
	}
	m = press(m, "/")
	m.input.SetValue("source=hook event:task.moved")
	m = press(m, "enter")
	v = m.View()
	if !strings.Contains(v, "notify [task.moved]") || strings.Contains(v, "plain") || strings.Contains(v, "reload.fail") {
		t.Fatalf("filtered:\n%s", v)
	}
	m = press(m, "enter")
	if m.mode != modeRunOutput || !strings.Contains(m.View(), "notify [task.moved] on csv1:1") {
		t.Fatalf("output header:\n%s", m.View())
	}
}

// A card shows the action going on it with the elapsed time, then its result
// for resultLifetime; the details list it; running=yes filters by it.
func TestCardAndDetailsShowRunState(t *testing.T) {
	m, _ := newModel(t)
	m = withActions(t, m, t.TempDir(), action.Spec{Name: "quick", Key: "a", Command: []string{"sh", "-c", "echo hi"}}, slowSpec(""))
	m, now := clocked(m)
	m, _ = startRun(t, m, "s")
	t.Cleanup(m.runs.cancelAll)
	*now = now.Add(12 * time.Second)
	if v := m.View(); !strings.Contains(v, "⟳ slow 12s") {
		t.Fatalf("no run marker on the card:\n%s", v)
	}
	m = press(m, "enter")
	if v := m.View(); !strings.Contains(v, "⟳ running slow 12s") {
		t.Fatalf("no run in the details:\n%s", v)
	}
	m = press(m, "esc", "/", "running=yes", "enter")
	if v := m.View(); !strings.Contains(v, "Alpha") || strings.Contains(v, "Beta") {
		t.Fatalf("running=yes:\n%s", v)
	}
	m = press(m, "esc", "/", "running=no", "enter")
	if v := m.View(); strings.Contains(v, "│ #1 Alpha") || !strings.Contains(v, "Beta") {
		t.Fatalf("running=no:\n%s", v)
	}
	m = press(m, "esc")

	m = press(m, "j") // Beta: a run that ends shows OK, then nothing
	m, cmd := startRun(t, m, "a")
	m = finish(t, m, cmd)
	m.clearNotice()
	if v := m.View(); !strings.Contains(v, "OK quick") {
		t.Fatalf("no result on the card:\n%s", v)
	}
	*now = now.Add(resultLifetime + time.Second)
	if v := m.View(); strings.Contains(v, "OK quick") {
		t.Fatalf("result still on the card:\n%s", v)
	}
	m.height = 24
	if n := strings.Count(m.View(), "\n") + 1; n > 24 {
		t.Fatalf("%d lines on a 24-line terminal", n)
	}
}

// Starting an action already running on the card warns; the same start again
// runs it once more. An exclusive action is refused.
func TestDuplicateStartWarnsThenRuns(t *testing.T) {
	m, _ := newModel(t)
	excl := slowSpec("")
	excl.Name, excl.Key, excl.Exclusive = "solo", "o", true
	m = withActions(t, m, t.TempDir(), slowSpec(""), excl)
	m, _ = clocked(m)
	m, _ = startRun(t, m, "s")
	t.Cleanup(m.runs.cancelAll)

	m = press(m, "x")
	m, cmd := pressCmd(m, "s")
	if cmd != nil || !strings.Contains(m.notice, "slow is already running on #1") || len(m.runs.runs) != 1 {
		t.Fatalf("no warning: cmd %v notice %q runs %d", cmd != nil, m.notice, len(m.runs.runs))
	}
	m, cmd = pressCmd(m, ".")
	if cmd == nil || m.runs.runningOn(cardKey{"csv1", "1"}, "slow") != 2 {
		t.Fatalf("the confirmed second start did not run")
	}
	if v := m.View(); !strings.Contains(v, "⟳ slow <1s +1") {
		t.Fatalf("two runs not shown on the card:\n%s", v)
	}

	m, _ = startRun(t, m, "o")
	m = press(m, "x")
	m, cmd = pressCmd(m, "o")
	if cmd != nil || m.noticeKind != noticeFail || !strings.Contains(m.notice, "exclusive") {
		t.Fatalf("exclusive not refused: cmd %v notice %q", cmd != nil, m.notice)
	}
}

// A batch skips the marked cards the action is already running on.
func TestBatchSkipsCardsAlreadyRunningIt(t *testing.T) {
	m, _ := newModel(t)
	quick := action.Spec{Name: "slow", Key: "s", Command: []string{"sh", "-c", "true"}}
	m = withActions(t, m, t.TempDir(), slowSpec(""))
	m, _ = startRun(t, m, "s") // slow on #1
	t.Cleanup(m.runs.cancelAll)
	m.allActions = []action.Spec{quick} // same name, quick command for the batch
	m = press(m, " ", "j", " ")
	m = press(m, "x")
	m, cmd := pressCmd(m, "s")
	m = finish(t, m, cmd) // the run on #2
	if m.noticeKind != noticeFail || !strings.Contains(m.notice, "1 skipped, already running (#1)") || !strings.Contains(m.notice, "ran slow on 1 of 2 cards") {
		t.Fatalf("notice %q", m.notice)
	}
}

// Cards assigned to one of board.agents are highlighted; the details say agent.
func TestAgentCardsHighlighted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status,assignee\n1,Alpha,pending,Claude\n2,Beta,pending,ann\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.DefaultSpec()
	spec.Agents = []string{"claude", "codex"}
	m := New(openSet(t, p), "t.csv", spec)
	if !m.isAgent("Claude ") || m.isAgent("ann") || m.isAgent("") {
		t.Fatal("isAgent")
	}
	if v := press(m, "enter").View(); !strings.Contains(v, "agent") {
		t.Fatalf("details do not say agent:\n%s", v)
	}
	if v := press(m, "j", "enter").View(); strings.Contains(v, "agent") {
		t.Fatalf("ann is not an agent:\n%s", v)
	}
	plain := renderCard(m.tasks[0], 30, false, false, nil, "", metaSty, false, "")
	if hl := renderCard(m.tasks[0], 30, false, false, nil, "", metaSty, true, ""); lipgloss.Height(hl) != lipgloss.Height(plain) || !strings.Contains(hl, "@Claude") {
		t.Fatalf("agent card:\n%s", hl)
	}
}
