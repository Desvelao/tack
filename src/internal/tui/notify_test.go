package tui

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"tkan/internal/action"
)

func quickSpec(notify string) action.Spec {
	return action.Spec{Name: "quick", Key: "a", Notify: notify, Command: []string{"sh", "-c", "echo hi"}}
}

func TestFinishedIndicatorStaysInEveryModeUntilR(t *testing.T) {
	m, _, _ := auditModel(t, filepath.Join(t.TempDir(), "audit.jsonl"))
	m = withActions(t, m, t.TempDir(), quickSpec(""))
	modes := []struct {
		name  string
		enter func(Model) Model
	}{
		{"detail", func(m Model) Model { return press(m, "enter") }},
		{"edit", func(m Model) Model { return press(m, "e") }},
		{"help", func(m Model) Model { return press(m, "?") }},
		{"audit", func(m Model) Model { return press(m, "A") }},
		{"actions", func(m Model) Model { return press(m, "x") }},
	}
	for i, md := range modes {
		var cmd tea.Cmd
		m, cmd = startRun(t, m, "a")
		m = md.enter(m)
		if m.mode == modeBoard {
			t.Fatalf("%s: did not leave the board", md.name)
		}
		m = finish(t, m, cmd)
		want := fmt.Sprintf("%d finished (R) · ", i+1)
		if got := ansi.Strip(lastLine(m)); !strings.HasPrefix(got, want) {
			t.Fatalf("%s: last line %q, want it to start with %q", md.name, got, want)
		}
		if w := ansi.StringWidth(ansi.Strip(lastLine(m))); w > m.width {
			t.Fatalf("%s: last line %d cells wide on %d", md.name, w, m.width)
		}
		m = press(m, "esc")
		if md.name == "help" || md.name == "edit" {
			m.mode = modeBoard
		}
	}
	if m.mode != modeBoard {
		t.Fatalf("mode %v, want board", m.mode)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(strings.SplitN(v, "\n", 2)[0], "5 finished (R)") {
		t.Fatalf("board title has no indicator:\n%s", v)
	}
	m = press(m, "R")
	if m.mode != modeRuns || m.unseen != 0 || strings.Contains(m.View(), "finished (R)") {
		t.Fatalf("R must clear the indicator: mode %v, unseen %d", m.mode, m.unseen)
	}
	// A run ending while the runs view is open is seen there.
	m = press(m, "esc")
	m, cmd := startRun(t, m, "a")
	m = press(m, "R")
	m = finish(t, m, cmd)
	if m.unseen != 0 {
		t.Fatalf("a run seen in the runs view counted: %d", m.unseen)
	}
}

func TestNotifyWritesBellOrDesktopNotification(t *testing.T) {
	for _, tc := range []struct{ notify, want string }{
		{"", ""},
		{"bell", "\a"},
		{"desktop", "\x1b]777;notify;tkan;quick on #1: OK\a\x1b]9;quick on #1: OK\a"},
	} {
		m, _ := newModel(t)
		m = withActions(t, m, t.TempDir(), quickSpec(tc.notify))
		var out bytes.Buffer
		m.term = &out
		m, cmd := startRun(t, m, "a")
		finish(t, m, cmd)
		if out.String() != tc.want {
			t.Errorf("notify %q wrote %q, want %q", tc.notify, out.String(), tc.want)
		}
	}
}

func TestNotifyTextCannotEndTheEscapeSequence(t *testing.T) {
	m, _ := newModel(t)
	var out bytes.Buffer
	m.term = &out
	m.notify(action.Spec{Notify: action.NotifyDesktop}, "evil\x1b]0;title\a; more\n")
	if got, want := out.String(), "\x1b]777;notify;tkan;evil]0,title, more\a\x1b]9;evil]0,title, more\a"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
