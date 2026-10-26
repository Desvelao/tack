// Package yamlbackend stores tasks in a YAML file: a list of task mappings, or
// a mapping with a `tasks` list. Keys named like the task fields are mapped
// (rename them with options.fields); every other key is kept in Task.Extra and
// written back. Dates are always written as quoted YYYY-MM-DD strings.
package yamlbackend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"tack/internal/backend"
	"tack/internal/backend/filestore"
	"tack/internal/task"
)

func init() { backend.RegisterType("yaml", Open) } // no extension: .yaml is what tack.yaml uses

// Open opens the YAML file named by spec.
func Open(spec backend.Spec) (backend.Backend, error) {
	o, path, poll, m, err := filestore.ParseOptions("yaml", spec)
	if err != nil {
		return nil, err
	}
	m.DefaultTyped("id")
	keys, _ := filestore.SplitPath("yaml", o.Path)
	indent := 2
	if o.Indent != nil && *o.Indent > 0 {
		indent = *o.Indent
	}
	return filestore.NewStore("yaml", path, poll, &codec{m: m, indent: indent, path: keys})
}

type codec struct {
	m      *filestore.Mapping
	indent int
	path   []string // keys leading to the task list; nil = the document or its "tasks" key
}

func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// root parses data and returns the document's top node (nil for an empty file).
func root(data []byte) (doc, top *yaml.Node, err error) {
	doc = &yaml.Node{}
	if err := yaml.Unmarshal(data, doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return doc, nil, nil
	}
	return doc, resolve(doc.Content[0]), nil
}

// taskList returns the sequence node holding the tasks, and the document
// mapping around it when there is one.
func taskList(top *yaml.Node, path []string) (seq *yaml.Node, err error) {
	if path != nil {
		return listAt(top, path)
	}
	switch top.Kind {
	case yaml.SequenceNode:
		return top, nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == "tasks" {
				v := resolve(top.Content[i+1])
				if v.Kind == yaml.SequenceNode {
					return v, nil
				}
				if v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null" {
					return v, nil // `tasks:` with nothing under it
				}
				return nil, fmt.Errorf(`"tasks" must be a list`)
			}
		}
		return nil, fmt.Errorf(`top-level mapping has no "tasks" list`)
	}
	return nil, fmt.Errorf("expected a list of tasks or a mapping with a \"tasks\" list")
}

// mappingGet returns the value of key in a mapping node (nil if absent).
func mappingGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// listAt follows the mapping keys in path to the task list.
func listAt(top *yaml.Node, path []string) (*yaml.Node, error) {
	cur := top
	for i, k := range path {
		if cur.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("path %q: %q is not a mapping", strings.Join(path, "."), strings.Join(path[:i], "."))
		}
		next := mappingGet(cur, k)
		if next == nil {
			if i == 0 && len(cur.Content) == 0 { // `{}`: no tasks yet
				return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}, nil
			}
			return nil, fmt.Errorf("path %q: no key %q", strings.Join(path, "."), k)
		}
		cur = resolve(next)
	}
	if cur.Kind == yaml.SequenceNode {
		return cur, nil
	}
	if cur.Kind == yaml.ScalarNode && cur.ShortTag() == "!!null" {
		return cur, nil // `tasks:` with nothing under it
	}
	return nil, fmt.Errorf("path %q: not a list of tasks", strings.Join(path, "."))
}

// setList stores seq at path below top, creating the missing mappings.
func setList(top *yaml.Node, path []string, seq *yaml.Node) {
	cur := top
	for i, k := range path {
		last := i == len(path)-1
		var idx = -1
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == k {
				idx = j + 1
				break
			}
		}
		if idx < 0 {
			cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			idx = len(cur.Content) - 1
		}
		if last {
			old := cur.Content[idx]
			seq.HeadComment, seq.FootComment = old.HeadComment, old.FootComment
			cur.Content[idx] = seq
			return
		}
		cur.Content[idx] = resolve(cur.Content[idx])
		cur = cur.Content[idx]
	}
}

