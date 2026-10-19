package tui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	_ "tkan/internal/backend/csv"
	"tkan/internal/board"
	"tkan/internal/task"
)

func newModel(t *testing.T) (Model, *backend.Set) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,description,status,priority\n1,Alpha,,pending,high\n2,Beta,,pending,low\n3,Gamma,,done,\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	be := openSet(t, p)
	m := New(be, "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return nm.(Model), be
}

func openSet(t *testing.T, paths ...string) *backend.Set {
	t.Helper()
	return openSetOpts(t, "", paths...)
}

// openSetOpts is openSet with extra csv options (YAML text) for every path.
func openSetOpts(t *testing.T, extra string, paths ...string) *backend.Set {
	t.Helper()
	var items []backend.Named
	for i, p := range paths {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte("source: "+strconv.Quote(p)+"\n"+extra), &n); err != nil {
			t.Fatal(err)
		}
		name := "csv" + strconv.Itoa(i+1)
		be, err := backend.OpenSpec(backend.Spec{Type: "csv", Name: name, Options: *n.Content[0]})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, backend.Named{Name: name, Type: "csv", Backend: be})
	}
	return backend.NewSet(items)
}

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		nm, _ := m.Update(msg)
		m = nm.(Model)
	}
	return m
}

func TestViewShowsColumnsAndCards(t *testing.T) {
	m, _ := newModel(t)
	v := m.View()
	for _, want := range []string{"PENDING (2)", "DONE (1)", "#1 Alpha", "#3 Gamma"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
}

func TestMoveCardPersists(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "L") // Alpha: pending -> in_progress
	tasks, _ := be.List(context.Background())
	if tasks[0].Status != "in_progress" {
		t.Fatalf("status not persisted: %+v", tasks[0])
	}
	if m.col != 1 {
		t.Fatalf("selection should follow the card, col=%d", m.col)
	}
}

func TestNewAndDelete(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "n", "W", "i", "p", "enter")
	tasks, _ := be.List(context.Background())
	if len(tasks) != 4 || tasks[3].Title != "Wip" || tasks[3].Status != "pending" {
		t.Fatalf("create failed: %+v", tasks)
	}
	m = press(m, "d", "y")
	tasks, _ = be.List(context.Background())
	if len(tasks) != 3 {
		t.Fatalf("delete failed: %+v", tasks)
	}
}

func TestFilter(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "/", "B", "e", "enter")
	v := m.View()
	if strings.Contains(v, "Alpha") || !strings.Contains(v, "Beta") {
		t.Fatalf("filter not applied:\n%s", v)
	}
}

func TestCustomColumnsAndWriteValue(t *testing.T) {
	_, be := newModel(t)
	spec := board.Spec{Columns: []board.ColumnSpec{
		{Name: "Backlog", Values: []string{"pending"}},
		{Name: "Doing", Values: []string{"in_progress", "wip"}, Write: "wip"},
		{Name: "Shipped", Values: []string{"done"}},
	}}
	m := New(be, "t.csv", spec)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)
	v := m.View()
	for _, want := range []string{"BACKLOG (2)", "DOING (0)", "SHIPPED (1)"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	m = press(m, "L") // Alpha: Backlog -> Doing, writes "wip"
	tasks, _ := be.List(context.Background())
	if tasks[0].Status != "wip" || m.col != 1 {
		t.Fatalf("status=%q col=%d", tasks[0].Status, m.col)
	}
	m = press(m, "n", "N", "e", "w", "enter") // new card goes to the focused column
	tasks, _ = be.List(context.Background())
	if got := tasks[len(tasks)-1]; got.Title != "New" || got.Status != "wip" || got.Priority != "" {
		t.Fatalf("%+v", got)
	}
}

func TestUnmappedValueGetsTrailingColumn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	_ = os.WriteFile(p, []byte("id,title,status\n1,A,pending\n2,B,blocked\n"), 0o644)
	be := openSet(t, p)
	m := New(be, "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	if v := nm.(Model).View(); !strings.Contains(v, "BLOCKED (1)") {
		t.Fatalf("missing extra column:\n%s", v)
	}
}

func typeText(m Model, s string) Model {
	for _, r := range s {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	return m
}

func TestFieldFilterAndErrorKeepsPrompt(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "/")
	m = typeText(m, "priority=high")
	m = press(m, "enter")
	v := m.View()
	if !strings.Contains(v, "Alpha") || strings.Contains(v, "Beta") || !strings.Contains(v, "filter: priority=high") {
		t.Fatalf("field filter not applied:\n%s", v)
	}

	// An invalid query keeps the prompt open and the previous filter active.
	m = press(m, "/")
	m = typeText(m, " colour:red")
	m = press(m, "enter")
	if m.mode != modeFilter || !strings.Contains(m.View(), "unknown field") {
		t.Fatalf("mode=%v view:\n%s", m.mode, m.View())
	}
	m = press(m, "esc") // cancel the prompt; filter unchanged
	if m.filter != "priority=high" {
		t.Fatalf("filter changed: %q", m.filter)
	}
	m = press(m, "esc") // clears
	if m.filter != "" || !strings.Contains(m.View(), "Beta") {
		t.Fatalf("not cleared:\n%s", m.View())
	}
}

func TestColumnAndNegationFilters(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "/")
	m = typeText(m, "col:done")
	m = press(m, "enter")
	if v := m.View(); !strings.Contains(v, "Gamma") || strings.Contains(v, "Alpha") {
		t.Fatalf("col filter:\n%s", v)
	}
	m = press(m, "/") // the prompt is pre-filled with the active filter; replace it
	m.input.SetValue("-alpha priority!=low")
	m = press(m, "enter")
	if v := m.View(); strings.Contains(v, "Alpha") || strings.Contains(v, "Beta") || !strings.Contains(v, "Gamma") {
		t.Fatalf("negations:\n%s", v)
	}
}

