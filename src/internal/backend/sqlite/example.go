package sqlitebackend

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"tkan/internal/backend"
)

func init() { backend.RegisterExample("sqlite", example) }

var plainScalar = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func yamlName(s string) string {
	if plainScalar.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

const header = "# SQLite CLI (default: the first of sqlite3, sqlite that is installed and prints JSON)\n" +
	"# cli: sqlite3                      # or a list: [program, args...]\n" +
	"# poll: watch                       # watch (default) | none | seconds between checks\n"

// genericSection is the sample for a database that does not exist (yet).
func genericSection(source string) string {
	return "source: " + source + "     # created on the first `tkan add`, relative to this file\n" +
		"table: tasks\n" + header
}

// example inspects the database at path (if it exists) and describes matching
// sqlite `options`: the table, the column of every field and the statuses seen.
func example(path string) (backend.Example, error) {
	source := "{{source}}"
	if path == "" {
		return backend.Example{
			Section: genericSection("tasks.db"),
			Notes:   []string{"no database given: wrote the default layout (table tasks, columns named like the fields)"},
		}, nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return backend.Example{
			Section: genericSection(source),
			Notes:   []string{"no database file yet: wrote the default layout (table tasks, columns named like the fields)"},
		}, nil
	}
	cli, err := resolveCLI(nil)
	if err != nil {
		return backend.Example{Section: genericSection(source), Notes: []string{err.Error()}}, nil
	}
	b := &Backend{path: path, cli: cli, timeout: runTimeout, fields: map[string]string{}}
	res, err := b.exec(context.Background(),
		"SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name;\n")
	if err != nil {
		return backend.Example{}, fmt.Errorf("%s: %w", path, err)
	}
	var tables []string
	if len(res) > 0 {
		for _, r := range res[0] {
			tables = append(tables, text(r["name"]))
		}
	}
	if len(tables) == 0 {
		return backend.Example{
			Section: genericSection(source),
			Notes:   []string{"the database has no table yet: wrote the default layout (table tasks)"},
		}, nil
	}
	b.table = tables[0]
	for _, t := range tables {
		if strings.EqualFold(t, defaultTable) {
			b.table = t
		}
	}
	var ex backend.Example
	if len(tables) > 1 {
		ex.Notes = append(ex.Notes, fmt.Sprintf("tables: %s; using %q, set options.table to change it", strings.Join(tables, ", "), b.table))
	}
	res, err = b.exec(context.Background(), "PRAGMA table_info("+ident(b.table)+");\n")
	if err != nil {
		return backend.Example{}, fmt.Errorf("%s: %w", path, err)
	}
	var cols []string
	if len(res) > 0 {
		for _, r := range res[0] {
			cols = append(cols, text(r["name"]))
		}
	}
	assigned := map[string]string{} // column -> field
	found := map[string]string{}    // field -> column
	claim := func(field string, names []string) {
		if _, ok := found[field]; ok {
			return
		}
		for _, want := range names {
			for _, c := range cols {
				if _, used := assigned[c]; !used && strings.EqualFold(strings.TrimSpace(c), want) {
					assigned[c], found[field] = field, c
					return
				}
			}
		}
	}
	for _, f := range backend.LogicalFields {
		claim(f, []string{f})
	}
	for _, f := range backend.LogicalFields {
		claim(f, backend.Synonyms[f])
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "source: %s\ntable: %s\n%sfields:\n", source, yamlName(b.table), header)
	for _, f := range backend.LogicalFields {
		switch c, ok := found[f]; {
		case ok:
			fmt.Fprintf(&sb, "  %s: %s\n", f, yamlName(c))
		case backend.RequiredFields[f]:
			fmt.Fprintf(&sb, "  # %s: <column name>   # TODO: required, not detected\n", f)
			ex.Notes = append(ex.Notes, fmt.Sprintf("required field %q not detected in table %q; set options.fields.%s", f, b.table, f))
		default:
			fmt.Fprintf(&sb, "  # %s: <column name>   # optional, not present\n", f)
		}
	}
	for _, c := range cols {
		if _, ok := assigned[c]; !ok {
			ex.Extras = append(ex.Extras, c)
		}
	}
	if len(ex.Extras) > 0 {
		fmt.Fprintf(&sb, "  # kept untouched: %s\n", strings.Join(ex.Extras, ", "))
	}
	ex.Section = strings.TrimRight(sb.String(), "\n")

	if sc, ok := found["status"]; ok {
		res, err = b.exec(context.Background(), "SELECT "+ident(sc)+" AS s FROM "+ident(b.table)+" GROUP BY "+ident(sc)+" ORDER BY MIN(rowid);\n")
		if err == nil && len(res) > 0 {
			for _, r := range res[0] {
				if s := text(r["s"]); s != "" {
					ex.Statuses = append(ex.Statuses, s)
				}
			}
		}
	}
	return ex, nil
}
