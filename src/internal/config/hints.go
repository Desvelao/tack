package config

import (
	"regexp"
	"strings"

	"tkan/internal/query"
)

// validKeys lists the YAML keys of each decoded Go type, for typo hints.
var validKeys = map[string]struct{ section, keys string }{
	"config.Config":    {"top level", "backend, board, default_backend, audit, actions, action_max_parallel, hooks, hook_max_depth, hook_max_parallel, action_log"},
	"board.Spec":       {"board", "columns, unmapped, search, sort, group, views, agents, avatars"},
	"board.ColumnSpec": {"board.columns[]", "name, values, write, limit, color, sort"},
	"board.ViewSpec":   {"board.views[]", "name, query"},
	"config.Entry":     {"backend[]", "type, name, command, disabled, options, actions, hooks"},
	"config.AuditSpec": {"audit", "file, max_size"},
	"actionlog.Spec":   {"action_log", "file, dir"},
	"hook.Spec":        {"hooks[]", "name, on, when, max_depth, max_parallel, min_interval, queue, unique, retries, retry_delay, dedupe, command, cwd, timeout, kill_grace, max_output, exclusive, stdin, env, capture, capture_max, on_success, on_failure, on_fail"},
	"action.Spec":      {"actions[]", "name, label, key, command, mode, cwd, timeout, interactive_timeout, kill_grace, max_output, exclusive, claim, stdin, show, notify, env, capture, capture_max, on_success, on_failure, on_fail"},
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
