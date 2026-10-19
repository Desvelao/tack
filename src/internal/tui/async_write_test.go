package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tkan/internal/backend"
	"tkan/internal/board"
)

// asyncModel is an interactive (NewAsync) board with its single backend loaded.
func asyncModel(t *testing.T, rows string) (Model, *backend.Set, string) {
	t.Helper()
	path := writeCSV(t, rows)
	be := openSet(t, path)
	m := NewAsync(be, "t", board.DefaultSpec(), nil)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)
	ts, err := be.ListOne(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	nm, _ = m.Update(backendLoadedMsg{idx: 0, tasks: ts})
	return nm.(Model), be, path
}

func keyCmd(m Model, key string) (Model, tea.Cmd) {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	nm, cmd := m.Update(msg)
	return nm.(Model), cmd
}

// doneWrite delivers the answer of a background write to the model.
func doneWrite(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	msg, ok := cmd().(writeDoneMsg)
	if !ok {
		t.Fatalf("command did not return a writeDoneMsg")
	}
	nm, _ := m.Update(msg)
	return nm.(Model)
}

// A move shows at once and the write runs in the background: the keys keep
// working while it is pending, and a second write waits for the first.
func TestAsyncMoveDoesNotBlock(t *testing.T) {
	m, be, _ := asyncModel(t, "1,Alpha,,pending,\n2,Beta,,pending,\n")
	m, cmd := keyCmd(m, "L")
	if cmd == nil || m.writing != 1 {
		t.Fatalf("the write should be running in the background: cmd=%v writing=%d", cmd != nil, m.writing)
	}
	if v := m.View(); !strings.Contains(v, "IN_PROGRESS (1)") || !strings.Contains(v, "saving") {
		t.Fatalf("the card should already be in its new column, with a saving marker:\n%s", v)
	}
	if ts, _ := be.List(context.Background()); ts[0].Status != "pending" {
		t.Fatalf("the backend was written on the event loop: %+v", ts[0])
	}
	m = press(m, "h", "l") // navigation is not held up
	if m.col != 1 {
		t.Fatalf("keys ignored while saving: col=%d", m.col)
	}
	m, again := keyCmd(m, "L")
	if again != nil || !strings.Contains(m.notice, "still saving") {
		t.Fatalf("a second write should wait: cmd=%v notice=%q", again != nil, m.notice)
	}

	m = doneWrite(t, m, cmd)
	if m.writing != 0 || m.notice != "moved #1 to in_progress" || len(m.undo) != 1 {
		t.Fatalf("after the write: writing=%d notice=%q undo=%d", m.writing, m.notice, len(m.undo))
	}
	if ts, _ := be.List(context.Background()); ts[0].Status != "in_progress" {
		t.Fatalf("status not persisted: %+v", ts[0])
	}
}

// A write that fails puts the card back and says why.
func TestAsyncFailedMoveIsReverted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	m, _, path := asyncModel(t, "1,Alpha,,pending,\n")
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	m, cmd := keyCmd(m, "L")
	m = doneWrite(t, m, cmd)
	if !strings.Contains(m.notice, "error") || len(m.undo) != 0 {
		t.Fatalf("notice=%q undo=%d", m.notice, len(m.undo))
	}
	if v := m.View(); !strings.Contains(v, "PENDING (1)") || !strings.Contains(v, "IN_PROGRESS (0)") {
		t.Fatalf("the card should be back in its column:\n%s", v)
	}
}

func TestAsyncDeleteAndCreate(t *testing.T) {
	m, be, _ := asyncModel(t, "1,Alpha,,pending,\n2,Beta,,pending,\n")
	m, _ = keyCmd(m, "d")
	m, cmd := keyCmd(m, "y")
	if cmd == nil || strings.Contains(m.View(), "#1 Alpha") {
		t.Fatalf("the card should vanish at once: cmd=%v", cmd != nil)
	}
	m = doneWrite(t, m, cmd)
	if ts, _ := be.List(context.Background()); len(ts) != 1 {
		t.Fatalf("not deleted: %+v", ts)
	}

	m = press(m, "n")
	m = typeText(m, "Zzz")
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(Model)
	if cmd == nil || m.writing != 1 {
		t.Fatalf("create should run in the background: cmd=%v writing=%d", cmd != nil, m.writing)
	}
	m = doneWrite(t, m, cmd)
	if !strings.HasPrefix(m.notice, "created #") || !strings.Contains(m.View(), "Zzz") {
		t.Fatalf("notice=%q\n%s", m.notice, m.View())
	}
}

// The spinner turns while a list runs, so the board does not look hung.
func TestAsyncSpinnerTurns(t *testing.T) {
	be := openSet(t, writeCSV(t, "1,Alpha,,pending,\n"))
	m := NewAsync(be, "t", board.DefaultSpec(), nil)
	first := m.loadingTag()
	nm, _ := m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	if m.loadingTag() == first {
		t.Fatalf("spinner frame did not advance: %q", first)
	}
}

// Only the cards in the window are drawn, and the selection stays on screen.
func TestColumnShowsWindowAroundSelection(t *testing.T) {
	var rows strings.Builder
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&rows, "%d,Task %d,,pending,\n", i, i)
	}
	m, _, _ := asyncModel(t, rows.String())
	m.row = 300
	v := m.View()
	if !strings.Contains(v, "#301 Task 301") || strings.Contains(v, "#1 Task 1 ") || !strings.Contains(v, "↑ ") {
		t.Fatalf("window not around the selection:\n%s", v)
	}
	m.row = 0
	if v := m.View(); !strings.Contains(v, "#1 Task 1") || strings.Contains(v, "#301 Task 301") || !strings.Contains(v, "↓ ") {
		t.Fatalf("top of the column:\n%s", v)
	}
}