// ListPaths lists the dotted paths of the lists of mappings in a YAML
// document (mappings are followed up to four levels), for `tack init`.
func ListPaths(data []byte) []string {
	_, top, err := root(data)
	if err != nil || top == nil {
		return nil
	}
	var out []string
	var walk func(m *yaml.Node, prefix []string)
	walk = func(m *yaml.Node, prefix []string) {
		if m.Kind != yaml.MappingNode || len(prefix) >= 4 {
			return
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			v := resolve(m.Content[i+1])
			p := append(append([]string(nil), prefix...), m.Content[i].Value)
			switch {
			case v.Kind == yaml.SequenceNode && len(v.Content) > 0 && resolve(v.Content[0]).Kind == yaml.MappingNode:
				out = append(out, strings.Join(p, "."))
			case v.Kind == yaml.MappingNode:
				walk(v, p)
			}
		}
	}
	walk(top, nil)
	return out
}

func fieldValue(key string, v *yaml.Node) (filestore.Field, error) {
	v = resolve(v)
	if v.Kind == yaml.ScalarNode {
		switch v.ShortTag() {
		case "!!int", "!!float", "!!bool":
			if json.Valid([]byte(v.Value)) {
				return filestore.Field{Key: key, Text: v.Value, Typed: true}, nil
			}
		case "!!null":
			return filestore.Field{Key: key, Text: "null", Typed: true}, nil
		}
		return filestore.Field{Key: key, Text: v.Value}, nil
	}
	var a any
	if err := v.Decode(&a); err != nil {
		return filestore.Field{}, err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return filestore.Field{}, fmt.Errorf("key %q: unsupported value (%v)", key, err)
	}
	return filestore.Field{Key: key, Text: string(b), Typed: true}, nil
}

// Records parses the content of a YAML file into records.
func Records(data []byte, path []string) ([]filestore.Record, error) {
	_, top, err := root(data)
	if err != nil || top == nil {
		return nil, err
	}
	seq, err := taskList(top, path)
	if err != nil {
		return nil, err
	}
	var out []filestore.Record
	if seq.Kind != yaml.SequenceNode {
		return nil, nil
	}
	for i, item := range seq.Content {
		item = resolve(item)
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("task %d: expected a mapping", i+1)
		}
		r, err := MappingRecord(item)
		if err != nil {
			return nil, fmt.Errorf("task %d: %w", i+1, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// MappingRecord reads a YAML mapping node as a record.
func MappingRecord(item *yaml.Node) (filestore.Record, error) {
	item = resolve(item)
	if item.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping")
	}
	var r filestore.Record
	for j := 0; j+1 < len(item.Content); j += 2 {
		f, err := fieldValue(item.Content[j].Value, item.Content[j+1])
		if err != nil {
			return nil, err
		}
		r = append(r, f)
	}
	return r, nil
}

// RecordNode renders a record as a YAML mapping node, keys in order.
func RecordNode(r filestore.Record) (*yaml.Node, error) {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, f := range r {
		v, err := valueNode(f)
		if err != nil {
			return nil, err
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.Key}, v)
	}
	return m, nil
}

func (c *codec) Decode(data []byte) ([]task.Task, error) {
	recs, err := Records(data, c.path)
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

func valueNode(f filestore.Field) (*yaml.Node, error) {
	n := &yaml.Node{}
	if f.Typed && json.Valid([]byte(f.Text)) {
		if filestore.IsScalarJSON(f.Text) {
			tag := "!!int"
			switch {
			case f.Text == "null":
				tag = "!!null"
			case f.Text == "true" || f.Text == "false":
				tag = "!!bool"
			case bytes.ContainsAny([]byte(f.Text), ".eE"):
				tag = "!!float"
			}
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: f.Text}, nil
		}
		var a any
		if err := json.Unmarshal([]byte(f.Text), &a); err != nil {
			return nil, err
		}
		if err := n.Encode(a); err != nil {
			return nil, err
		}
		return n, nil
	}
	if err := n.Encode(f.Text); err != nil {
		return nil, err
	}
	return n, nil
}

// oldItem is a task mapping of the file being rewritten, kept so that a task
// that did not change (or changed in a few fields) is written back with its
// comments, anchors, quoting and key order.
type oldItem struct {
	node *yaml.Node
	rec  filestore.Record
	used bool
}

func hasAlias(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}
	return false
}

