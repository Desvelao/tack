// Package board maps raw backend status values onto the kanban columns shown
// to the user. It is independent of any backend or UI.
package board

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"tkan/internal/task"
)

// Modes for Spec.Unmapped.
const (
	Append = "append" // one extra column per unmapped raw value (default)
	Hide   = "hide"   // cards with unmapped values are not shown
	Other  = "other"  // a single trailing "other" column
)

// NoneName is the title of the extra column holding empty status values.
const NoneName = "(none)"

// ColumnSpec is one configured column.
type ColumnSpec struct {
	Name   string   `yaml:"name" json:"name"`
	Values []string `yaml:"values,omitempty" json:"values,omitempty"` // raw values shown here; default [Name]
	Write  string   `yaml:"write,omitempty" json:"write,omitempty"`   // raw value written on move; default Values[0]
	Limit  int      `yaml:"limit,omitempty" json:"limit,omitempty"`   // WIP limit: warn above this many cards; 0 = none
	Color  string   `yaml:"color,omitempty" json:"color,omitempty"`   // header/selection colour: ANSI 0-255 or #rrggbb
	// Sort overrides board.sort inside this column (display only).
	Sort []SortKey `yaml:"sort,omitempty" json:"-"`
}

// Spec is the board part of the configuration.
type Spec struct {
	Columns  []ColumnSpec `yaml:"columns,omitempty"`
	Unmapped string       `yaml:"unmapped,omitempty"`
	// Search lists the fields a plain text filter term is matched against
	// (default: id, title, description, notes). Names are checked by the caller.
	Search []string `yaml:"search,omitempty"`
	// Sort orders the cards inside each column on screen (display only; no
	// backend is touched). Keys apply in order; see SortKey.
	Sort []SortKey `yaml:"sort,omitempty"`
	// Group is the field cards are grouped by inside each column (default none;
	// `z` in the board cycles it). Checked against known fields by the caller.
	Group string `yaml:"group,omitempty"`
	// Views are named, saved filters (board.views). The query text is parsed
	// by the config loader; this package only checks the names.
	Views []ViewSpec `yaml:"views,omitempty"`
	// Agents are assignee names of AI agents (case-insensitive): the board
	// highlights the cards assigned to one of them. Display only.
	Agents []string `yaml:"agents,omitempty"`
	// Avatars give an agent (a name as in Agents) the frames of its picture in
	// the fleet view: emoji or short text, cycled while it is busy.
	Avatars map[string][]string `yaml:"avatars,omitempty"`
}

// ViewSpec is one saved filter.
type ViewSpec struct {
	Name  string `yaml:"name" json:"name"`
	Query string `yaml:"query" json:"query"`
}

// View returns the saved filter called name (case-insensitive).
func (s Spec) View(name string) (ViewSpec, bool) {
	for _, v := range s.Views {
		if key(v.Name) == key(name) {
			return v, true
		}
	}
	return ViewSpec{}, false
}

// DefaultSpec reproduces the original fixed task workflow.
func DefaultSpec() Spec {
	names := []string{"pending", "in_progress", "done", "failed"}
	s := Spec{Unmapped: Append}
	for _, n := range names {
		s.Columns = append(s.Columns, ColumnSpec{Name: n, Values: []string{n}, Write: n})
	}
	return s
}

// Column is a resolved column.
type Column struct {
	Name   string    `json:"name" yaml:"name"`
	Values []string  `json:"values" yaml:"values"`
	Write  string    `json:"write" yaml:"write"`
	Limit  int       `json:"limit,omitempty" yaml:"limit,omitempty"`
	Color  string    `json:"color,omitempty" yaml:"color,omitempty"`
	Sort   []SortKey `json:"-" yaml:"-"`
	Extra  bool      `json:"extra,omitempty" yaml:"extra,omitempty"` // created for unmapped values
}

// Board is the resolved, ordered set of columns.
type Board struct {
	Columns []Column
	byValue map[string]valueRef
}

type valueRef struct {
	col int
	raw string // canonical spelling
}

// Match is the result of Lookup.
type Match struct {
	Col  int
	Raw  string // raw value to write / compare
	Name bool   // true when the input named a column rather than a raw value
}

