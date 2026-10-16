package config

import (
	"regexp"
	"strings"

	"tkan/internal/query"
)

// validKeys lists the YAML keys of each decoded Go type, for typo hints.
var validKeys = map[string]struct{ section, keys string }{
	"config.Config":    {"top level", "backend, board"},
	"board.Spec":       {"board", "columns, unmapped, search, sort, views"},
	"board.ColumnSpec": {"board.columns[]", "name, values, write, limit, color"},
	"board.ViewSpec":   {"board.views[]", "name, query"},
	"config.Entry":     {"backend[]", "type, name, options"},
}

var unknownKey = regexp.MustCompile(`field (\S+) not found in type ([\w.]+)`)

// explainUnknownKeys rewrites yaml.v3's "field X not found in type T" lines so
// they name the section, list its valid keys and suggest the nearest one.
func explainUnknownKeys(msg string) string {
	return unknownKey.ReplaceAllStringFunc(msg, func(m string) string {
		sub := unknownKey.FindStringSubmatch(m)
		info, ok := validKeys[sub[2]]
		if !ok {
			return m
		}
		keys := strings.Split(info.keys, ", ")
		return "unknown key \"" + sub[1] + "\" in " + info.section + " (valid keys: " + info.keys + ")" + query.DidYouMean(sub[1], keys)
	})
}
