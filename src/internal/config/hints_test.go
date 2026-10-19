package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"tkan/internal/action"
	"tkan/internal/actionlog"
	"tkan/internal/board"
	"tkan/internal/hook"
)

// yamlKeys lists the yaml keys of a struct type, following inlined structs.
func yamlKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		switch {
		case name == "-" || !f.IsExported():
		case strings.Contains(opts, "inline"):
			keys = append(keys, yamlKeys(f.Type)...)
		case name == "":
			keys = append(keys, strings.ToLower(f.Name)) // yaml.v3's default key
		default:
			keys = append(keys, name)
		}
	}
	return keys
}

// The typo hints must name every key the decoder accepts, so a new field
// cannot be forgotten in hints.go.
func TestValidKeysMatchTheStructs(t *testing.T) {
	types := map[string]reflect.Type{
		"config.Config":    reflect.TypeOf(Config{}),
		"config.Entry":     reflect.TypeOf(Entry{}),
		"config.AuditSpec": reflect.TypeOf(AuditSpec{}),
		"board.Spec":       reflect.TypeOf(board.Spec{}),
		"board.ColumnSpec": reflect.TypeOf(board.ColumnSpec{}),
		"board.ViewSpec":   reflect.TypeOf(board.ViewSpec{}),
		"actionlog.Spec":   reflect.TypeOf(actionlog.Spec{}),
		"action.Spec":      reflect.TypeOf(action.Spec{}),
		"hook.Spec":        reflect.TypeOf(hook.Spec{}),
	}
	// Decoded but refused by validation, so not worth suggesting.
	notSuggested := map[string][]string{"hook.Spec": {"label", "key", "mode", "show", "notify", "interactive_timeout", "claim"}}
	for name, typ := range types {
		info, ok := validKeys[name]
		if !ok {
			t.Errorf("%s has no entry in validKeys", name)
			continue
		}
		want := map[string]bool{}
		for _, k := range yamlKeys(typ) {
			want[k] = true
		}
		for _, k := range notSuggested[name] {
			delete(want, k)
		}
		got := map[string]bool{}
		for _, k := range strings.Split(info.keys, ", ") {
			got[k] = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: hints list %v, the struct has %v", name, sorted(got), sorted(want))
		}
	}
	for name := range validKeys {
		if _, ok := types[name]; !ok {
			t.Errorf("validKeys has %s, which this test does not know", name)
		}
	}
}

func sorted(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestTypoInAuditKeyGetsAHint(t *testing.T) {
	msg := explainUnknownKeys("yaml: unmarshal errors:\n  line 3: field fle not found in type config.AuditSpec")
	if !strings.Contains(msg, `unknown key "fle" in audit (valid keys: file, max_size)`) || !strings.Contains(msg, "file") {
		t.Fatal(msg)
	}
}
