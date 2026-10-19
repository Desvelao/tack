package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tkan/internal/board"
	"tkan/internal/task"
)

func writeCSV(t *testing.T, rows string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,description,status,priority\n"+rows), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The board opens with its columns and a loading marker; cards arrive per backend.
func TestAsyncOpensEmptyThenFills(t *testing.T) {
	be := openSet(t, writeCSV(t, "1,Alpha,,pending,\n"), writeCSV(t, "1,Beta,,done,\n"))
	m := NewAsync(be, "t", board.DefaultSpec(), nil)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(Model)

	v := m.View()
	if !strings.Contains(v, "PENDING") || !strings.Contains(v, "DONE") {
		t.Fatalf("columns missing before the load:\n%s", v)
	}
	if !strings.Contains(v, "csv1") || !strings.Contains(v, "csv2") || !strings.Contains(v, "loading…") {
		t.Fatalf("loading indicators missing:\n%s", v)
	}

	ts, err := be.ListOne(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	nm, _ = m.Update(backendLoadedMsg{idx: 1, tasks: ts})
	m = nm.(Model)
	v = m.View()
	head := strings.SplitN(v, "\n", 2)[0]
	if !strings.Contains(v, "Beta") || strings.Contains(head, "csv2") || !strings.Contains(head, "csv1") {
		t.Fatalf("second backend should show its cards and drop only its own marker:\n%s", v)
	}

	ts, _ = be.ListOne(context.Background(), 0)
	nm, _ = m.Update(backendLoadedMsg{idx: 0, tasks: ts})
	m = nm.(Model)
	v = m.View()
	if m.busy() || !strings.Contains(v, "Alpha") || strings.Contains(v, "loading") {
		t.Fatalf("all loaded:\n%s", v)
	}
	if len(m.tasks) != 2 || m.tasks[0].Backend != "csv1" {
		t.Fatalf("tasks not merged in config order: %+v", m.tasks)
	}
}

// One failing backend keeps the other's cards and is named in the header.
func TestAsyncFailedBackend(t *testing.T) {
	be := openSet(t, writeCSV(t, "1,Alpha,,pending,\n"), writeCSV(t, "1,Beta,,done,\n"))
	m := NewAsync(be, "t", board.DefaultSpec(), nil)
	ts, _ := be.ListOne(context.Background(), 0)
	nm, _ := m.Update(backendLoadedMsg{idx: 0, tasks: ts})
	nm, _ = nm.Update(backendLoadedMsg{idx: 1, err: os.ErrPermission})
	m = nm.(Model)
	if m.busy() || !strings.Contains(m.View(), "Alpha") || !strings.Contains(m.failedTag(), "csv2") {
		t.Fatalf("failed backend not reported:\n%s", m.View())
	}
}

// Writes wait for the load; the first complete load runs the validation once.
func TestAsyncGuardsWritesAndValidates(t *testing.T) {
	be := openSet(t, writeCSV(t, "1,Alpha,,pending,\n"))
	calls := 0
	m := NewAsync(be, "t", board.DefaultSpec(), func([]task.Task) error { calls++; return os.ErrInvalid })
	m = press(m, "n")
	if m.mode != modeBoard || !strings.Contains(m.notice, "still loading") {
		t.Fatalf("n should be refused while loading: mode=%v notice=%q", m.mode, m.notice)
	}
	ts, _ := be.ListOne(context.Background(), 0)
	nm, _ := m.Update(backendLoadedMsg{idx: 0, tasks: ts})
	m = nm.(Model)
	if calls != 1 || !strings.Contains(m.notice, "invalid") {
		t.Fatalf("validate: calls=%d notice=%q", calls, m.notice)
	}
	if m.tickEvery() == spinEvery {
		t.Fatal("spinner tick should stop once loaded")
	}
}