func TestConfiguredSearchFields(t *testing.T) {
	_, be := newModel(t)
	spec := board.DefaultSpec()
	spec.Search = []string{"priority"} // plain text now looks only at priority
	m := New(be, "t.csv", spec)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)
	m = press(m, "/")
	m = typeText(m, "alpha")
	m = press(m, "enter")
	if v := m.View(); strings.Contains(v, "Alpha") {
		t.Fatalf("title must not be searched:\n%s", v)
	}
	m = press(m, "/")
	m.input.SetValue("high")
	m = press(m, "enter")
	if v := m.View(); !strings.Contains(v, "Alpha") || strings.Contains(v, "Beta") {
		t.Fatalf("priority search:\n%s", v)
	}
}

func TestHelpPagesShowFilterSyntax(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "?")
	if v := m.View(); !strings.Contains(v, "1/3") || !strings.Contains(v, "move card") {
		t.Fatalf("keys page:\n%s", v)
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(Model)
	v := m.View()
	for _, want := range []string{"2/3", "field:value", "field!:value", "field=value", "field!=value", "field^value", "field~regex", "Searched by plain text"} {
		if !strings.Contains(v, want) {
			t.Errorf("filter page missing %q:\n%s", want, v)
		}
	}
	if lines := strings.Count(v, "\n"); lines > 24 {
		t.Errorf("help page is %d lines, must fit 24 rows", lines)
	}
	// The third page says what every field means.
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(Model)
	v = m.View()
	for _, want := range []string{"3/3", "Filter fields", "status", "raw stored value", "column (col)", "backend", "A plain word"} {
		if !strings.Contains(v, want) {
			t.Errorf("fields page missing %q:\n%s", want, v)
		}
	}
	if lines := strings.Count(v, "\n"); lines > 24 {
		t.Errorf("fields page is %d lines, must fit 24 rows", lines)
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // wraps to the keys
	if v := nm.(Model).View(); !strings.Contains(v, "1/3") {
		t.Errorf("tab on the last page should wrap:\n%s", v)
	}
	m = press(m, "esc")
	if m.mode != modeBoard {
		t.Fatalf("mode=%v", m.mode)
	}
}

func TestQuestionMarkInEmptyPromptOpensSyntaxAndReturns(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "/", "?")
	if m.mode != modeHelp || m.helpPage != 1 || !strings.Contains(m.View(), "field~regex") {
		t.Fatalf("mode=%v page=%d", m.mode, m.helpPage)
	}
	m = press(m, "esc")
	if m.mode != modeFilter {
		t.Fatalf("should return to the prompt, mode=%v", m.mode)
	}
	m = typeText(m, "a?") // non-empty prompt: '?' is just a character
	if m.mode != modeFilter || m.input.Value() != "a?" {
		t.Fatalf("mode=%v value=%q", m.mode, m.input.Value())
	}
}

func TestCardsShowBackendAndRouteByIt(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.csv")
	b := filepath.Join(dir, "b.csv")
	// Same ID in both stores: writes must go to the right one.
	_ = os.WriteFile(a, []byte("id,title,status\n1,InA,pending\n"), 0o644)
	_ = os.WriteFile(b, []byte("id,title,status\n1,InB,pending\n"), 0o644)
	set := openSet(t, a, b)
	nm, _ := New(set, "cfg", board.DefaultSpec()).Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m := nm.(Model)
	v := m.View()
	if !strings.Contains(v, "csv1") || !strings.Contains(v, "csv2") {
		t.Fatalf("cards should show their backend:\n%s", v)
	}
	m = press(m, "j", "L") // select second card (csv2) and move it right
	tasks, _ := set.List(context.Background())
	for _, tk := range tasks {
		want := "pending"
		if tk.Backend == "csv2" {
			want = "in_progress"
		}
		if tk.Status != want {
			t.Fatalf("%s #%s status %s, want %s", tk.Backend, tk.ID, tk.Status, want)
		}
	}
	// `b` picks the backend new tasks are created in.
	m = press(m, "b", "n")
	if !strings.Contains(m.View(), "@csv2") {
		t.Fatalf("prompt should name the target backend:\n%s", m.View())
	}
	m = press(m, "x", "enter")
	tasks, _ = set.List(context.Background())
	if len(tasks) != 3 || tasks[2].Backend != "csv2" {
		t.Fatalf("%+v", tasks)
	}
}

func titlesInOrder(view string, titles ...string) bool {
	last := -1
	for _, t := range titles {
		i := strings.Index(view, t)
		if i < 0 || i < last {
			return false
		}
		last = i
	}
	return true
}

func TestSortIsDisplayOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status,priority\n1,Low,pending,low\n2,High,pending,high\n3,Mid,pending,medium\n10,None,pending,\n"
	_ = os.WriteFile(p, []byte(data), 0o644)
	spec := board.DefaultSpec()
	spec.Sort = []board.SortKey{{Field: "priority", Values: []string{"high", "medium", "low"}}}
	nm, _ := New(openSet(t, p), "t.csv", spec).Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m := nm.(Model)
	if v := m.View(); !titlesInOrder(v, "High", "Mid", "Low", "None") || !strings.Contains(v, "sort: priority↑") {
		t.Fatalf("configured sort:\n%s", v)
	}
	// Cursor starts on the first sorted card; S reverses (empty stays last) and follows the card.
	m = press(m, "S")
	if v := m.View(); !titlesInOrder(v, "Low", "Mid", "High", "None") || !strings.Contains(v, "priority↓") {
		t.Fatalf("reversed:\n%s", v)
	}
	if c, _ := m.selected(); c.Title != "High" {
		t.Fatalf("cursor should stay on its card, got %q", c.Title)
	}
	// s cycles to the first field (id, natural order); past the last it returns to the config.
	m = press(m, "s")
	if v := m.View(); !titlesInOrder(v, "Low", "High", "Mid", "None") || !strings.Contains(v, "sort: id↑") {
		t.Fatalf("id sort:\n%s", v)
	}
	for range m.fields {
		m = press(m, "s")
	}
	if v := m.View(); !strings.Contains(v, "sort: priority↑") {
		t.Fatalf("back to config:\n%s", v)
	}
	if got, _ := os.ReadFile(p); string(got) != data {
		t.Fatalf("sorting must not write to the backend:\n%s", got)
	}
}

