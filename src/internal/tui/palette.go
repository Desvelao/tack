package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"tkan/internal/task"
)

// pair is a colour with a 256-colour value and an ANSI-16 fallback for each
// background, so the roles stay apart on a 16-colour console and are readable
// on light terminals too. Plain numbers are valid for every colour profile.
func pair(light256, dark256, light16, dark16 string) lipgloss.CompleteAdaptiveColor {
	return lipgloss.CompleteAdaptiveColor{
		Light: lipgloss.CompleteColor{TrueColor: light256, ANSI256: light256, ANSI: light16},
		Dark:  lipgloss.CompleteColor{TrueColor: dark256, ANSI256: dark256, ANSI: dark16},
	}
}

var (
	priorityGrey = pair("245", "245", "8", "8")
	prioColors   = []lipgloss.CompleteAdaptiveColor{
		pair("124", "196", "1", "9"),  // rank 0: critical
		pair("167", "203", "5", "13"), // rank 1: high
		pair("136", "221", "3", "11"), // rank 2: medium
		pair("28", "114", "2", "10"),  // ranks 3-4: low
	}
	// typeColors is the palette task types are coloured from.
	typeColors = []lipgloss.CompleteAdaptiveColor{
		pair("91", "141", "5", "13"),
		pair("30", "80", "6", "14"),
		pair("130", "180", "3", "11"),
		pair("131", "174", "1", "9"),
		pair("25", "110", "4", "12"),
		pair("28", "150", "2", "10"),
	}
	overdueSty = lipgloss.NewStyle().Bold(true).Foreground(pair("160", "203", "1", "9"))
	soonSty    = lipgloss.NewStyle().Foreground(pair("136", "221", "3", "11"))
)

// priorityColorOf colours the priority word on a card from its rank (see
// task.PriorityRank), so every spelling of a level looks alike; unknown values
// stay grey.
func priorityColorOf(p string) lipgloss.CompleteAdaptiveColor {
	switch r := task.PriorityRank(p); {
	case r == 0:
		return prioColors[0]
	case r == 1:
		return prioColors[1]
	case r == 2:
		return prioColors[2]
	case r <= 4:
		return prioColors[3]
	}
	return priorityGrey
}

// typeColor gives every type a stable colour (hash of its lower-case name), so
// "bug" looks the same on every card and in every run.
func typeColor(typ string) lipgloss.CompleteAdaptiveColor {
	h := 0
	for _, r := range strings.ToLower(strings.TrimSpace(typ)) {
		h = (h*31 + int(r)) % 9973
	}
	return typeColors[h%len(typeColors)]
}
