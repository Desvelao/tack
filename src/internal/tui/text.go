package tui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// sanitize makes text that comes from outside (a command's output, an error
// message, a task title) safe to draw: escape sequences are removed, a line a
// program redrew with \r keeps only its last version, tabs become spaces and
// the other control characters are dropped. Newlines stay.
func sanitize(s string) string {
	s = ansi.Strip(strings.ReplaceAll(s, "\r\n", "\n"))
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if j := strings.LastIndex(line, "\r"); j >= 0 { // progress bars and spinners
			line = line[j+1:]
		}
		lines[i] = strings.Map(func(r rune) rune {
			switch {
			case r == '\t':
				return ' '
			case unicode.IsControl(r):
				return -1
			}
			return r
		}, line)
	}
	return strings.Join(lines, "\n")
}

// oneLine is the first line of s, sanitized, for a place that holds one line.
func oneLine(s string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(sanitize(s)), "\n")
	return strings.TrimSpace(first)
}

// cut shortens s to w terminal cells with an ellipsis (a wide character takes
// two cells, a combining one none). The result is one sanitized line; w < 1
// leaves nothing.
func cut(s string, w int) string {
	if w < 1 {
		return ""
	}
	return ansi.Truncate(oneLine(s), w, "…")
}