// oldItems lists the plain task mappings of prev. An item that is an alias, has
// an anchor, or uses an alias or a << merge key anywhere inside is left out:
// it is rebuilt (flattened), because editing it in place would change what
// the other items that share it mean.
func (c *codec) oldItems(top *yaml.Node) []*oldItem {
	if top == nil {
		return nil
	}
	seq, err := taskList(top, c.path)
	if err != nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	var out []*oldItem
	for _, n := range seq.Content {
		if n.Kind != yaml.MappingNode || n.Anchor != "" || hasAlias(n) {
			continue
		}
		rec, err := MappingRecord(n)
		if err != nil {
			continue
		}
		out = append(out, &oldItem{node: n, rec: rec})
	}
	return out
}

func sameRecord(a, b filestore.Record) bool {
	if len(a) != len(b) {
		return false
	}
	by := make(map[string]filestore.Field, len(a))
	for _, f := range a {
		by[f.Key] = f
	}
	for _, f := range b {
		if g, ok := by[f.Key]; !ok || g != f {
			return false
		}
	}
	return len(by) == len(a)
}

// itemFor returns the node to write for rec: the old mapping of the same task
// when it is unchanged, that mapping with only the changed values replaced
// when the task (same id) was edited, else a new mapping. A "<<" key
// in the old mapping makes the edit rebuild it.
func itemFor(old []*oldItem, rec filestore.Record) (*yaml.Node, error) {
	for _, o := range old {
		if !o.used && sameRecord(o.rec, rec) {
			o.used = true
			return o.node, nil
		}
	}
	if len(rec) > 0 { // the id is the first field of a record
		for _, o := range old {
			if o.used || mappingGet(o.node, "<<") != nil {
				continue
			}
			for _, f := range o.rec {
				if f.Key == rec[0].Key && f.Text == rec[0].Text {
					o.used = true
					return editItem(o.node, o.rec, rec)
				}
			}
		}
	}
	return RecordNode(rec)
}

// editItem replaces in n (whose fields are oldRec) the values that differ from
// rec, drops the keys rec no longer has and appends the new ones.
func editItem(n *yaml.Node, oldRec, rec filestore.Record) (*yaml.Node, error) {
	want := make(map[string]filestore.Field, len(rec))
	for _, f := range rec {
		want[f.Key] = f
	}
	had := make(map[string]filestore.Field, len(oldRec))
	for _, f := range oldRec {
		had[f.Key] = f
	}
	out := *n
	out.Content = nil
	done := map[string]bool{}
	for j := 0; j+1 < len(n.Content); j += 2 {
		k, v := n.Content[j], n.Content[j+1]
		f, ok := want[k.Value]
		if !ok || done[k.Value] {
			continue
		}
		done[k.Value] = true
		if had[k.Value] != f {
			nv, err := valueNode(f)
			if err != nil {
				return nil, err
			}
			nv.HeadComment, nv.LineComment, nv.FootComment = v.HeadComment, v.LineComment, v.FootComment
			v = nv
		}
		out.Content = append(out.Content, k, v)
	}
	for _, f := range rec {
		if done[f.Key] {
			continue
		}
		nv, err := valueNode(f)
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.Key}, nv)
	}
	return &out, nil
}

func (c *codec) Encode(tasks []task.Task, prev []byte) ([]byte, error) {
	prev, bom := filestore.StripBOM(prev)
	doc, top, err := root(prev)
	if err != nil {
		return nil, err
	}
	old := c.oldItems(top)
	items := make([]*yaml.Node, 0, len(tasks))
	for _, t := range tasks {
		m, err := itemFor(old, c.m.Record(t))
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
	if len(items) == 0 {
		seq.Style = yaml.FlowStyle
	}

	// Keep the document around the list (comments, other keys) when there is one.
	switch {
	case top == nil && c.path != nil:
		root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setList(root, c.path, seq)
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	case top == nil:
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{seq}}
	case c.path != nil:
		if _, err := taskList(top, c.path); err != nil {
			return nil, err
		}
		if len(top.Content) == 0 {
			top.Style = 0 // `{}` becomes a block mapping once it holds something
		}
		setList(top, c.path, seq)
	case top.Kind == yaml.SequenceNode:
		seq.HeadComment, seq.LineComment, seq.FootComment = top.HeadComment, top.LineComment, top.FootComment
		doc.Content[0] = seq
	default:
		old, err := taskList(top, nil)
		if err != nil {
			return nil, err
		}
		seq.HeadComment, seq.FootComment = old.HeadComment, old.FootComment
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == "tasks" {
				top.Content[i+1] = seq
			}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(c.indent)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return filestore.WithBOM(buf.Bytes(), bom), nil
}
