package query

import (
	"strings"

	"tkan/internal/task"
)

// TaskLookup exposes a task's fields to Match. column is the board column the
// task is shown in ("" if hidden).
func TaskLookup(t task.Task, column string) Lookup {
	return func(field string) (string, bool) {
		switch field {
		case "id":
			return t.ID, true
		case "title":
			return t.Title, true
		case "description":
			return t.Description, true
		case "status":
			return t.Status, true
		case "priority":
			return t.Priority, true
		case "notes":
			return t.Notes, true
		case "type":
			return t.Type, true
		case "assignee":
			return t.Assignee, true
		case "due":
			return t.Due, true
		case "start":
			return t.Start, true
		case "labels":
			return t.Labels, true
		case "column":
			return column, true
		case "backend":
			return t.Backend, true
		}
		for k, v := range t.Extra {
			if Normalize(k) == field {
				return v, true
			}
		}
		return "", false
	}
}

// KnownFields lists every field name usable in a query for these tasks:
// the logical fields, "column", "backend", and the keys of any extra columns.
func KnownFields(tasks []task.Task) []string {
	names := append([]string(nil), LogicalFields...)
	names = append(names, "column", "backend")
	for _, t := range tasks {
		for k := range t.Extra {
			names = append(names, k)
		}
	}
	// keep the logical fields first, in their natural order
	known := names[:len(LogicalFields)+2]
	var extras []string
	for _, n := range sortedUnique(names[len(LogicalFields)+2:]) {
		if !contains(known, n) {
			extras = append(extras, n)
		}
	}
	return append(append([]string(nil), known...), extras...)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// CheckSearch validates a configured list of searchable fields.
func CheckSearch(search []string, known []string) error {
	set := map[string]bool{}
	for _, k := range known {
		set[Normalize(k)] = true
	}
	for _, s := range search {
		if !set[Normalize(s)] {
			return &UnknownFieldError{Name: s, Known: known}
		}
	}
	return nil
}

// UnknownFieldError is returned by CheckSearch.
type UnknownFieldError struct {
	Name   string
	Known  []string
	Prefix string // config key in the message; default "board.search"
}

func (e *UnknownFieldError) Error() string {
	prefix := e.Prefix
	if prefix == "" {
		prefix = "board.search"
	}
	return prefix + ": unknown field \"" + e.Name + "\" (fields: " + strings.Join(e.Known, ", ") + ")" + DidYouMean(e.Name, e.Known)
}
