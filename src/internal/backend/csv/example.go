package csvbackend

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"tkan/internal/backend"
)

func init() { backend.RegisterExample("csv", example) }

// synonyms lists header names recognised for each logical field, best first.
var synonyms = map[string][]string{
	"id":          {"id", "key", "ticket", "number", "#"},
	"title":       {"title", "summary", "name", "subject"},
	"status":      {"status", "state", "stage", "column"},
	"description": {"description", "desc", "details", "body"},
	"priority":    {"priority", "prio", "severity"},
	"notes":       {"notes", "note", "comment", "comments"},
	"type":        {"type", "kind", "category"},
	"assignee":    {"assignee", "owner", "assigned_to", "assigned"},
	"due":         {"due", "due_date", "deadline"},
	"start":       {"start", "start_date", "begin"},
	"labels":      {"labels", "label", "tags", "tag"},
}

var plainScalar = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func yamlName(s string) string {
	if plainScalar.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// example inspects the CSV at path (if any) and describes matching csv `options`.
func example(path string) (backend.Example, error) {
	var recs [][]string
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte(bom))))
			r.FieldsPerRecord = -1
			if recs, err = r.ReadAll(); err != nil {
				return backend.Example{}, fmt.Errorf("%s: %w", path, err)
			}
		case os.IsNotExist(err):
		default:
			return backend.Example{}, err
		}
	}
	if len(recs) == 0 {
		note := "no data file yet: wrote the default CSV layout (columns named like the fields)"
		if path == "" {
			note = "no data file given: wrote the default CSV layout (columns named like the fields)"
		}
		return backend.Example{Section: genericSection(), Notes: []string{note}}, nil
	}

	header := recs[0]
	assigned := map[int]string{} // header index -> logical field
	found := map[string]int{}    // logical field -> header index
	claim := func(field string, names []string) {
		if _, ok := found[field]; ok {
			return
		}
		for _, want := range names {
			for i, h := range header {
				if _, used := assigned[i]; used {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(h), want) {
					assigned[i] = field
					found[field] = i
					return
				}
			}
		}
	}
	for _, f := range logicalFields { // exact field names win over synonyms
		claim(f, []string{f})
	}
	for _, f := range logicalFields {
		claim(f, synonyms[f])
	}

	var ex backend.Example
	if len(found) == 0 {
		ex.Notes = append(ex.Notes, "no header row recognised: wrote header: false with guessed positions; check them")
		ex.Section = headerlessSection()
		return ex, nil
	}

	var b strings.Builder
	b.WriteString("source: {{source}}\nheader: true\nfields:\n")
	for _, f := range logicalFields {
		if i, ok := found[f]; ok {
			fmt.Fprintf(&b, "  %s: %s\n", f, yamlName(strings.TrimSpace(header[i])))
			continue
		}
		if required[f] {
			fmt.Fprintf(&b, "  # %s: <column name or position>   # TODO: required, not detected\n", f)
			ex.Notes = append(ex.Notes, fmt.Sprintf("required field %q not detected in the header; set options.fields.%s", f, f))
		} else {
			fmt.Fprintf(&b, "  # %s: <column name or position>   # optional, not present\n", f)
		}
	}
	var extra []string
	for i, h := range header {
		if _, ok := assigned[i]; !ok && strings.TrimSpace(h) != "" {
			extra = append(extra, strings.TrimSpace(h))
		}
	}
	if len(extra) > 0 {
		fmt.Fprintf(&b, "  # kept untouched: %s\n", strings.Join(extra, ", "))
		ex.Extras = extra
	}
	ex.Section = strings.TrimRight(b.String(), "\n")

	if si, ok := found["status"]; ok {
		seen := map[string]bool{}
		for _, row := range recs[1:] {
			if si >= len(row) {
				continue
			}
			v := row[si]
			if strings.TrimSpace(v) == "" || seen[strings.ToLower(v)] {
				continue
			}
			seen[strings.ToLower(v)] = true
			ex.Statuses = append(ex.Statuses, v)
		}
	}
	return ex, nil
}

func genericSection() string {
	var b strings.Builder
	b.WriteString("source: {{source}}     # relative to this file\n")
	b.WriteString("header: true        # false = no header row; then give every field as a 1-based position\n")
	b.WriteString("fields:             # logical field -> header NAME or 1-based POSITION\n")
	for _, f := range logicalFields {
		note := ""
		if required[f] {
			note = "   # required"
		}
		fmt.Fprintf(&b, "  %s: %s%s\n", f, f, note)
	}
	b.WriteString("# Columns not listed here are preserved untouched.")
	return b.String()
}

func headerlessSection() string {
	return "source: {{source}}\nheader: false       # TODO: no header row was recognised; check these positions\n" +
		"fields:\n  id: 1\n  title: 2\n  status: 3\n  # description: 4"
}