func TestEditCard(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "e")
	if m.mode != modeEdit {
		t.Fatal("e did not open the form")
	}
	m = typeText(m, " one")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(Model)
	for i := 0; i < 3; i++ { // description -> priority -> type -> assignee
		nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = nm.(Model)
	}
	m = typeText(m, "ann")
	m = press(m, "enter")
	tasks, _ := be.List(context.Background())
	if m.mode != modeBoard || tasks[0].Title != "Alpha one" || tasks[0].Assignee != "ann" || tasks[0].Priority != "high" {
		t.Fatalf("edit not saved: mode=%v %+v", m.mode, tasks[0])
	}
}

func TestEditValidationAndCancel(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "e")
	for i := 0; i < 5; i++ { // to due
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = nm.(Model)
	}
	m = typeText(m, "tomorrow")
	m = press(m, "enter")
	if m.mode != modeEdit || !strings.Contains(m.notice, "YYYY-MM-DD") {
		t.Fatalf("bad date accepted: mode=%v notice=%q", m.mode, m.notice)
	}
	m = press(m, "esc", "esc") // dirty form: the first esc only warns
	tasks, _ := be.List(context.Background())
	if m.mode != modeBoard || tasks[0].Due != "" {
		t.Fatalf("cancel changed data: %+v", tasks[0])
	}
}

func TestEditKeepsLongMultilineFields(t *testing.T) {
	m, be := newModel(t)
	ctx := context.Background()
	tasks, _ := be.List(ctx)
	orig := tasks[0]
	orig.Description = strings.Repeat("long ", 200) + "\nsecond line"
	orig.Notes = "line1\nline2"
	if err := be.Update(ctx, orig); err != nil {
		t.Fatal(err)
	}
	m.reload()
	m = press(m, "e")
	m = typeText(m, " x") // edit only the title
	m = press(m, "enter")
	tasks, _ = be.List(ctx)
	if m.mode != modeBoard || tasks[0].Title != "Alpha x" {
		t.Fatalf("not saved: mode=%v %+v", m.mode, tasks[0])
	}
	if tasks[0].Description != orig.Description || tasks[0].Notes != orig.Notes {
		t.Fatalf("untouched fields changed:\n%q\n%q", tasks[0].Description, tasks[0].Notes)
	}
}

func TestViewFitsScreenAt80x24(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,description,status\n"
	for i := 1; i <= 20; i++ {
		data += strconv.Itoa(i) + ",Task " + strconv.Itoa(i) + ",,pending\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(Model)
	check := func(label string) {
		t.Helper()
		if n := strings.Count(m.View(), "\n") + 1; n > 24 {
			t.Errorf("%s: view is %d lines, want <= 24", label, n)
		}
	}
	check("top")
	m = press(m, "j", "j", "j", "j", "j", "j", "j", "j", "j", "j") // scrolled: both markers
	check("scrolled")
	m = press(m, "/")
	check("filter prompt")
}

func TestReloadKeepsSelectedCard(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "j") // Beta
	if sel, _ := m.selected(); sel.Title != "Beta" {
		t.Fatalf("setup: selected %q", sel.Title)
	}
	tasks, _ := be.List(context.Background())
	if err := be.Delete(context.Background(), tasks[0].Backend, tasks[0].ID); err != nil { // delete Alpha above Beta
		t.Fatal(err)
	}
	m.reload()
	if sel, ok := m.selected(); !ok || sel.Title != "Beta" {
		t.Fatalf("cursor jumped to %+v", sel)
	}
}

func TestHeaderShowsColumnPositionWhenColumnsDoNotFit(t *testing.T) {
	m, _ := newModel(t) // width 120: 4 columns fit, default board has 3
	if strings.Contains(m.View(), "column 1/") {
		t.Fatal("indicator shown although all columns fit")
	}
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,T,A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.Spec{Columns: []board.ColumnSpec{{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"}}}
	m = New(openSet(t, p), "t.csv", spec)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = press(nm.(Model), "l", "l", "l")
	if v := m.View(); !strings.Contains(v, "column 4/4") || !strings.Contains(v, "D (0)") {
		t.Fatalf("missing indicator or last column:\n%s", v)
	}
}

func TestDetailViewScrollsAndFitsScreen(t *testing.T) {
	m, be := newModel(t)
	ctx := context.Background()
	tasks, _ := be.List(ctx)
	tasks[0].Notes = strings.Repeat("a long note line\n", 60) + "THE END"
	if err := be.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(Model)
	m.reload()
	m = press(m, "enter")
	check := func(label string) string {
		t.Helper()
		v := m.View()
		if n := strings.Count(v, "\n") + 1; n > 24 {
			t.Errorf("%s: %d lines, want <= 24", label, n)
		}
		return v
	}
	if v := check("top"); !strings.Contains(v, "#1  Alpha") || strings.Contains(v, "THE END") {
		t.Fatalf("top of card wrong:\n%s", v)
	}
	m = press(m, "G")
	if v := check("end"); !strings.Contains(v, "THE END") || strings.Contains(v, "#1  Alpha") {
		t.Fatalf("end of card wrong:\n%s", v)
	}
	m = press(m, "g")
	if v := check("home"); !strings.Contains(v, "#1  Alpha") {
		t.Fatalf("home did not scroll back:\n%s", v)
	}
}

func TestMoveAndCreateGiveFeedback(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "L")
	if m.notice != "moved #1 to in_progress" {
		t.Fatalf("notice %q", m.notice)
	}
	// With a filter that hides the moved card the user is told why it vanished.
	m = press(m, "/")
	m = typeText(m, "col:pending")
	m = press(m, "enter")
	m = press(m, "h", "L") // Beta leaves the filtered column
	if !strings.Contains(m.notice, "hidden by the filter") {
		t.Fatalf("no hidden hint after move: %q", m.notice)
	}
	m, _ = newModel(t)
	m = press(m, "/")
	m = typeText(m, "title:Beta")
	m = press(m, "enter")
	m = press(m, "n")
	m = typeText(m, "Zzz")
	m = press(m, "enter")
	if !strings.HasPrefix(m.notice, "created #") || !strings.Contains(m.notice, "hidden by the filter") {
		t.Fatalf("no hidden hint after create: %q", m.notice)
	}
}

func TestNavigationKeysAndRowMemory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status\n"
	for i := 1; i <= 10; i++ {
		data += strconv.Itoa(i) + ",T" + strconv.Itoa(i) + ",pending\n"
	}
	data += "11,D1,done\n12,D2,done\n13,D3,done\n14,P1,in_progress\n15,P2,in_progress\n16,P3,in_progress\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)
	m = press(m, "G")
	if m.row != 9 {
		t.Fatalf("G: row %d", m.row)
	}
	m = press(m, "g")
	if m.row != 0 {
		t.Fatalf("g: row %d", m.row)
	}
	pg := tea.KeyMsg{Type: tea.KeyPgDown}
	want := m.pageSize(true)
	nm, _ = m.Update(pg)
	m = nm.(Model)
	if m.row != want || m.row < 2 {
		t.Fatalf("pgdn: row %d", m.row)
	}
	m = press(m, "G")
	m = press(m, "l", "l") // to done (3 cards): row clamps, not resets
	if m.col != 2 || m.row != 2 {
		t.Fatalf("column change: col %d row %d, want 2/2", m.col, m.row)
	}
}

func TestEditEscNeedsConfirmationWhenDirty(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "e", "esc") // nothing changed: closes at once
	if m.mode != modeBoard {
		t.Fatal("clean esc did not close the form")
	}
	m = press(m, "e")
	m = typeText(m, "!")
	m = press(m, "esc")
	if m.mode != modeEdit || !strings.Contains(m.notice, "unsaved") {
		t.Fatalf("dirty esc closed the form: mode=%v notice=%q", m.mode, m.notice)
	}
	m = typeText(m, "?") // any other key disarms
	m = press(m, "esc")
	if m.mode != modeEdit {
		t.Fatal("esc after typing should warn again")
	}
	m = press(m, "esc")
	tasks, _ := be.List(context.Background())
	if m.mode != modeBoard || tasks[0].Title != "Alpha" {
		t.Fatalf("discard failed: mode=%v %+v", m.mode, tasks[0])
	}
}