func key(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Validate checks names and that no raw value is claimed twice.
func (s Spec) Validate() error {
	switch s.Unmapped {
	case "", Append, Hide, Other:
	default:
		return fmt.Errorf("board.unmapped: %q is not one of append, hide, other", s.Unmapped)
	}
	for i, f := range s.Search {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("board.search[%d]: field name is empty", i)
		}
	}
	for i, a := range s.Agents {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("board.agents[%d]: name is empty", i)
		}
	}
	for name, frames := range s.Avatars {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("board.avatars: agent name is empty")
		}
		if len(frames) == 0 {
			return fmt.Errorf("board.avatars.%s: needs at least one frame", name)
		}
		for i, f := range frames {
			if strings.TrimSpace(f) == "" {
				return fmt.Errorf("board.avatars.%s[%d]: frame is empty", name, i)
			}
		}
	}
	if err := s.validateSort(); err != nil {
		return err
	}
	seenView := map[string]bool{}
	for i, v := range s.Views {
		switch {
		case strings.TrimSpace(v.Name) == "":
			return fmt.Errorf("board.views[%d]: name is required", i)
		case strings.TrimSpace(v.Query) == "":
			return fmt.Errorf("board.views[%d] %q: query is required", i, v.Name)
		case seenView[key(v.Name)]:
			return fmt.Errorf("board.views: duplicate view name %q", v.Name)
		}
		seenView[key(v.Name)] = true
	}
	names := map[string]bool{}
	values := map[string]string{}
	for i, c := range s.Columns {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("board.columns[%d]: name is required", i)
		}
		if names[key(c.Name)] {
			return fmt.Errorf("board.columns: duplicate column name %q", c.Name)
		}
		names[key(c.Name)] = true
		if err := checkColor(c.Color); err != nil {
			return fmt.Errorf("board.columns[%d] %q: %w", i, c.Name, err)
		}
		if c.Limit < 0 {
			return fmt.Errorf("board.columns[%d] %q: limit must be 0 (none) or more, got %d", i, c.Name, c.Limit)
		}
		vals := c.Values
		if len(vals) == 0 {
			vals = []string{c.Name}
		}
		if w := strings.TrimSpace(c.Write); w != "" {
			// A card moved here is stored as `write`; it must read back as this column.
			in := false
			for _, v := range vals {
				if key(v) == key(w) {
					in = true
				}
			}
			if !in {
				return fmt.Errorf("board.columns[%d] %q: write %q is not one of its values (%s)", i, c.Name, c.Write, strings.Join(vals, ", "))
			}
		}
		for _, v := range vals {
			if prev, dup := values[key(v)]; dup {
				if prev == c.Name {
					return fmt.Errorf("board.columns[%d] %q: value %q is listed twice (case and surrounding spaces are ignored)", i, c.Name, v)
				}
				return fmt.Errorf("board.columns[%d] %q: value %q is mapped to both %q and %q", i, c.Name, v, prev, c.Name)
			}
			values[key(v)] = c.Name
		}
	}
	return nil
}

// New resolves s against the raw status values present in the store.
// An empty column list falls back to DefaultSpec's columns.
func New(s Spec, present []string) (Board, error) {
	if len(s.Columns) == 0 {
		s.Columns = DefaultSpec().Columns
	}
	if err := s.Validate(); err != nil {
		return Board{}, err
	}
	b := Board{byValue: map[string]valueRef{}}
	for i, cs := range s.Columns {
		c := Column{Name: cs.Name, Values: cs.Values, Write: cs.Write, Limit: cs.Limit, Color: strings.TrimSpace(cs.Color), Sort: cs.Sort}
		if len(c.Values) == 0 {
			c.Values = []string{c.Name}
		}
		if c.Write == "" {
			c.Write = c.Values[0]
		}
		for _, v := range c.Values { // store the column's own spelling of the value
			if key(v) == key(c.Write) {
				c.Write = v
				break
			}
		}
		for _, v := range c.Values {
			b.byValue[key(v)] = valueRef{i, v}
		}
		b.Columns = append(b.Columns, c)
	}

	other := -1
	for _, raw := range present {
		k := key(raw)
		if _, ok := b.byValue[k]; ok {
			continue
		}
		switch s.Unmapped {
		case Hide:
			continue
		case Other:
			if other < 0 {
				other = len(b.Columns)
				b.Columns = append(b.Columns, Column{Name: b.freeName("other"), Write: raw, Extra: true})
			}
			b.Columns[other].Values = append(b.Columns[other].Values, raw)
			b.byValue[k] = valueRef{other, raw}
		default:
			name := raw
			if strings.TrimSpace(raw) == "" {
				name = NoneName
			}
			b.byValue[k] = valueRef{len(b.Columns), raw}
			b.Columns = append(b.Columns, Column{Name: b.freeName(name), Values: []string{raw}, Write: raw, Extra: true})
		}
	}
	return b, nil
}

