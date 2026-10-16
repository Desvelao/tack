package filestore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"tkan/internal/backend"
	"tkan/internal/task"
)

// Field is one key/value of a stored record. When Typed is false Text is the
// plain string value; when true the value was not a string (number, bool, null,
// list, object) and Text is its JSON rendering.
type Field struct {
	Key   string
	Text  string
	Typed bool
}

// Record is a stored task as an ordered list of fields.
type Record []Field

// Mapping converts Records to Tasks and back. The record keys of the logical
// fields default to their names and can be renamed with options.fields; every
// other key is kept in Task.Extra. It also remembers what the file looked like
// (which keys held non-strings, whether labels were a list) so a write keeps
// the file's conventions. Not safe for concurrent use: the Store serialises it.
type Mapping struct {
	names        map[string]string // logical field -> record key
	byKey        map[string]string // lower-cased record key -> logical field
	typed        map[string]bool   // record keys that held non-string values
	labelsString bool              // labels were seen as a comma-separated string
	defaults     []string          // record keys written as non-strings until the file says otherwise
}

// DefaultTyped makes the record key of a logical field (e.g. "id") be written
// as a number/bool when it looks like one, unless the file holds strings there.
func (m *Mapping) DefaultTyped(field string) {
	m.defaults = append(m.defaults, m.names[field])
	m.Reset()
}

// NewMapping validates an options.fields table (logical field -> record key).
func NewMapping(typ string, fields map[string]string) (*Mapping, error) {
	m := &Mapping{names: map[string]string{}, byKey: map[string]string{}, typed: map[string]bool{}}
	known := map[string]bool{}
	for _, f := range backend.LogicalFields {
		known[f] = true
		m.names[f] = f
	}
	for f, k := range fields {
		f = strings.ToLower(strings.TrimSpace(f))
		if !known[f] {
			return nil, fmt.Errorf("%s options: unknown field %q (valid: %s)", typ, f, strings.Join(backend.LogicalFields, ", "))
		}
		if k = strings.TrimSpace(k); k == "" {
			return nil, fmt.Errorf("%s options: field %q: empty key name", typ, f)
		}
		m.names[f] = k
	}
	for _, f := range backend.LogicalFields {
		k := strings.ToLower(m.names[f])
		if prev, dup := m.byKey[k]; dup {
			return nil, fmt.Errorf("%s options: key %q is mapped to both %q and %q", typ, m.names[f], prev, f)
		}
		m.byKey[k] = f
	}
	return m, nil
}

// Reset forgets what the previous file looked like; call it before decoding.
func (m *Mapping) Reset() {
	m.typed = map[string]bool{}
	for _, k := range m.defaults {
		m.typed[k] = true
	}
	m.labelsString = false
}

// Task builds a task from a record.
func (m *Mapping) Task(r Record) task.Task {
	var t task.Task
	for _, f := range r {
		logical, ok := m.byKey[strings.ToLower(f.Key)]
		if !ok {
			if t.Extra == nil {
				t.Extra = map[string]string{}
			}
			t.Extra[f.Key] = f.Text
			if f.Typed {
				m.typed[f.Key] = true
			}
			continue
		}
		text := f.Text
		switch {
		case f.Typed && logical == "labels":
			var list []string
			if json.Unmarshal([]byte(f.Text), &list) == nil {
				text = strings.Join(list, ",")
			}
		case f.Typed && f.Text == "null":
			text = ""
		case f.Typed:
			m.typed[f.Key] = true
		case logical == "labels":
			m.labelsString = true
		case logical == "id":
			m.typed[f.Key] = false
		}
		*t.FieldPtr(logical) = text
	}
	return t
}

// Record renders a task: the logical fields in their usual order (optional ones
// only when set), then the extra keys alphabetically.
func (m *Mapping) Record(t task.Task) Record {
	var r Record
	for _, f := range backend.LogicalFields {
		v := *t.FieldPtr(f)
		if v == "" && !backend.RequiredFields[f] {
			continue
		}
		key := m.names[f]
		switch {
		case f == "labels" && !m.labelsString:
			list := t.LabelList()
			b, _ := json.Marshal(list)
			r = append(r, Field{Key: key, Text: string(b), Typed: true})
		case m.typed[key] && isScalarJSON(v):
			r = append(r, Field{Key: key, Text: v, Typed: true})
		default:
			r = append(r, Field{Key: key, Text: v})
		}
	}
	keys := make([]string, 0, len(t.Extra))
	for k := range t.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := t.Extra[k]
		if m.typed[k] && json.Valid([]byte(v)) {
			r = append(r, Field{Key: k, Text: v, Typed: true})
		} else {
			r = append(r, Field{Key: k, Text: v})
		}
	}
	return r
}

// isScalarJSON reports whether s is a JSON number, true, false or null.
func isScalarJSON(s string) bool {
	if s == "" || !json.Valid([]byte(s)) {
		return false
	}
	switch s[0] {
	case '"', '[', '{':
		return false
	}
	return true
}

// IsScalarJSON reports whether s is a JSON number, true, false or null.
func IsScalarJSON(s string) bool { return isScalarJSON(s) }

