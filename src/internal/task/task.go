// Package task defines the backend-agnostic task model.
package task

import (
	"fmt"
	"strings"
	"time"
)

// DateLayout is the format of Due and Start.
const DateLayout = "2006-01-02"

// Status values of the default task workflow (see board.DefaultSpec). Status is
// otherwise free text: the board configuration decides what it means.
const (
	Pending    = "pending"
	InProgress = "in_progress"
	Done       = "done"
	Failed     = "failed"
)

// Task is a single backlog item.
type Task struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	Priority    string `json:"priority,omitempty"`
	Notes       string `json:"notes,omitempty"`
	Type        string `json:"type,omitempty"`
	Assignee    string `json:"assignee,omitempty"`
	Due         string `json:"due,omitempty"`    // ISO date, YYYY-MM-DD
	Start       string `json:"start,omitempty"`  // ISO date, YYYY-MM-DD
	Labels      string `json:"labels,omitempty"` // comma-separated

	// Backend names the configured backend the task belongs to. It is filled in
	// by backend.Set; stores neither read nor persist it.
	Backend string `json:"backend,omitempty"`

	// Extra holds backend-specific fields the model does not know about.
	// Backends must round-trip them untouched.
	Extra map[string]string `json:"extra,omitempty"`
}

// Clone returns a deep copy.
func (t Task) Clone() Task {
	if t.Extra != nil {
		m := make(map[string]string, len(t.Extra))
		for k, v := range t.Extra {
			m[k] = v
		}
		t.Extra = m
	}
	return t
}

// Validate checks what a new task must satisfy: a title, and Due/Start (when
// set) as YYYY-MM-DD. Status stays free text. Stores call it on Create only:
// existing rows may predate these rules and must still be movable.
func (t Task) Validate() error {
	if strings.TrimSpace(t.Title) == "" {
		return fmt.Errorf("title cannot be empty")
	}
	// A fixed order keeps the message stable when both dates are wrong. Dates
	// are trimmed like DueTime/StartTime do, so the two always agree.
	for _, d := range []struct{ name, val string }{{"due", t.Due}, {"start", t.Start}} {
		if v := strings.TrimSpace(d.val); v != "" {
			if _, err := time.Parse(DateLayout, v); err != nil {
				return fmt.Errorf("%s %q must be YYYY-MM-DD", d.name, v)
			}
		}
	}
	due, okDue := t.DueTime()
	start, okStart := t.StartTime()
	if okDue && okStart && start.After(due) {
		return fmt.Errorf("start %s is after due %s", strings.TrimSpace(t.Start), strings.TrimSpace(t.Due))
	}
	return nil
}

// LabelList splits Labels on commas, trimming spaces, dropping empty entries and
// repeats (case-insensitive; the first spelling wins), in order.
func (t Task) LabelList() []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(t.Labels, ",") {
		l = strings.TrimSpace(l)
		if k := strings.ToLower(l); l != "" && !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	return out
}

// HasLabel reports whether name is one of the labels (case-insensitive, whole label).
func (t Task) HasLabel(name string) bool {
	name = strings.TrimSpace(name)
	for _, l := range t.LabelList() {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// Normalize trims and lower-cases a free-text value (type, priority...) for
// comparing and grouping, so "Fix", "fix " and "FIX" are the same. Stored
// values are never rewritten: backends round-trip what they were given.
func Normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// UnknownPriority is the rank of empty or unrecognised priorities: after all known ones.
const UnknownPriority = 1000

// PriorityRank orders priorities from most to least urgent: 0 critical (also
// urgent, blocker, highest, p0), 1 high (p1), 2 medium (normal, p2), 3 low
// (minor, p3), 4 trivial (lowest, p4). A bare number 1..5 ranks as n-1 (1 is the
// most urgent). Anything else is UnknownPriority.
func PriorityRank(p string) int {
	switch Normalize(p) {
	case "critical", "urgent", "blocker", "highest", "p0", "1":
		return 0
	case "high", "p1", "2":
		return 1
	case "medium", "normal", "p2", "3":
		return 2
	case "low", "minor", "p3", "4":
		return 3
	case "trivial", "lowest", "p4", "5":
		return 4
	}
	return UnknownPriority
}

// DueTime parses Due (midnight UTC). ok is false when it is empty or invalid.
func (t Task) DueTime() (time.Time, bool) { return parseDate(t.Due) }

// StartTime parses Start the same way.
func (t Task) StartTime() (time.Time, bool) { return parseDate(t.Start) }

// Overdue reports whether Due is before the calendar day of now. Whether a
// finished task counts is up to the caller: status is free text here.
func (t Task) Overdue(now time.Time) bool {
	due, ok := t.DueTime()
	if !ok {
		return false
	}
	y, m, d := now.Date()
	return due.Before(time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
}

func parseDate(s string) (time.Time, bool) {
	d, err := time.Parse(DateLayout, strings.TrimSpace(s))
	return d, err == nil
}
