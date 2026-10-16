package board

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SortKey is one entry of board.sort: a field, a direction and an optional
// explicit value order. Sorting only affects how the board is displayed.
type SortKey struct {
	Field  string   // lower case, spaces as underscores (a query field name)
	Desc   bool     // descending
	Values []string // optional explicit order; unlisted values come after the listed ones
}

// UnmarshalYAML accepts "[-]field", "[-]field:v1,v2,v3" and the single-key
// mapping "[-]field: [v1, v2, v3]".
func (k *SortKey) UnmarshalYAML(n *yaml.Node) error {
	var field string
	var values []string
	switch n.Kind {
	case yaml.ScalarNode:
		field = n.Value
		if i := strings.Index(field, ":"); i >= 0 {
			for _, v := range strings.Split(field[i+1:], ",") {
				values = append(values, strings.TrimSpace(v))
			}
			field = field[:i]
		}
	case yaml.MappingNode:
		if len(n.Content) != 2 {
			return fmt.Errorf("line %d: a sort key mapping has exactly one entry: field: [v1, v2]", n.Line)
		}
		field = n.Content[0].Value
		if err := n.Content[1].Decode(&values); err != nil {
			return fmt.Errorf("line %d: sort values must be a list: %w", n.Line, err)
		}
	default:
		return fmt.Errorf("line %d: a sort key is \"[-]field[:v1,v2]\" or {field: [v1, v2]}", n.Line)
	}
	field = strings.TrimSpace(field)
	if strings.HasPrefix(field, "-") {
		k.Desc = true
		field = strings.TrimSpace(field[1:])
	}
	k.Field = normalizeField(field)
	k.Values = values
	return nil
}

func normalizeField(name string) string {
	n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "_")
	if n == "col" {
		return "column"
	}
	return n
}

// String renders the key like the config syntax ("-id", "priority:high,low").
func (k SortKey) String() string {
	s := k.Field
	if k.Desc {
		s = "-" + s
	}
	if len(k.Values) > 0 {
		s += ":" + strings.Join(k.Values, ",")
	}
	return s
}

func (s Spec) validateSort() error {
	if err := validateKeys("board.sort", s.Sort); err != nil {
		return err
	}
	for i, c := range s.Columns {
		if err := validateKeys(fmt.Sprintf("board.columns[%d].sort", i), c.Sort); err != nil {
			return err
		}
	}
	return nil
}

func validateKeys(prefix string, keys []SortKey) error {
	seen := map[string]bool{}
	for i, k := range keys {
		if k.Field == "" {
			return fmt.Errorf("%s[%d]: field name is empty", prefix, i)
		}
		if seen[k.Field] {
			return fmt.Errorf("%s: field %q is listed twice", prefix, k.Field)
		}
		seen[k.Field] = true
		for _, v := range k.Values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s[%d]: empty value in the order of %q", prefix, i, k.Field)
			}
		}
	}
	return nil
}