func TestEditFormAdaptsToWidth(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "e")
	if w := m.edit.inputs[0].Width; w != 50 {
		t.Fatalf("wide terminal: width %d, want 50", w)
	}
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = nm.(Model)
	if w := m.edit.inputs[0].Width; w != 25 {
		t.Fatalf("40 columns: width %d, want 25", w)
	}
}

func TestDeleteConfirmCancel(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "d")
	if m.mode != modeConfirmDelete {
		t.Fatal("d did not ask for confirmation")
	}
	m = press(m, "x") // anything but y cancels
	tasks, _ := be.List(context.Background())
	if m.mode != modeBoard || len(tasks) != 3 {
		t.Fatalf("cancel deleted: mode=%v tasks=%d", m.mode, len(tasks))
	}
}

func TestBackendKeyWithSingleBackendIsNoop(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "b")
	if m.notice != "" || m.target != 0 {
		t.Fatalf("b changed state: notice=%q target=%d", m.notice, m.target)
	}
}

func TestDetailEditKeyOpensForm(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "enter", "e")
	if m.mode != modeEdit {
		t.Fatalf("mode %v, want edit", m.mode)
	}
}

func TestTickReloadsExternalChanges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n2,Fresh,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, later, later)
	nm, _ := m.Update(tickMsg(time.Now()))
	if v := nm.(Model).View(); !strings.Contains(v, "Fresh") {
		t.Fatalf("tick did not pick up the external change:\n%s", v)
	}
}

func TestReloadErrorShowsNotice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	m = press(m, "r")
	if !strings.HasPrefix(m.notice, "error:") {
		t.Fatalf("notice %q", m.notice)
	}
	// the tick surfaces the missing file too, without a keypress
	m.notice = ""
	nm, _ := m.Update(tickMsg(time.Now()))
	if !strings.HasPrefix(nm.(Model).notice, "error:") {
		t.Fatalf("tick notice %q", nm.(Model).notice)
	}
}

func TestWIPLimitShownInHeaderAndMoveNotice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,todo\n2,B,todo\n3,C,doing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.Spec{Columns: []board.ColumnSpec{{Name: "Todo", Values: []string{"todo"}}, {Name: "Doing", Values: []string{"doing"}, Limit: 1}}}
	m := New(openSet(t, p), "t.csv", spec)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)
	if v := m.View(); !strings.Contains(v, "DOING (1/1)") || strings.Contains(v, "DOING (1/1) !") {
		t.Fatalf("at the limit is not over:\n%s", v)
	}
	m = press(m, "L") // Todo's first card moves to Doing: 2/1
	if !strings.Contains(m.notice, "over its limit of 1") {
		t.Fatalf("notice %q", m.notice)
	}
	if v := m.View(); !strings.Contains(v, "DOING (2/1) !") {
		t.Fatalf("over-limit marker missing:\n%s", v)
	}
}

func TestColumnColorRendersAndKeepsLayout(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,todo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.Spec{Columns: []board.ColumnSpec{{Name: "Todo", Values: []string{"todo"}, Color: "#ff8800"}, {Name: "Done", Values: []string{"done"}}}}
	m := New(openSet(t, p), "t.csv", spec)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	v := nm.(Model).View()
	for _, want := range []string{"TODO (1)", "#1 A", "DONE (0)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lost %q with a column colour:\n%s", want, v)
		}
	}
}

