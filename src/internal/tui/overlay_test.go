package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"

	"tack/internal/backend"
	"tack/internal/backend/overlay"
	"tack/internal/board"
	"tack/internal/task"
)

// overlayModel is a board on a csv without claim columns whose claims live in
// an overlay: task 2 is claimed by claude through it, task 3 is "assigned" to a
// person in the store's own (here: unrelated) priority column.
func overlayModel(t *testing.T, width int) Model {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,Alpha,pending\n2,Fix the parser,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("source: "+p+"\n"), &n); err != nil {
		t.Fatal(err)
	}
	be, err := backend.OpenSpec(backend.Spec{Type: "csv", Name: "gh", Options: *n.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	ob, err := overlay.Wrap(be, filepath.Join(dir, "ov.json"), overlay.ClaimFields)
	if err != nil {
		t.Fatal(err)
	}
	ov := `{"2": {"assignee": "claude", "lease": "2026-10-19T12:10:00Z", "claimed_at": "2026-10-19T11:55:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, "ov.json"), []byte(ov), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := board.DefaultSpec()
	spec.Agents = []string{"claude"}
	set := backend.NewSet([]backend.Named{{Name: "gh", Type: "csv", Backend: ob}})
	nm, _ := New(set, "t.csv", spec).Update(tea.WindowSizeMsg{Width: width, Height: 24})
	m := nm.(Model)
	m.now = func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }
	return m
}

func TestOverlaidValueIsMarked(t *testing.T) {
	m := overlayModel(t, 80)
	v := m.View()
	if !strings.Contains(v, "@claude"+task.OverlayMark) {
		t.Fatalf("no marker on the card:\n%s", v)
	}
	if strings.Count(v, task.OverlayMark) != 1 {
		t.Fatalf("only the overlaid card is marked:\n%s", v)
	}
	// open the card with the claim: the detail view explains the mark
	for i := 0; i < 2; i++ {
		if c, ok := m.selected(); ok && c.ID == "2" {
			break
		}
		m = press(m, "j")
	}
	if c, ok := m.selected(); !ok || c.ID != "2" {
		t.Fatalf("selected %+v", c)
	}
	d := press(m, "enter").View()
	if !strings.Contains(d, "@claude"+task.OverlayMark) || !strings.Contains(d, "local overlay") {
		t.Fatalf("detail view lacks the marker or its legend:\n%s", d)
	}
	f := press(m, "W").View()
	if !strings.Contains(f, "claude"+task.OverlayMark) {
		t.Fatalf("workers lacks the marker:\n%s", f)
	}
	for _, line := range strings.Split(f, "\n") {
		if w := len([]rune(line)); w > 82 {
			t.Fatalf("line too wide (%d): %q", w, line)
		}
	}
}
