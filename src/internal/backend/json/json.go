// Package jsonbackend stores tasks in a JSON file (type `json`: an array of
// task objects, or an object with a `tasks` array) or a JSON Lines file (type
// `jsonl`: one task object per line). Keys named like the task fields are
// mapped (rename them with options.fields); every other key is kept in
// Task.Extra and written back.
package jsonbackend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"tack/internal/backend"
	"tack/internal/backend/filestore"
	"tack/internal/task"
)

func init() {
	backend.RegisterType("json", func(s backend.Spec) (backend.Backend, error) { return open("json", s, false) }, ".json")
	backend.RegisterType("jsonl", func(s backend.Spec) (backend.Backend, error) { return open("jsonl", s, true) }, ".jsonl", ".ndjson")
}

func open(typ string, spec backend.Spec, lines bool) (backend.Backend, error) {
	o, path, poll, m, err := filestore.ParseOptions(typ, spec)
	if err != nil {
		return nil, err
	}
	keys, _ := filestore.SplitPath(typ, o.Path)
	if lines && keys != nil {
		return nil, fmt.Errorf("jsonl options: path is not supported (one task per line)")
	}
	indent := 2
	if o.Indent != nil {
		indent = *o.Indent
	}
	return filestore.NewStore(typ, path, poll, &codec{m: m, lines: lines, indent: indent, path: keys})
}

type codec struct {
	m      *filestore.Mapping
	lines  bool
	indent int
	path   []string // keys leading to the task list; nil = the document or its "tasks" key
}

type member struct {
	key string
	raw json.RawMessage
}

// objectMembers reads one JSON object from dec, keeping the order of its keys.
func objectMembers(dec *json.Decoder) ([]member, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object, found %v", tok)
	}
	var out []member
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, member{k.(string), raw})
	}
	if _, err := dec.Token(); err != nil { // closing }
		return nil, err
	}
	return out, nil
}

func toRecord(ms []member) (filestore.Record, error) {
	r := make(filestore.Record, 0, len(ms))
	for _, m := range ms {
		raw := bytes.TrimSpace(m.raw)
		if len(raw) > 0 && raw[0] == '"' {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, err
			}
			r = append(r, filestore.Field{Key: m.key, Text: s})
			continue
		}
		var c bytes.Buffer
		if err := json.Compact(&c, raw); err != nil {
			return nil, err
		}
		r = append(r, filestore.Field{Key: m.key, Text: c.String(), Typed: true})
	}
	return r, nil
}