func TestSavedViewsCycle(t *testing.T) {
	spec := board.DefaultSpec()
	spec.Views = []board.ViewSpec{{Name: "high", Query: "priority=high"}, {Name: "done", Query: "col:done"}}
	m, _ := newModel(t)
	m.spec = spec
	m = press(m, "v")
	if m.view != "high" || m.filter != "priority=high" {
		t.Fatalf("first view: %q %q", m.view, m.filter)
	}
	if v := m.View(); !strings.Contains(v, "#1 Alpha") || strings.Contains(v, "#2 Beta") || !strings.Contains(v, "view high") {
		t.Fatalf("view not applied:\n%s", v)
	}
	m = press(m, "v")
	if m.view != "done" || !strings.Contains(m.View(), "#3 Gamma") {
		t.Fatalf("second view: %q", m.view)
	}
	m = press(m, "v")
	if m.view != "" || m.filter != "" || !strings.Contains(m.View(), "#2 Beta") {
		t.Fatalf("cycle did not end with no view: %q %q", m.view, m.filter)
	}
	m.spec.Views = []board.ViewSpec{{Name: "bad", Query: "nofield=1"}}
	if m = press(m, "v"); m.filter != "" || !strings.Contains(m.notice, "view bad") {
		t.Fatalf("invalid view applied: %q %q", m.filter, m.notice)
	}
	m.spec.Views = nil
	if m = press(m, "v"); !strings.Contains(m.notice, "no saved views") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestColumnSortOverridesBoardSort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,todo\n2,B,todo\n3,C,done\n4,D,done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.Spec{
		Columns: []board.ColumnSpec{
			{Name: "Todo", Values: []string{"todo"}},
			{Name: "Done", Values: []string{"done"}, Sort: []board.SortKey{{Field: "id", Desc: true}}},
		},
		Sort: []board.SortKey{{Field: "id"}},
	}
	m := New(openSet(t, p), "t.csv", spec)
	if got := m.column(0); got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("board.sort not applied to Todo: %+v", got)
	}
	if got := m.column(1); got[0].ID != "4" || got[1].ID != "3" {
		t.Fatalf("column sort not applied to Done: %+v", got)
	}
	m = press(m, "s") // a session sort wins everywhere
	if got := m.column(1); got[0].ID == "4" && m.sortKeys == nil {
		t.Fatal("session sort ignored")
	}
}

func TestHeaderCountsFollowTheFilter(t *testing.T) {
	m, _ := newModel(t) // 2 pending, 1 done
	if v := m.View(); !strings.Contains(v, "3 tasks") || !strings.Contains(v, "PENDING (2)") {
		t.Fatalf("unfiltered header:\n%s", v)
	}
	m = press(m, "/")
	m = typeText(m, "Alpha")
	m = press(m, "enter")
	v := m.View()
	for _, want := range []string{"1/3 tasks", "PENDING (1/2)", "DONE (0/1)"} {
		if !strings.Contains(v, want) {
			t.Errorf("filtered header lacks %q:\n%s", want, v)
		}
	}
}

func TestCardColoursAndLabels(t *testing.T) {
	for _, p := range []string{"critical", "URGENT", "p0", "P1", "Normal", "p3", "minor"} {
		if priorityColorOf(p) == priorityGrey {
			t.Errorf("priority %q has no colour", p)
		}
	}
	if priorityColorOf("urgent") == priorityColorOf("low") || priorityColorOf("HIGH") != priorityColorOf("p1") || priorityColorOf("zzz") != priorityGrey {
		t.Error("priority colours are not consistent across spellings")
	}
	if typeColor("Bug") != typeColor(" bug ") {
		t.Error("type colour must ignore case and spaces")
	}
	seen := map[string]bool{}
	for _, ty := range []string{"bug", "feature", "chore", "fix", "docs", "enhancement"} {
		seen[typeColor(ty).Dark.ANSI256] = true
	}
	if len(seen) < 3 {
		t.Errorf("types share too few colours: %v", seen)
	}
	for labels, want := range map[string]string{"": "", "ui": "[ui]", "ui, api": "[ui,api]", "a,b,c,d": "[a,b+2]", " , ,": ""} {
		if got := labelChip(task.Task{Labels: labels}); got != want {
			t.Errorf("labelChip(%q) = %q, want %q", labels, got, want)
		}
	}
	meta := cardMeta(task.Task{Priority: "p1", Type: "bug", Labels: "ui,api,x"}, 60, metaSty, false)
	if !strings.Contains(meta, "p1") || !strings.Contains(meta, "bug") || !strings.Contains(meta, "[ui,api+1]") {
		t.Errorf("meta %q", meta)
	}
}

func TestGroupByShowsSubHeadersAndKeepsNavigationConsistent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status,priority,type,labels\n" +
		"1,A,pending,low,bug,ui\n2,B,pending,high,bug,\n3,C,pending,low,feature,api;x\n4,D,pending,high,bug,ui\n"
	data = strings.ReplaceAll(data, ";", ",")
	data = strings.Replace(data, "api,x", `"api,x"`, 1)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(Model)
	m = press(m, "z")
	if m.groupBy != "priority" {
		t.Fatalf("group %q", m.groupBy)
	}
	// store order is 1(low) 2(high) 3(low) 4(high): groups in first-appearance order low, high
	col := m.column(0)
	if ids := col[0].ID + col[1].ID + col[2].ID + col[3].ID; ids != "1324" {
		t.Fatalf("grouped order %s, want 1324", ids)
	}
	v := m.View()
	for _, want := range []string{"-- low (2) --", "-- high (2) --", "group: priority"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	// moving down selects the next card in the grouped order
	m = press(m, "j", "j")
	if sel, _ := m.selected(); sel.ID != "2" {
		t.Fatalf("selected %s, want 2 (first card of the high group)", sel.ID)
	}
	m = press(m, "z") // type
	if m.groupBy != "type" || !strings.Contains(m.View(), "-- bug (3) --") {
		t.Fatalf("type grouping:\n%s", m.View())
	}
	m = press(m, "z", "z") // assignee, labels
	if m.groupBy != "labels" || !strings.Contains(m.View(), "-- ui (2) --") || !strings.Contains(m.View(), "-- (none) (1) --") {
		t.Fatalf("labels grouping:\n%s", m.View())
	}
	m = press(m, "z")
	if m.groupBy != "" || strings.Contains(m.View(), "-- ") {
		t.Fatal("grouping not switched off")
	}
}

