package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tack/internal/board"
)

// lastLine is the status line at the bottom of the board view.
func lastLine(m Model) string {
	lines := strings.Split(m.View(), "\n")
	return lines[len(lines)-1]
}

func TestWriteSuccessesAreOKNotices(t *testing.T) {
	m, _ := newModel(t)
	m = press(m, "L")
	if m.noticeKind != noticeOK || !strings.HasPrefix(lastLine(m), "OK") || !strings.Contains(lastLine(m), "moved #1") {
		t.Fatalf("move: kind %v line %q", m.noticeKind, lastLine(m))
	}
	m = press(m, "n", "N", "e", "w", "enter")
	if m.noticeKind != noticeOK || !strings.HasPrefix(lastLine(m), "OK") || !strings.Contains(lastLine(m), "created #") {
		t.Fatalf("create: kind %v line %q", m.noticeKind, lastLine(m))
	}
	m = press(m, "e", "enter")
	if m.noticeKind != noticeOK || !strings.Contains(lastLine(m), "saved #") {
		t.Fatalf("edit: kind %v line %q", m.noticeKind, lastLine(m))
	}
	m = press(m, "d", "y")
	if m.noticeKind != noticeOK || !strings.HasPrefix(lastLine(m), "OK") || !strings.Contains(lastLine(m), "deleted #") {
		t.Fatalf("delete: kind %v line %q", m.noticeKind, lastLine(m))
	}
	m = press(m, "u")
	if m.noticeKind != noticeOK || !strings.Contains(lastLine(m), "undid delete") {
		t.Fatalf("undo: kind %v line %q", m.noticeKind, lastLine(m))
	}
	// a plain hint stays a plain note
	if m = press(m, "u", "u", "u", "u", "u", "u", "u"); m.noticeKind == noticeFail {
		t.Fatalf("hint drawn as failure: %q", m.notice)
	}
}

// vanished returns a model whose backend file was removed, so every write fails.
func vanished(t *testing.T) Model {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte("id,title,status\n1,A,pending\n2,B,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(openSet(t, p), "t.csv", board.DefaultSpec())
	m.width, m.height = 80, 24
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWriteFailuresAreFAILNotices(t *testing.T) {
	for name, keys := range map[string][]string{
		"move":   {"L"},
		"delete": {"d", "y"},
		"create": {"n", "X", "enter"},
		"edit":   {"e", "enter"},
	} {
		m := press(vanished(t), keys...)
		if m.noticeKind != noticeFail {
			t.Errorf("%s: kind %v notice %q", name, m.noticeKind, m.notice)
			continue
		}
		if line := lastLine(m); !strings.HasPrefix(line, "FAIL") || strings.Contains(line, "error:") {
			t.Errorf("%s: status line %q", name, line)
		}
	}
}

func TestOKFadesOnTickButFailStaysUntilKey(t *testing.T) {
	m, _ := newModel(t)
	now := time.Now()
	m.now = func() time.Time { return now }
	m = press(m, "L")
	tick := func(m Model) Model {
		nm, _ := m.Update(tickMsg(now))
		return nm.(Model)
	}
	if m = tick(m); m.noticeKind != noticeOK {
		t.Fatal("OK vanished at once")
	}
	now = now.Add(okLifetime + time.Second)
	if m = tick(m); m.notice != "" {
		t.Fatalf("OK did not fade: %q", m.notice)
	}

	f := vanished(t)
	f.now = func() time.Time { return now }
	f = press(f, "L")
	now = now.Add(time.Hour)
	nm, _ := f.Update(tickMsg(now))
	if f = nm.(Model); f.noticeKind != noticeFail || f.notice == "" {
		t.Fatalf("FAIL faded: %q", f.notice)
	}
	if f = press(f, "j"); f.notice != "" || f.noticeKind != noticeInfo {
		t.Fatalf("key did not clear the FAIL: %q", f.notice)
	}
}

func TestLongFailureFitsOneLineAt80Columns(t *testing.T) {
	m := vanished(t)
	m = press(m, "L")
	m.fail("error: " + strings.Repeat("the backend said no because ", 12))
	line := lastLine(m)
	if utf8.RuneCountInString(line) > 80 || !strings.HasSuffix(line, "…") || !strings.HasPrefix(line, "FAIL") {
		t.Fatalf("line %d runes: %q", utf8.RuneCountInString(line), line)
	}
	if h := strings.Count(m.View(), "\n") + 1; h > 24 {
		t.Fatalf("view is %d lines high, want <= 24", h)
	}
	m.width = 5
	m.fail("error: something long")
	if line := lastLine(m); utf8.RuneCountInString(line) > 5 {
		t.Fatalf("narrow line %q", line)
	}
}

func TestReloadErrorIsNotHiddenBySuccess(t *testing.T) {
	// The write succeeds but the board cannot be re-read afterwards: show the failure.
	m := vanished(t)
	m = press(m, "r")
	if m.noticeKind != noticeFail {
		t.Fatalf("reload of a missing file: kind %v %q", m.noticeKind, m.notice)
	}
}
