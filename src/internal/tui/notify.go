package tui

import (
	"io"
	"os"
	"strings"
	"unicode"

	"tack/internal/action"
)

// notify tells the user a background run of s ended, the way s.Notify asks:
// the terminal bell, or a desktop notification (OSC 777 for VTE, foot, rxvt;
// OSC 9 for iTerm2, Windows Terminal, kitty). Terminals ignore what they do not
// know. One Write, and no cursor movement, so it does not disturb the renderer.
func (m Model) notify(s action.Spec, text string) {
	var seq string
	switch s.Notify {
	case action.NotifyBell:
		seq = "\a"
	case action.NotifyDesktop:
		text = strings.ReplaceAll(notifyText(text), ";", ",") // ; separates OSC 777 fields
		seq = "\x1b]777;notify;tack;" + text + "\a" + "\x1b]9;" + text + "\a"
	default:
		return
	}
	var w io.Writer = os.Stdout
	if m.term != nil {
		w = m.term
	}
	_, _ = io.WriteString(w, seq)
}

// notifyText drops control characters (an ID or action output could end the
// escape sequence early) and keeps the text short.
func notifyText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}
