package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"tack/internal/backend/overlay"
	"tack/internal/query"
	"tack/internal/task"
)

// Overlay is the `overlay:` of a backend entry: fields kept in a local file
// instead of the store. Either the word `claims` (assignee, lease and
// claimed_at in the default file) or a map {file, fields}.
type Overlay struct {
	File   string   `yaml:"file"`   // default .tack/overlay-<name>.json, relative to the config
	Fields []string `yaml:"fields"` // logical fields to keep locally
}

// UnmarshalYAML accepts `claims` or the map form.
func (o *Overlay) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if strings.ToLower(strings.TrimSpace(n.Value)) != "claims" {
			return fmt.Errorf("line %d: overlay %q: the only shorthand is `claims`, or give {file, fields}", n.Line, n.Value)
		}
		o.Fields = slices.Clone(overlay.ClaimFields)
		return nil
	}
	if n.Kind == yaml.MappingNode { // Decode here does not know KnownFields: check the keys
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i]; k.Value != "file" && k.Value != "fields" {
				return fmt.Errorf("line %d: unknown key %q in overlay (valid keys: file, fields)%s", k.Line, k.Value, query.DidYouMean(k.Value, []string{"file", "fields"}))
			}
		}
	}
	type plain Overlay
	return n.Decode((*plain)(o))
}

// check validates the fields.
func (o *Overlay) check() error {
	if len(o.Fields) == 0 {
		return fmt.Errorf("overlay: fields is empty (name the fields kept locally, e.g. [assignee, lease, claimed_at])")
	}
	for i, f := range o.Fields {
		f = strings.ToLower(strings.TrimSpace(f))
		o.Fields[i] = f
		if f == "id" || !slices.Contains(task.FieldNames, f) {
			return fmt.Errorf("overlay: %q cannot be overlaid (one of %s)", f, strings.Join(task.FieldNames[1:], ", "))
		}
	}
	return nil
}

// path is the overlay file for the entry called name.
func (o *Overlay) path(dir, name string) string {
	f := strings.TrimSpace(o.File)
	if f == "" {
		f = filepath.Join(".tack", "overlay-"+name+".json")
	}
	if filepath.IsAbs(f) {
		return f
	}
	return filepath.Join(dir, f)
}
