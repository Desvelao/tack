package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/board"
)

func workersModel(t *testing.T, width int) Model {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.csv")
	data := "id,title,status,assignee,lease,claimed_at\n" +
		"1,Alpha,pending,,,\n" +
		"2,Fix the parser,pending,claude,2026-10-19T12:10:00Z,2026-10-19T11:55:00Z\n" +
		"3,Beta,pending,ann,,\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.DefaultSpec()
	spec.Agents = []string{"claude", "aider"}
	spec.Avatars = map[string][]string{"claude": {"C1", "C2"}}
	nm, _ := New(openSet(t, p), "t.csv", spec).Update(tea.WindowSizeMsg{Width: width, Height: 24})
	m := nm.(Model)
	m.now = func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }
	return m
}

func TestWorkersView(t *testing.T) {
	m := press(workersModel(t, 80), "W")
	if m.mode != modeWorkers {
		t.Fatalf("mode %v", m.mode)
	}
	v := m.View()
	for _, want := range []string{"tack workers", "claude", "busy", "#2 Fix the parser", "5m", "10m left", "aider", "idle"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "C1") && !strings.Contains(v, "C2") {
		t.Fatalf("no avatar frame:\n%s", v)
	}
	if m.tickEvery() != animEvery {
		t.Fatalf("tick %v", m.tickEvery())
	}
	for _, line := range strings.Split(v, "\n") {
		if w := len([]rune(line)); w > 90 { // runes, ANSI is off in tests
			t.Fatalf("line too wide (%d): %q", w, line)
		}
	}
}

func TestWorkersAnimates(t *testing.T) {
	m := press(workersModel(t, 80), "W")
	a := m.View()
	m.now = func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, int(animEvery), time.UTC) }
	if b := m.View(); a == b {
		t.Fatal("the avatar did not change frame")
	}
}

func TestWorkersEnterGoesToTheCardAndEscReturns(t *testing.T) {
	m := press(workersModel(t, 80), "W", "enter")
	if m.mode != modeBoard {
		t.Fatalf("mode %v", m.mode)
	}
	if c, ok := m.selected(); !ok || c.ID != "2" {
		t.Fatalf("selected %+v", c)
	}
	if m = press(m, "W", "esc"); m.mode != modeBoard {
		t.Fatalf("esc: mode %v", m.mode)
	}
}

func TestWorkersEmpty(t *testing.T) {
	m, _ := newModel(t)
	if v := press(m, "W").View(); !strings.Contains(v, "nobody holds a task") {
		t.Fatal(v)
	}
}

func TestWorkersFitsNarrowScreen(t *testing.T) {
	m := press(workersModel(t, 60), "W")
	for _, line := range strings.Split(m.View(), "\n") {
		if w := len([]rune(line)); w > 62 {
			t.Fatalf("line too wide (%d): %q", w, line)
		}
	}
}
