package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tkan/internal/audit"
	"tkan/internal/board"
)

func auditModel(t *testing.T, file string) (Model, *audit.Log, string) {
	t.Helper()
	m, _ := newModel(t)
	l, err := audit.New(file)
	if err != nil {
		t.Fatal(err)
	}
	l.SetSource("tui")
	m = m.WithAudit(l)
	return m, l, ""
}

func entriesOf(t *testing.T, l *audit.Log) []audit.Entry {
	t.Helper()
	es, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestAuditRecordsWritesAndReloads(t *testing.T) {
	m, l, _ := auditModel(t, "")
	m = press(m, "L", "r")          // move Alpha, manual reload
	m = press(m, "n", "W", "enter") // create
	m = press(m, "d", "y")          // delete the selected card
	_ = m
	var ops []string
	for _, e := range entriesOf(t, l) {
		ops = append(ops, e.Op)
	}
	if got := strings.Join(ops, ","); got != "update,reload,create,delete" {
		t.Fatalf("ops = %s", got)
	}
	es := entriesOf(t, l)
	if !strings.Contains(es[0].Details, "moved to in_progress") || !strings.Contains(es[1].Details, "manual") {
		t.Fatalf("details: %q / %q", es[0].Details, es[1].Details)
	}
}

func TestAuditTickRecordsOnlyRealChanges(t *testing.T) {
	m, l, _ := auditModel(t, "")
	nm, _ := m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	if n := len(entriesOf(t, l)); n != 0 {
		t.Fatalf("idle tick recorded %d entries", n)
	}
}

func TestAuditFailedReloadIsRecordedOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644)
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	l, _ := audit.New("")
	m = m.WithAudit(l)
	os.Remove(p)
	for i := 0; i < 3; i++ {
		nm, _ := m.Update(tickMsg(time.Now()))
		m = nm.(Model)
	}
	var errs int
	for _, e := range entriesOf(t, l) {
		if e.Op == audit.OpReload && e.Result == audit.ResultError {
			errs++
		}
	}
	if errs != 1 {
		t.Fatalf("recorded %d reload errors, want 1", errs)
	}
}

func TestAuditViewOpensFiltersAndCloses(t *testing.T) {
	m, _, _ := auditModel(t, "")
	m = press(m, "L", "d", "y") // an update and a delete
	m = press(m, "A")
	if m.mode != modeAudit {
		t.Fatalf("mode %v", m.mode)
	}
	v := m.View()
	for _, want := range []string{"tkan audit", "this session", "update", "delete", "2/2 entries"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	m = press(m, "/", "o", "p", "=", "d", "e", "l", "e", "t", "e", "enter")
	v = m.View()
	if !strings.Contains(v, "1/2 entries") || strings.Contains(v, "moved to") {
		t.Fatalf("filter op=delete:\n%s", v)
	}
	m = press(m, "/")
	m.input.SetValue("")
	m = press(m, "n", "o", "p", "e", "=", "1", "enter") // unknown field: prompt stays open
	if !m.auditEditing || !strings.Contains(m.notice, "unknown field") {
		t.Fatalf("editing=%v notice=%q", m.auditEditing, m.notice)
	}
	m = press(m, "esc", "esc") // cancel prompt, clear filter
	if m.auditFilter != "" || m.mode != modeAudit {
		t.Fatalf("filter %q mode %v", m.auditFilter, m.mode)
	}
	m = press(m, "esc")
	if m.mode != modeBoard {
		t.Fatalf("mode %v", m.mode)
	}
}

func TestAuditViewShowsPreviousSession(t *testing.T) {
	file := filepath.Join(t.TempDir(), "audit.jsonl")
	old, _ := audit.New(file)
	old.Audit(audit.OpDelete, "t", "9", nil, "from an older run")
	m, _, _ := auditModel(t, file)
	m = press(m, "A")
	v := m.View()
	if !strings.Contains(v, "previous session") || !strings.Contains(v, "from an older run") {
		t.Fatalf("view:\n%s", v)
	}
}

func TestAuditFitsSmallTerminal(t *testing.T) {
	m, _, _ := auditModel(t, "")
	nm, _ := m.Update(tea_size(80, 24))
	m = nm.(Model)
	for i := 0; i < 40; i++ {
		m = press(m, "L", "H")
	}
	m = press(m, "A")
	lines := strings.Split(m.View(), "\n")
	if len(lines) > 24 {
		t.Fatalf("%d lines in a 24-line terminal", len(lines))
	}
	for _, l := range lines {
		if len([]rune(l)) > 200 { // ANSI codes aside, nothing should be wider than the screen
			t.Fatalf("line too long: %q", l)
		}
	}
}

func tea_size(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

func TestPollNoneBoardReloadsOnlyOnR(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	os.WriteFile(p, []byte("id,title,status\n1,A,pending\n"), 0o644)
	be := openSetOpts(t, "poll: none\n", p)
	m := New(be, "t.csv", board.DefaultSpec())
	l, _ := audit.New("")
	m = m.WithAudit(l)
	os.WriteFile(p, []byte("id,title,status\n1,A,pending\n2,Fresh,pending\n"), 0o644)
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, later, later)
	nm, _ := m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	if strings.Contains(m.View(), "Fresh") || len(entriesOf(t, l)) != 0 {
		t.Fatal("the tick reloaded under poll: none")
	}
	m = press(m, "r")
	if !strings.Contains(m.View(), "Fresh") {
		t.Fatalf("r did not reload:\n%s", m.View())
	}
	if es := entriesOf(t, l); len(es) != 1 || es[0].Op != audit.OpReload {
		t.Fatalf("%+v", es)
	}
}