// Options are the `options:` shared by the whole-file backends.
type Options struct {
	Source string            `yaml:"source"`
	Fields map[string]string `yaml:"fields"`
	Poll   any               `yaml:"poll"`
	Indent *int              `yaml:"indent"`
	Path   string            `yaml:"path"` // json/yaml: dotted key path of the task list
}

// SplitPath splits a dotted key path ("data.tasks") into its keys; an empty
// path means the default location (nil). typ names the backend in errors.
func SplitPath(typ, p string) ([]string, error) {
	if strings.TrimSpace(p) == "" {
		return nil, nil
	}
	keys := strings.Split(p, ".")
	for i, k := range keys {
		if keys[i] = strings.TrimSpace(k); keys[i] == "" {
			return nil, fmt.Errorf("%s options: path %q has an empty key (use dots only between keys, e.g. data.tasks)", typ, p)
		}
	}
	return keys, nil
}

// ParseOptions decodes spec.Options of backend typ and returns the options, the
// resolved source path, the poll mode and the field mapping.
func ParseOptions(typ string, spec backend.Spec) (Options, string, backend.PollMode, *Mapping, error) {
	var o Options
	if spec.Options.Kind != 0 {
		if err := spec.Options.Decode(&o); err != nil {
			return o, "", backend.PollMode{}, nil, fmt.Errorf("%s options: %w", typ, err)
		}
	}
	if strings.TrimSpace(o.Source) == "" {
		return o, "", backend.PollMode{}, nil, fmt.Errorf("%s options: source is required (path of the file)", typ)
	}
	poll, err := backend.ParsePoll(typ, o.Poll)
	if err != nil {
		return o, "", poll, nil, err
	}
	m, err := NewMapping(typ, o.Fields)
	if err != nil {
		return o, "", poll, nil, err
	}
	if o.Indent != nil && (*o.Indent < 0 || *o.Indent > 8) {
		return o, "", poll, nil, fmt.Errorf("%s options: indent must be between 0 and 8", typ)
	}
	if _, err := SplitPath(typ, o.Path); err != nil {
		return o, "", poll, nil, err
	}
	return o, spec.Resolve(o.Source), poll, m, nil
}

// ExampleFromRecords builds the sample configuration for a file whose records
// were read: it matches record keys to fields (exact names first, then the
// usual synonyms) and lists the raw statuses found.
func ExampleFromRecords(typ string, recs []Record, generic string) backend.Example {
	if len(recs) == 0 {
		return backend.Example{Section: generic, Notes: []string{"no tasks in the file yet: wrote the default layout (keys named like the fields)"}}
	}
	var keys []string
	seenKey := map[string]bool{}
	for _, r := range recs {
		for _, f := range r {
			if !seenKey[f.Key] {
				seenKey[f.Key] = true
				keys = append(keys, f.Key)
			}
		}
	}
	found := map[string]string{} // field -> key
	used := map[string]bool{}
	claim := func(field string, names []string) {
		if _, ok := found[field]; ok {
			return
		}
		for _, want := range names {
			for _, k := range keys {
				if !used[k] && strings.EqualFold(strings.TrimSpace(k), want) {
					found[field], used[k] = k, true
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
	var ex backend.Example
	var b bytes.Buffer
	b.WriteString("source: {{source}}\nfields:\n")
	for _, f := range backend.LogicalFields {
		if k, ok := found[f]; ok {
			fmt.Fprintf(&b, "  %s: %s\n", f, quoteKey(k))
			continue
		}
		if backend.RequiredFields[f] {
			fmt.Fprintf(&b, "  # %s: <key>   # TODO: required, not detected\n", f)
			ex.Notes = append(ex.Notes, fmt.Sprintf("required field %q not detected in the %s keys; set options.fields.%s", f, typ, f))
		} else {
			fmt.Fprintf(&b, "  # %s: <key>   # optional, not present\n", f)
		}
	}
	var extra []string
	for _, k := range keys {
		if !used[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		fmt.Fprintf(&b, "  # kept untouched: %s\n", strings.Join(extra, ", "))
		ex.Extras = extra
	}
	ex.Section = strings.TrimRight(b.String(), "\n")
	if sk, ok := found["status"]; ok {
		seen := map[string]bool{}
		for _, r := range recs {
			for _, f := range r {
				if f.Key != sk || strings.TrimSpace(f.Text) == "" || seen[strings.ToLower(f.Text)] {
					continue
				}
				seen[strings.ToLower(f.Text)] = true
				ex.Statuses = append(ex.Statuses, f.Text)
			}
		}
	}
	return ex
}

func quoteKey(s string) string {
	s = strings.TrimSpace(s)
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Sprintf("%q", s)
		}
	}
	return s
}

// GenericSection is the sample `options` body for a backend with no data yet.
func GenericSection(comment string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "source: {{source}}     # relative to this file\n")
	if comment != "" {
		b.WriteString(comment + "\n")
	}
	b.WriteString("fields:             # logical field -> key NAME in the file (only where it differs)\n")
	for _, f := range backend.LogicalFields {
		note := ""
		if backend.RequiredFields[f] {
			note = "   # required"
		}
		fmt.Fprintf(&b, "  %s: %s%s\n", f, f, note)
	}
	b.WriteString("# Keys not listed here are preserved untouched.")
	return b.String()
}