// freeName returns name, suffixed with '*' until no existing column has it, so
// a generated column never shadows a configured one in Lookup.
func (b Board) freeName(name string) string {
	for taken := true; taken; {
		taken = false
		for _, c := range b.Columns {
			if key(c.Name) == key(name) {
				taken = true
				name += "*"
				break
			}
		}
	}
	return name
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// checkColor accepts "" (none), an ANSI colour number 0-255 or #rrggbb.
func checkColor(c string) error {
	c = strings.TrimSpace(c)
	if c == "" || hexColor.MatchString(c) {
		return nil
	}
	if n, err := strconv.Atoi(c); err == nil && n >= 0 && n <= 255 {
		return nil
	}
	return fmt.Errorf("color %q must be an ANSI number 0-255 or #rrggbb", c)
}

// Over reports whether n cards exceed column col's WIP limit; limit is that limit.
func (b Board) Over(col, n int) (limit int, over bool) {
	if col < 0 || col >= len(b.Columns) {
		return 0, false
	}
	limit = b.Columns[col].Limit
	return limit, limit > 0 && n > limit
}

// ColumnOf returns the column index for a raw value, or -1 if it is hidden.
func (b Board) ColumnOf(raw string) int {
	if r, ok := b.byValue[key(raw)]; ok {
		return r.col
	}
	return -1
}

// WriteValue is the raw value to store when a card is moved to column col.
func (b Board) WriteValue(col int) string {
	if col < 0 || col >= len(b.Columns) {
		return ""
	}
	return b.Columns[col].Write
}

// Lookup resolves user input: a column name (preferred) or a raw value.
func (b Board) Lookup(s string) (Match, bool) {
	k := key(s)
	for i, c := range b.Columns {
		if key(c.Name) == k {
			return Match{Col: i, Raw: c.Write, Name: true}, true
		}
	}
	if r, ok := b.byValue[k]; ok {
		return Match{Col: r.col, Raw: r.raw}, true
	}
	return Match{}, false
}

// Names lists the column names in order.
func (b Board) Names() []string {
	out := make([]string, len(b.Columns))
	for i, c := range b.Columns {
		out[i] = c.Name
	}
	return out
}

// finishedNames are column names that mean "work is over" (matched ignoring
// case, spaces, '-' and '_').
var finishedNames = map[string]bool{
	"done": true, "closed": true, "complete": true, "completed": true, "resolved": true,
	"finished": true, "shipped": true, "merged": true, "released": true, "deployed": true,
	"archived": true, "cancelled": true, "canceled": true, "wontfix": true, "wontdo": true, "rejected": true,
}

// Finished reports whether a column name looks like an end state (Done,
// Closed, Released, ...). Used to avoid flagging finished cards as overdue.
func Finished(name string) bool {
	n := strings.NewReplacer(" ", "", "-", "", "_", "", "'", "").Replace(strings.ToLower(name))
	return finishedNames[n]
}

// Deps knows which tasks still block others (see Board.Deps). The zero value
// knows nothing: Known is false and no task is blocked.
type Deps struct {
	open map[depKey]bool // every listed task -> true while it is not in a Finished column
}

type depKey struct{ backend, id string }

// Deps records, for the listed tasks, which are finished: in a column whose
// name Finished accepts (cards of hidden values count as not finished).
func (b Board) Deps(tasks []task.Task) Deps {
	d := Deps{open: make(map[depKey]bool, len(tasks))}
	for _, t := range tasks {
		c := b.ColumnOf(t.Status)
		d.open[depKey{t.Backend, t.ID}] = c < 0 || !Finished(b.Columns[c].Name)
	}
	return d
}

// Known reports whether d was built from a task list.
func (d Deps) Known() bool { return d.open != nil }

// Blockers lists the ids of t's blocked_by that still block it: tasks of the
// same backend that exist and are not finished. An id naming no listed task
// (deleted, a typo, a backend that failed to load) and t's own id do not block.
func (d Deps) Blockers(t task.Task) []string {
	var out []string
	for _, id := range t.BlockedByList() {
		if id != t.ID && d.open[depKey{t.Backend, id}] {
			out = append(out, id)
		}
	}
	return out
}

// Blocked reports whether t has any blocker left.
func (d Deps) Blocked(t task.Task) bool { return len(d.Blockers(t)) > 0 }