func TestGroupedViewStillFitsAt80x24(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status,type\n"
	for i := 1; i <= 15; i++ {
		data += strconv.Itoa(i) + ",T,pending," + []string{"a", "b", "c"}[i%3] + "\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = press(nm.(Model), "z", "z") // by type
	for i := 0; i < 12; i++ {
		m = press(m, "j")
		if n := strings.Count(m.View(), "\n") + 1; n > 24 {
			t.Fatalf("row %d: %d lines", m.row, n)
		}
	}
}

func TestQuickFilterFromSelectedCard(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status,priority,type,assignee,labels\n" +
		"1,A,pending,high,bug,ann lee,\"ui,api\"\n2,B,pending,low,feature,bob,\n3,C,pending,high,bug,,\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)

	m = press(m, "f", "t") // card 1: type=bug
	if m.filter != "type=bug" || len(m.column(0)) != 2 {
		t.Fatalf("type filter: %q (%d cards)", m.filter, len(m.column(0)))
	}
	m = press(m, "esc")
	m = press(m, "f", "a") // assignee has a space: quoted
	if m.filter != `assignee="ann lee"` || len(m.column(0)) != 1 {
		t.Fatalf("assignee filter: %q", m.filter)
	}
	m = press(m, "esc")
	m = press(m, "f", "l")
	if m.filter != "labels:ui" || len(m.column(0)) != 1 {
		t.Fatalf("label filter: %q", m.filter)
	}
	m = press(m, "esc", "j", "j") // card 3 has no assignee
	m = press(m, "f", "a")
	if m.filter != "" || !strings.Contains(m.notice, "no assignee") {
		t.Fatalf("empty property: filter %q notice %q", m.filter, m.notice)
	}
	m = press(m, "f", "esc") // cancel
	if m.mode != modeBoard {
		t.Fatal("esc did not leave quick mode")
	}
}

func TestFilterHistoryRecall(t *testing.T) {
	m, _ := newModel(t)
	for _, f := range []string{"Alpha", "col:done"} {
		m = press(m, "/")
		m.input.SetValue(f)
		m = press(m, "enter")
	}
	m = press(m, "esc", "/")
	m.input.SetValue("")
	up := tea.KeyMsg{Type: tea.KeyUp}
	nm, _ := m.Update(up)
	m = nm.(Model)
	if m.input.Value() != "col:done" {
		t.Fatalf("first up: %q", m.input.Value())
	}
	nm, _ = m.Update(up)
	m = nm.(Model)
	if m.input.Value() != "Alpha" {
		t.Fatalf("second up: %q", m.input.Value())
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = nm.(Model)
	if m.input.Value() != "col:done" {
		t.Fatalf("down: %q", m.input.Value())
	}
}

func TestUndoMoveEditDelete(t *testing.T) {
	m, be := newModel(t)
	ctx := context.Background()
	status := func(title string) string {
		ts, _ := be.List(ctx)
		for _, x := range ts {
			if x.Title == title {
				return x.Status
			}
		}
		return "<gone>"
	}
	if m = press(m, "u"); m.notice != "nothing to undo" {
		t.Fatalf("empty undo: %q", m.notice)
	}

	m = press(m, "L") // Alpha pending -> in_progress
	if status("Alpha") != "in_progress" {
		t.Fatal("setup: move failed")
	}
	m = press(m, "u")
	if status("Alpha") != "pending" || !strings.Contains(m.notice, "undid move") {
		t.Fatalf("move not undone: %s %q", status("Alpha"), m.notice)
	}

	m = press(m, "e")
	m = typeText(m, "X")
	m = press(m, "enter")
	if status("AlphaX") == "<gone>" {
		t.Fatal("setup: edit failed")
	}
	m = press(m, "u")
	if status("Alpha") == "<gone>" || status("AlphaX") != "<gone>" {
		t.Fatalf("edit not undone, notice %q", m.notice)
	}

	m = press(m, "d", "y")
	if status("Alpha") != "<gone>" {
		t.Fatal("setup: delete failed")
	}
	m = press(m, "u")
	if status("Alpha") != "pending" || !strings.Contains(m.notice, "undid delete") {
		t.Fatalf("delete not undone: %q %q", status("Alpha"), m.notice)
	}
	if !strings.Contains(m.notice, "restored as #") {
		t.Fatalf("a changed ID must be reported: %q", m.notice)
	}
}

func TestUndoStackIsBounded(t *testing.T) {
	m, _ := newModel(t)
	for i := 0; i < maxUndo+10; i++ {
		m.pushUndo(undoOp{what: "x"})
	}
	if len(m.undo) != maxUndo {
		t.Fatalf("undo stack grew to %d", len(m.undo))
	}
}

func TestColumnStripAndNumberKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,a\n2,B,a\n3,C,d\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.Spec{Columns: []board.ColumnSpec{{Name: "Alpha", Values: []string{"a"}}, {Name: "Beta"}, {Name: "Gamma"}, {Name: "Delta", Values: []string{"d"}}}}
	m := New(openSet(t, p), "t.csv", spec)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(Model)
	v := m.View()
	if !strings.Contains(v, "1 Alpha 2") {
		t.Fatalf("strip missing:\n%s", v)
	}
	for _, want := range []string{"4 Delta 1", "2 Beta 0"} {
		if !strings.Contains(v, want) {
			t.Errorf("strip lacks %q:\n%s", want, v)
		}
	}
	m = press(m, "4")
	if m.col != 3 || !strings.Contains(m.View(), "DELTA (1)") {
		t.Fatalf("4 did not jump to Delta: col %d", m.col)
	}
	m = press(m, "9") // no such column: unchanged
	if m.col != 3 {
		t.Fatalf("9 moved to column %d", m.col)
	}
	if n := strings.Count(m.View(), "\n") + 1; n > 24 {
		t.Fatalf("view is %d lines", n)
	}

	// narrow terminal: other columns shrink to N:count, the focused one keeps its name
	nm, _ = m.Update(tea.WindowSizeMsg{Width: 30, Height: 24})
	strip := nm.(Model).columnStrip()
	if !strings.Contains(strip, "4 Delta 1") || !strings.Contains(strip, "1:2") {
		t.Fatalf("compact strip: %q", strip)
	}

	// everything fits: no strip, the blank line stays
	nm, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 24})
	if strings.Contains(nm.(Model).View(), "1 Alpha") {
		t.Fatal("strip shown although all columns fit")
	}
}

