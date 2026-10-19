package query

import (
	"slices"
	"strconv"
	"strings"
)

// FieldDoc says what one filter field is. Alias is a second name that works
// the same (col for column). Name may list related fields ("due, start").
type FieldDoc struct {
	Name, Alias, Desc string
}

// TaskFieldDocs documents the fields a filter on tasks can use: the built-in
// ones, then one line for the columns or keys of the user's own file (extra
// lists every known field name; the built-in ones are taken out of it).
func TaskFieldDocs(extra []string) []FieldDoc {
	docs := []FieldDoc{
		{"id, backend", "", "task id (unique per backend); name of its backend"},
		{"title", "", "one-line summary"},
		{"description", "", "the longer text of the task"},
		{"status", "", "the raw stored value (col is the board column)"},
		{"priority", "", "free text, usually low / medium / high"},
		{"notes", "", "free text added while working on it"},
		{"type", "", "kind of task, for example fix, feature, chore"},
		{"assignee, lease, claimed_at", "", "claim: who, until when, since when"},
		{"due, start", "", "due and start dates, YYYY-MM-DD: due<today+7d"},
		{"labels", "", "comma separated; labels=ops is one whole label"},
		{"blocked_by", "", "ids of tasks to finish first, comma separated"},
		{"column", "col", "name of the board column the card is shown in"},
		{"blocked, ready", "", "yes/no: a blocked_by task is not done yet (or is)"},
		{"running", "", "yes/no: a board action is running on it (board only)"},
	}
	var own []string
	for _, f := range extra {
		if !slices.Contains(LogicalFields, f) && !slices.Contains(DerivedFields, f) && f != "column" && f != "backend" && f != RunningField {
			own = append(own, f)
		}
	}
	if len(own) > 0 {
		docs = append(docs, FieldDoc{"<your columns>", "", "the file's own columns, by their name (spaces as _):"},
			FieldDoc{"", "", nameList(own, 56)})
	}
	return docs
}

// FieldsHelp renders the fields page: what each field is, then what a plain
// word is matched against. It is one screen (24 rows, 80 columns) for the field
// sets tkan has; Desc texts must stay under 60 characters.
func FieldsHelp(title string, docs []FieldDoc, search []string) string {
	if len(search) == 0 {
		search = DefaultSearch
	}
	var b strings.Builder
	b.WriteString(title + "  (name:value, name=value, ... see the syntax page)\n\n")
	for _, d := range docs {
		name := d.Name
		if d.Alias != "" {
			name += " (" + d.Alias + ")"
		}
		b.WriteString("  " + pad(name, 18) + d.Desc + "\n")
	}
	b.WriteString("\nA plain word (no field) is looked for in: " + strings.Join(sortedUniqueKeepOrder(search), ", ") + "\n")
	return b.String()
}

// nameList joins names with commas, cut to width with the total when they do not fit.
func nameList(names []string, width int) string {
	all := strings.Join(names, ", ")
	if len(all) <= width {
		return all
	}
	var shown []string
	n := 0
	for _, nm := range names {
		if n+len(nm)+2 > width-14 {
			break
		}
		shown = append(shown, nm)
		n += len(nm) + 2
	}
	return strings.Join(shown, ", ") + ", … (" + strconv.Itoa(len(names)) + " in all)"
}
