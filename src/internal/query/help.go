package query

import "strings"

// Help returns the filter syntax reference shown by the TUI (`?`, then tab),
// by `tack help filter`, and mirrored in docs/filter.md. search are the fields
// plain text is matched against. What each field means is FieldsHelp, on its
// own page: this one must fit 20 lines.
func Help(search []string) string {
	if len(search) == 0 {
		search = DefaultSearch
	}
	var b strings.Builder
	b.WriteString("Filter  (/ opens the prompt, enter applies, empty clears, esc on the board clears)\n")
	b.WriteString("Terms are separated by spaces and must ALL match. Case-insensitive.\n\n")
	rows := [][2]string{
		{"text", "contains text in the searched fields"},
		{"-text", "none of the searched fields contains text"},
		{`"two words"`, "quote values that contain spaces"},
		{"field:value", "field contains value"},
		{"field!:value", "field does not contain value"},
		{"field=value", "field equals value exactly  (labels: one whole label)"},
		{"field!=value", "field is not equal to value"},
		{"field^value", "field starts with value"},
		{"field~regex", "field matches the regular expression"},
		{"field<value", "also > <= >= (numbers, dates; priority: >=high is more urgent)"},
		{"field=", "field is empty  (field!= : has a value)"},
		{"a,b", "any of the values: priority=high,medium  (!: and != : none)"},
		{"a OR b", "either side (also |); dates: today, tomorrow, today+7d (d w m y)"},
	}
	for _, r := range rows {
		b.WriteString("  " + pad(r[0], 14) + r[1] + "\n")
	}
	b.WriteString("\nSearched by plain text: " + strings.Join(sortedUniqueKeepOrder(search), ", ") + "\n")
	b.WriteString("Fields and what they mean: next page (tab)  ·  tack help filter fields  (col = column)\n")
	b.WriteString("Examples: priority=high col:doing   title:\"fix bug\" -wip   notes=   id~^A-\\d+   due<2026-12-01\n")
	return b.String()
}

func pad(s string, n int) string {
	for len([]rune(s)) < n {
		s += " "
	}
	return s
}

func sortedUniqueKeepOrder(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = Normalize(n)
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