func TestPageSizeUsesRealCardHeights(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status\n"
	for i := 1; i <= 40; i++ {
		data += strconv.Itoa(i) + ",Task,pending\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(Model)
	// one-line titles make 4-line cards; 24 rows leave 18 lines of cards: 4 fit
	if n := m.pageSize(true); n != 4 {
		t.Fatalf("pageSize = %d, want 4 (cards are 4 lines, %d lines available)", n, m.cardsHeight())
	}
	m = press(m, "G")
	if n := m.pageSize(false); n != 4 {
		t.Fatalf("pageSize(up) = %d, want 4", n)
	}
	// after pgdn the selected card must still be on screen (the view scrolls to it)
	m = press(m, "g")
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = nm.(Model)
	if !strings.Contains(m.View(), "#"+strconv.Itoa(m.row+1)+" Task") {
		t.Fatalf("selected card #%d is off screen:\n%s", m.row+1, m.View())
	}
	if n := strings.Count(m.View(), "\n") + 1; n > 24 {
		t.Fatalf("%d lines", n)
	}
}

func TestCardMetaNeverWrapsAndDropsLeastImportantFirst(t *testing.T) {
	long := task.Task{Backend: "backend-with-a-long-name", Priority: "critical", Type: "documentation", Assignee: "christopher-longname", Due: "2026-12-31", Labels: "frontend,backend,infra"}
	for _, w := range []int{60, 40, 24, 12, 6} {
		got := cardMeta(long, w, metaSty, false)
		if strings.Contains(got, "\n") || lipgloss.Width(got) > w {
			t.Errorf("width %d: %q is %d wide", w, got, lipgloss.Width(got))
		}
	}
	// plenty of room: everything shows
	if got := cardMeta(task.Task{Priority: "high", Type: "bug", Assignee: "ann", Due: "2026-01-02", Labels: "ui"}, 60, metaSty, false); !strings.Contains(got, "due 2026-01-02") || !strings.Contains(got, "[ui]") {
		t.Errorf("wide meta lost parts: %q", got)
	}
	// tight: labels and due go before priority
	got := cardMeta(task.Task{Priority: "high", Type: "bug", Assignee: "ann", Due: "2026-01-02", Labels: "ui"}, 16, metaSty, false)
	if strings.Contains(got, "[ui]") || strings.Contains(got, "due") || !strings.Contains(got, "high") {
		t.Errorf("tight meta kept the wrong parts: %q", got)
	}
}

func TestLongMetaDoesNotPushFooterOffAt80x24(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status,priority,type,assignee,due,labels\n"
	for i := 1; i <= 12; i++ {
		data += strconv.Itoa(i) + ",T,pending,critical,documentation,christopher-longname,2026-12-31,\"frontend,backend,infra\"\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = press(nm.(Model), "j", "j", "j", "j")
	v := m.View()
	if n := strings.Count(v, "\n") + 1; n > 24 {
		t.Fatalf("%d lines:\n%s", n, v)
	}
	if !strings.Contains(v, "? help") {
		t.Fatalf("footer missing:\n%s", v)
	}
}

func TestDueDatesAreFlaggedOutsideFinishedColumns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status,due\n1,Late,pending,2026-10-10\n2,Soon,pending,2026-10-18\n3,Later,pending,2026-12-01\n4,NoDate,pending,\n5,LateButDone,done,2026-10-10\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	m.now = func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }
	want := map[string]dueLevel{"1": dueOverdue, "2": dueSoon, "3": dueNormal, "4": dueNormal, "5": dueNormal}
	for c := range m.board.Columns {
		for _, tk := range m.column(c) {
			if got := m.dueLevelOf(tk, c); got != want[tk.ID] {
				t.Errorf("task %s (%s): level %d, want %d", tk.ID, tk.Title, got, want[tk.ID])
			}
		}
	}
}

func TestFinishedColumnNames(t *testing.T) {
	for _, n := range []string{"Done", "closed", "Won't Do", "WONT_DO", "Released", "cancelled"} {
		if !board.Finished(n) {
			t.Errorf("%q should be finished", n)
		}
	}
	for _, n := range []string{"Doing", "pending", "Review", "Backlog", ""} {
		if board.Finished(n) {
			t.Errorf("%q should not be finished", n)
		}
	}
}

func TestDeleteNoticesAndFailurePath(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "d", "y")
	if !strings.HasPrefix(m.notice, "deleted #1") {
		t.Fatalf("delete notice %q", m.notice)
	}
	// the backend file vanishes: the delete reports an error and keeps the board usable
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m = New(openSet(t, p), "t.csv", board.DefaultSpec())
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	m = press(m, "d", "y")
	if !strings.HasPrefix(m.notice, "error:") || m.mode != modeBoard {
		t.Fatalf("failed delete: notice %q mode %v", m.notice, m.mode)
	}
}

func TestTickIgnoredOutsideBoardModeThenAppliedOnReturn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, enter := range []string{"enter", "n", "e"} { // detail, new, edit
		m := New(openSet(t, p), "t.csv", board.DefaultSpec())
		nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
		m = press(nm.(Model), enter)
		if m.mode == modeBoard {
			t.Fatalf("%s did not leave board mode", enter)
		}
		if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n2,External,pending\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		later := time.Now().Add(time.Hour)
		_ = os.Chtimes(p, later, later)
		nm, _ = m.Update(tickMsg(time.Now()))
		m = nm.(Model)
		if len(m.tasks) != 1 {
			t.Fatalf("%s mode: tick reloaded behind the user's back (%d tasks)", enter, len(m.tasks))
		}
		m = press(m, "esc")
		if m.mode == modeEdit { // a clean edit form closes on one esc; belt and braces
			m = press(m, "esc")
		}
		nm, _ = m.Update(tickMsg(time.Now()))
		m = nm.(Model)
		if len(m.tasks) != 2 {
			t.Fatalf("%s mode: change not picked up after returning to the board (%d tasks)", enter, len(m.tasks))
		}
		if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644); err != nil { // reset
			t.Fatal(err)
		}
	}
}