// Records parses the content of a json/jsonl file into records.
func Records(data []byte, lines bool, path []string) ([]filestore.Record, error) {
	data, _ = filestore.StripBOM(data)
	if lines {
		var out []filestore.Record
		for n, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			ms, err := objectMembers(json.NewDecoder(bytes.NewReader(line)))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			r, err := toRecord(ms)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			out = append(out, r)
		}
		return out, nil
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	arr, err := tasksArray(data, path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(arr))
	if _, err := dec.Token(); err != nil { // [
		return nil, err
	}
	var out []filestore.Record
	for i := 1; dec.More(); i++ {
		ms, err := objectMembers(dec)
		if err != nil {
			return nil, fmt.Errorf("task %d: %w", i, err)
		}
		r, err := toRecord(ms)
		if err != nil {
			return nil, fmt.Errorf("task %d: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// tasksArray returns the JSON array holding the tasks: the document itself, or
// the value of the `tasks` key of a top-level object.
func tasksArray(data []byte, path []string) ([]byte, error) {
	if path != nil {
		return arrayAt(data, path)
	}
	switch data[0] {
	case '[':
		return data, nil
	case '{':
		ms, err := objectMembers(json.NewDecoder(bytes.NewReader(data)))
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if m.key == "tasks" {
				if a := bytes.TrimSpace(m.raw); len(a) > 0 && a[0] == '[' {
					return a, nil
				}
				return nil, fmt.Errorf(`"tasks" must be an array`)
			}
		}
		return nil, fmt.Errorf(`top-level object has no "tasks" array`)
	}
	return nil, fmt.Errorf("expected a JSON array of tasks or an object with a \"tasks\" array")
}

// arrayAt returns the JSON array found by following the object keys in path.
func arrayAt(data []byte, path []string) ([]byte, error) {
	cur := bytes.TrimSpace(data)
	for i, k := range path {
		if len(cur) == 0 || cur[0] != '{' {
			return nil, fmt.Errorf("path %q: %q is not an object", strings.Join(path, "."), strings.Join(path[:i], "."))
		}
		ms, err := objectMembers(json.NewDecoder(bytes.NewReader(cur)))
		if err != nil {
			return nil, err
		}
		if len(ms) == 0 && i == 0 {
			return []byte("[]"), nil // `{}`: no tasks yet
		}
		found := false
		for _, m := range ms {
			if m.key == k {
				cur, found = bytes.TrimSpace(m.raw), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("path %q: no key %q", strings.Join(path, "."), k)
		}
	}
	if len(cur) == 0 || cur[0] != '[' {
		return nil, fmt.Errorf("path %q: not a list of tasks", strings.Join(path, "."))
	}
	return cur, nil
}

// setAt returns doc with the value at path replaced by val, creating the
// missing objects. An empty doc becomes a new nested object.
func setAt(doc []byte, path []string, val []byte) ([]byte, error) {
	var members []member
	if d := bytes.TrimSpace(doc); len(d) > 0 {
		if d[0] != '{' {
			return nil, fmt.Errorf("path %q: the document is not an object", strings.Join(path, "."))
		}
		var err error
		if members, err = objectMembers(json.NewDecoder(bytes.NewReader(d))); err != nil {
			return nil, err
		}
	}
	var w bytes.Buffer
	w.WriteByte('{')
	found := false
	emit := func(key string, raw []byte) {
		if w.Len() > 1 {
			w.WriteByte(',')
		}
		k := quote(key)
		w.Write(k)
		w.WriteByte(':')
		w.Write(raw)
	}
	for _, m := range members {
		if m.key != path[0] {
			emit(m.key, m.raw)
			continue
		}
		found = true
		if len(path) == 1 {
			emit(m.key, val)
			continue
		}
		inner, err := setAt(m.raw, path[1:], val)
		if err != nil {
			return nil, err
		}
		emit(m.key, inner)
	}
	if !found {
		if len(path) == 1 {
			emit(path[0], val)
		} else {
			inner, _ := setAt(nil, path[1:], val)
			emit(path[0], inner)
		}
	}
	w.WriteByte('}')
	return w.Bytes(), nil
}

// ArrayPaths lists the dotted paths of the arrays of objects in a JSON
// document (objects are followed up to four levels), for `tack init`.
func ArrayPaths(data []byte) []string {
	var out []string
	var walk func(raw []byte, prefix []string)
	walk = func(raw []byte, prefix []string) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || raw[0] != '{' || len(prefix) >= 4 {
			return
		}
		ms, err := objectMembers(json.NewDecoder(bytes.NewReader(raw)))
		if err != nil {
			return
		}
		for _, m := range ms {
			v := bytes.TrimSpace(m.raw)
			p := append(append([]string(nil), prefix...), m.key)
			switch {
			case len(v) > 0 && v[0] == '[' && firstIsObject(v):
				out = append(out, strings.Join(p, "."))
			case len(v) > 0 && v[0] == '{':
				walk(v, p)
			}
		}
	}
	walk(data, nil)
	return out
}

func firstIsObject(arr []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(arr))
	if _, err := dec.Token(); err != nil || !dec.More() {
		return false
	}
	t, err := dec.Token()
	d, ok := t.(json.Delim)
	return err == nil && ok && d == '{'
}

func (c *codec) Decode(data []byte) ([]task.Task, error) {
	recs, err := Records(data, c.lines, c.path)
	if err != nil {
		return nil, err
	}
	if len(recs) > 0 {
		c.m.Reset()
	}
	tasks := make([]task.Task, len(recs))
	for i, r := range recs {
		tasks[i] = c.m.Task(r)
	}
	return tasks, nil
}

// quote is s as a JSON string. Unlike json.Marshal it leaves & < > as they are,
// so saving a file does not rewrite "A & B" as "A \u0026 B" in a hand-edited
// or git-tracked backlog.
func quote(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func writeRecord(buf *bytes.Buffer, r filestore.Record) {
	buf.WriteByte('{')
	for i, f := range r {
		if i > 0 {
			buf.WriteByte(',')
		}
		k := quote(f.Key)
		buf.Write(k)
		buf.WriteByte(':')
		if f.Typed && json.Valid([]byte(f.Text)) {
			buf.WriteString(f.Text)
			continue
		}
		v := quote(f.Text)
		buf.Write(v)
	}
	buf.WriteByte('}')
}

func (c *codec) Encode(tasks []task.Task, prev []byte) ([]byte, error) {
	prev, bom := filestore.StripBOM(prev)
	var out bytes.Buffer
	if c.lines {
		for _, t := range tasks {
			writeRecord(&out, c.m.Record(t))
			out.WriteByte('\n')
		}
		return filestore.WithBOM(out.Bytes(), bom), nil
	}
	var arr bytes.Buffer
	arr.WriteByte('[')
	for i, t := range tasks {
		if i > 0 {
			arr.WriteByte(',')
		}
		writeRecord(&arr, c.m.Record(t))
	}
	arr.WriteByte(']')

	doc := arr.Bytes()
	if c.path != nil {
		var err error
		if doc, err = setAt(prev, c.path, doc); err != nil {
			return nil, err
		}
		prev = nil
	}
	// Keep the shape of the document: an object with a `tasks` key stays one,
	// with its other keys.
	if p := bytes.TrimSpace(prev); len(p) > 0 && p[0] == '{' {
		ms, err := objectMembers(json.NewDecoder(bytes.NewReader(p)))
		if err != nil {
			return nil, err
		}
		var w bytes.Buffer
		w.WriteByte('{')
		for i, m := range ms {
			if i > 0 {
				w.WriteByte(',')
			}
			k := quote(m.key)
			w.Write(k)
			w.WriteByte(':')
			if m.key == "tasks" {
				w.Write(doc)
			} else {
				w.Write(m.raw)
			}
		}
		w.WriteByte('}')
		doc = w.Bytes()
	}
	if c.indent > 0 {
		var ind bytes.Buffer
		if err := json.Indent(&ind, doc, "", strings.Repeat(" ", c.indent)); err != nil {
			return nil, err
		}
		doc = ind.Bytes()
	}
	return filestore.WithBOM(append(append([]byte(nil), doc...), '\n'), bom), nil
}