func TestNewTaskIsSelectedUnderActiveSort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,Bravo,pending\n2,Alpha,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.DefaultSpec()
	spec.Sort = []board.SortKey{{Field: "title"}}
	spec.Search = []string{"title"}
	m := New(openSet(t, p), "t.csv", spec)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)
	m = press(m, "n")
	m = typeText(m, "Charlie")
	m = press(m, "enter")
	sel, ok := m.selected()
	if !ok || sel.Title != "Charlie" {
		t.Fatalf("selected %+v", sel)
	}
	if got := m.column(0); got[0].Title != "Alpha" || got[2].Title != "Charlie" || m.row != 2 {
		t.Fatalf("sorted column %v, row %d", got, m.row)
	}
}

func TestTitleLineStaysOneLineAt80Cols(t *testing.T) {
	m, _ := newModel(t)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(Model)
	m.filter = "type:bug label:frontend assignee:somebody-with-a-long-name"
	m.groupBy = "type"
	m.marks = map[cardKey]bool{{}: true, {id: "x"}: true}
	first := strings.SplitN(m.View(), "\n", 2)[0]
	if w := lipgloss.Width(first); w > 79 {
		t.Fatalf("title line is %d cols, want <= 79: %q", w, first)
	}
}

func TestFocusedColumnHeaderHasTextMarker(t *testing.T) {
	m, _ := newModel(t)
	v := m.View()
	if strings.Count(v, "▸ ") != 1 {
		t.Fatalf("want exactly one focus marker, got %d in:\n%s", strings.Count(v, "▸ "), v)
	}
}

// TestRolesStayDistinctOnSixteenColours renders the colour roles under the ANSI
// profile (a Linux console, a Raspberry Pi over serial): none may collapse.
func TestRolesStayDistinctOnSixteenColours(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)
	groups := map[string]map[string]lipgloss.Style{
		"ids":         {"id": idSty},
		"header tags": {"loading": loadSty, "filter": filterSty, "mark": markSty, "sort": sortSty, "group": groupSty, "fail": failSty, "ok": okSty},
		"priorities": {
			"p0": lipgloss.NewStyle().Foreground(priorityColorOf("p0")),
			"p1": lipgloss.NewStyle().Foreground(priorityColorOf("p1")),
			"p2": lipgloss.NewStyle().Foreground(priorityColorOf("p2")),
			"p3": lipgloss.NewStyle().Foreground(priorityColorOf("p3")),
		},
	}
	for gname, roles := range groups {
		seen := map[string]string{}
		for name, sty := range roles {
			out := sty.Render("x")
			if !strings.Contains(out, "\x1b[") {
				t.Errorf("%s: %s renders without colour under ANSI: %q", gname, name, out)
			}
			if other, dup := seen[out]; dup {
				t.Errorf("%s: %s and %s render the same on 16 colours: %q", gname, name, other, out)
			}
			seen[out] = name
		}
	}
}

// TestTextCuesSurviveNoColor: with colours off, the board still shows the focus
// marker.
func TestTextCuesSurviveNoColor(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(old)
	m, _ := newModel(t)
	v := m.View()
	if strings.Contains(v, "\x1b[") {
		t.Fatalf("ascii profile still emits escapes: %q", v)
	}
	if !strings.Contains(v, "▸ ") {
		t.Error("no focus marker without colour")
	}
}

func TestDeleteMarkedCards(t *testing.T) {
	m, be := newModel(t)
	m = press(m, " ", "down", " ") // mark two cards of the first column
	if len(m.marks) != 2 {
		t.Fatalf("marks %d, want 2", len(m.marks))
	}
	m = press(m, "d")
	if m.mode != modeConfirmDelete || len(m.deleteTargets) != 2 {
		t.Fatalf("d: mode %v, %d targets", m.mode, len(m.deleteTargets))
	}
	m = press(m, "x") // cancel keeps everything, marks included
	tasks, _ := be.List(context.Background())
	if len(tasks) != 3 || len(m.marks) != 2 {
		t.Fatalf("cancel: %d tasks, %d marks", len(tasks), len(m.marks))
	}
	m = press(m, "d", "y")
	tasks, _ = be.List(context.Background())
	if len(tasks) != 1 || len(m.marks) != 0 || !strings.Contains(m.notice, "deleted 2 cards") {
		t.Fatalf("delete: %d tasks, %d marks, notice %q", len(tasks), len(m.marks), m.notice)
	}
	m = press(m, "u")
	tasks, _ = be.List(context.Background())
	if len(tasks) != 3 {
		t.Fatalf("undo: %d tasks, want 3 (notice %q)", len(tasks), m.notice)
	}
}

func TestMoveToColumn(t *testing.T) {
	m, be := newModel(t)
	m = press(m, "m", "3") // #1 Alpha: pending -> done
	tasks, _ := be.List(context.Background())
	if got := tasks[0].Status; got != "done" {
		t.Fatalf("status %q, want done (notice %q)", got, m.notice)
	}
	m = press(m, "u")
	tasks, _ = be.List(context.Background())
	if got := tasks[0].Status; got != "pending" {
		t.Fatalf("undo: status %q", got)
	}
	m = press(m, "m", "1")
	if !strings.Contains(m.notice, "already in") {
		t.Fatalf("notice %q", m.notice)
	}
	m = press(m, "m", "esc", "m", "9")
	if m.mode != modeBoard {
		t.Fatalf("mode %v", m.mode)
	}
}

func TestMoveMarkedCardsToColumn(t *testing.T) {
	m, be := newModel(t)
	m = press(m, " ", "down", " ", "m", "3")
	tasks, _ := be.List(context.Background())
	if tasks[0].Status != "done" || tasks[1].Status != "done" || len(m.marks) != 0 || !strings.Contains(m.notice, "moved 2 cards") {
		t.Fatalf("statuses %q %q, marks %d, notice %q", tasks[0].Status, tasks[1].Status, len(m.marks), m.notice)
	}
	m = press(m, "u")
	tasks, _ = be.List(context.Background())
	if tasks[0].Status != "pending" || tasks[1].Status != "pending" {
		t.Fatalf("undo: %q %q (notice %q)", tasks[0].Status, tasks[1].Status, m.notice)
	}
}
