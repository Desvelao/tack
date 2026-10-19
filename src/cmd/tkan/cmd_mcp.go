package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"tkan/internal/action"
)

// mcpDefaultVersion is the protocol revision offered when the client names none.
const mcpDefaultVersion = "2025-06-18"

// mcpTool is one entry of tools/list. run turns the call's arguments into the
// argv of an existing subcommand (always with -json), so the MCP layer shares
// every rule and exit code of the CLI.
type mcpTool struct {
	name, desc string
	write      bool // changes tasks or runs programs: hidden and refused with -read-only
	props      map[string]any
	required   []string
	argv       func(a *app, args map[string]any) ([]string, error)
}

func (t mcpTool) schema() map[string]any {
	s := map[string]any{"type": "object", "properties": t.props}
	if len(t.required) > 0 {
		s["required"] = t.required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func listProp(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// argStr returns the string argument key and whether it was given at all
// (an empty string is a value: edit_task uses it to clear a field).
func argStr(args map[string]any, key string) (string, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("argument %q must be a string", key)
	}
	return s, true, nil
}

func argList(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be a list of strings", key)
	}
	out := make([]string, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q must be a list of strings", key)
		}
		out[i] = s
	}
	return out, nil
}

func argBool(args map[string]any, key string) (bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("argument %q must be a boolean", key)
	}
	return b, nil
}

// flagArgs turns string arguments into -flag=value tokens, so a value that
// looks like a flag (or is empty) never changes how the rest is parsed.
// params maps the argument name to the flag name.
func flagArgs(args map[string]any, params [][2]string) ([]string, error) {
	var argv []string
	for _, p := range params {
		v, ok, err := argStr(args, p[0])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if v == "-" { // the CLI would read stdin, which here carries the protocol
			return nil, fmt.Errorf("argument %q: %q is not allowed", p[0], v)
		}
		argv = append(argv, "-"+p[1]+"="+v)
	}
	return argv, nil
}

// claimArgv builds `-json [flags] ID...` for claim and release.
func claimArgv(args map[string]any, params [][2]string) ([]string, error) {
	ids, err := argList(args, "ids")
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, errors.New(`argument "ids" must not be empty`)
	}
	if err := positional("ids", ids...); err != nil {
		return nil, err
	}
	argv, err := flagArgs(args, params)
	if err != nil {
		return nil, err
	}
	return append(append([]string{"-json"}, argv...), ids...), nil
}

// positional checks a value that goes on the command line without a flag.
func positional(name string, vals ...string) error {
	for _, v := range vals {
		if strings.TrimSpace(v) == "" || strings.HasPrefix(v, "-") {
			return fmt.Errorf("argument %q: %q is not a valid value", name, v)
		}
	}
	return nil
}

var (
	filterParams = [][2]string{{"status", "status"}, {"query", "q"}, {"view", "v"}}
	taskParams   = func() [][2]string {
		var p [][2]string
		for _, f := range taskFields {
			p = append(p, [2]string{strings.ReplaceAll(f.name, "-", "_"), f.name})
		}
		return p
	}()
)

func taskProps(m map[string]any) map[string]any {
	for _, f := range taskFields {
		m[strings.ReplaceAll(f.name, "-", "_")] = strProp(f.help)
	}
	m["description"] = strProp("description (plain text; '-' is not allowed)")
	m["notes"] = strProp("notes (plain text; '-' is not allowed)")
	return m
}

func mcpTools() []mcpTool {
	backendProp := strProp("backend name; needed only when the id exists in several backends (or write NAME:ID)")
	return []mcpTool{
		{
			name: "list_tasks",
			desc: "List tasks as JSON (id, backend, title, status, column, priority, ...). Filter with 'status' (column name or raw value) and/or 'query' (e.g. \"priority=high col:doing\"). Use this first to find a task id.",
			props: map[string]any{
				"status": strProp("only tasks in this column or with this raw status"),
				"query":  strProp("filter query, e.g. \"priority=high col:doing label:bug\""),
				"view":   strProp("saved view (board.views) to apply"),
				"sort":   strProp("comma-separated fields, leading - means descending, e.g. -priority,id"),
				"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "at most N tasks (0 = all)"},
			},
			argv: func(a *app, args map[string]any) ([]string, error) {
				argv, err := flagArgs(args, append(filterParams, [2]string{"sort", "sort"}))
				if err != nil {
					return nil, err
				}
				if v, ok := args["limit"]; ok && v != nil {
					n, isNum := v.(float64)
					if !isNum || n != float64(int(n)) {
						return nil, errors.New(`argument "limit" must be an integer`)
					}
					argv = append(argv, fmt.Sprintf("-limit=%d", int(n)))
				}
				return append([]string{"-json"}, argv...), nil
			},
		},
		{
			name:     "get_task",
			desc:     "Get one task by id as JSON, including its board column and all fields.",
			props:    map[string]any{"id": strProp("task id (or NAME:ID)"), "backend": backendProp},
			required: []string{"id"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				id, _, err := argStr(args, "id")
				if err != nil {
					return nil, err
				}
				if err := positional("id", id); err != nil {
					return nil, err
				}
				b, err := flagArgs(args, [][2]string{{"backend", "b"}})
				if err != nil {
					return nil, err
				}
				return append(append([]string{"-json"}, b...), id), nil
			},
		},
		{
			name: "count",
			desc: "Count tasks per group as JSON: {by, total, counts:[{key,count}]}. Group 'by' column (default), priority, type, assignee or label.",
			props: map[string]any{
				"by":     strProp("column | priority | type | assignee | label"),
				"status": strProp("only tasks in this column or raw status"),
				"query":  strProp("filter query"),
				"view":   strProp("saved view to apply"),
			},
			argv: func(a *app, args map[string]any) ([]string, error) {
				argv, err := flagArgs(args, append(filterParams, [2]string{"by", "by"}))
				return append([]string{"-json"}, argv...), err
			},
		},
		{
			name:  "statuses",
			desc:  "List the board columns and the raw status values each one holds. Call this before set_status/add_task if you do not know the valid status names.",
			props: map[string]any{},
			argv:  func(*app, map[string]any) ([]string, error) { return []string{"-json"}, nil },
		},
		{
			name:  "add_task",
			desc:  "Create a task and return it as JSON. 'title' is required; 'status' is a column name or raw value (default: the first column). Dates are YYYY-MM-DD; labels and blocked_by are comma-separated.",
			write: true,
			props: taskProps(map[string]any{
				"status":  strProp("initial column name or raw status (default: first column)"),
				"backend": strProp("backend to create the task in (default: the default backend)"),
			}),
			required: []string{"title"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				argv, err := flagArgs(args, append(taskParams, [2]string{"status", "status"}, [2]string{"backend", "b"}))
				return append([]string{"-json"}, argv...), err
			},
		},
		{
			name:  "edit_task",
			desc:  "Change only the fields given on one task and return it as JSON. An empty string clears a field. 'if' (e.g. \"status=pending\") makes the edit fail with a precondition error unless the task still has that value. add_labels/rm_labels change labels without rewriting them all. append_notes adds text after the current notes (not combinable with notes).",
			write: true,
			props: taskProps(map[string]any{
				"id":           strProp("task id (or NAME:ID)"),
				"backend":      backendProp,
				"status":       strProp("move to this column name or raw status"),
				"if":           strProp("only edit if the task still has FIELD=VALUE, e.g. status=pending"),
				"add_labels":   listProp("labels to add"),
				"rm_labels":    listProp("labels to remove"),
				"append_notes": strProp("text to append to the notes, a blank line after what is there"),
			}),
			required: []string{"id"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				id, _, err := argStr(args, "id")
				if err != nil {
					return nil, err
				}
				if err := positional("id", id); err != nil {
					return nil, err
				}
				argv, err := flagArgs(args, append(taskParams, [2]string{"status", "status"}, [2]string{"backend", "b"}, [2]string{"if", "if"}, [2]string{"append_notes", "append-notes"}))
				if err != nil {
					return nil, err
				}
				for _, l := range [][2]string{{"add_labels", "add-label"}, {"rm_labels", "rm-label"}} {
					vals, err := argList(args, l[0])
					if err != nil {
						return nil, err
					}
					for _, v := range vals {
						argv = append(argv, "-"+l[1]+"="+v)
					}
				}
				return append(append([]string{"-json"}, argv...), id), nil
			},
		},
		{
			name:  "set_status",
			desc:  "Move one or more tasks to a column/status and return them as JSON (with a 'changed' flag). 'from' makes it fail with a precondition error unless each task is still in that column, so two agents cannot move the same task.",
			write: true,
			props: map[string]any{
				"ids":     listProp("task ids (or NAME:ID)"),
				"status":  strProp("target column name or raw status; see statuses"),
				"from":    strProp("only move tasks that are still in this column/status"),
				"backend": backendProp,
			},
			required: []string{"ids", "status"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				ids, err := argList(args, "ids")
				if err != nil {
					return nil, err
				}
				status, _, err := argStr(args, "status")
				if err != nil {
					return nil, err
				}
				if len(ids) == 0 {
					return nil, errors.New(`argument "ids" must not be empty`)
				}
				if err := positional("ids", ids...); err != nil {
					return nil, err
				}
				if err := positional("status", status); err != nil {
					return nil, err
				}
				argv, err := flagArgs(args, [][2]string{{"from", "from"}, {"backend", "b"}})
				if err != nil {
					return nil, err
				}
				return append(append(append([]string{"-json"}, argv...), ids...), status), nil
			},
		},
		{
			name: "next_task",
			desc: "Get the most urgent task that is ready (not blocked) and unclaimed, as JSON, without claiming it. Narrow it with status/query/view. Exit 3 (not found) means nothing is ready.",
			props: map[string]any{
				"status": strProp("only tasks in this column or with this raw status"),
				"query":  strProp("filter query, e.g. \"type=fix\""),
				"view":   strProp("saved view (board.views) to apply"),
			},
			argv: func(a *app, args map[string]any) ([]string, error) {
				argv, err := flagArgs(args, filterParams)
				return append([]string{"-json"}, argv...), err
			},
		},
		{
			name:  "claim_next_task",
			desc:  "Pick the most urgent ready, unclaimed task and claim it for 'agent' in one step (assignee + optional lease), returning it as JSON. If another agent wins the race the next candidate is tried. Exit 3 means nothing is ready.",
			write: true,
			props: map[string]any{
				"agent":  strProp("who claims; becomes the assignee"),
				"lease":  strProp("the claim runs out after this long, e.g. 30m (default: until released)"),
				"to":     strProp("also move the task to this column/status"),
				"status": strProp("only tasks in this column or with this raw status"),
				"query":  strProp("filter query"),
				"view":   strProp("saved view to apply"),
			},
			required: []string{"agent"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				argv, err := flagArgs(args, append(filterParams, [2]string{"agent", "as"}, [2]string{"lease", "lease"}, [2]string{"to", "to"}))
				return append([]string{"-json", "-claim"}, argv...), err
			},
		},
		{
			name:  "claim_task",
			desc:  "Claim one or more tasks for 'agent' and return them as JSON. Fails with a precondition error (exit 4) when another agent holds one, so two agents cannot take the same task.",
			write: true,
			props: map[string]any{
				"ids":     listProp("task ids (or NAME:ID)"),
				"agent":   strProp("who claims; becomes the assignee"),
				"lease":   strProp("the claim runs out after this long, e.g. 30m (default: until released)"),
				"status":  strProp("also move the tasks to this column/status"),
				"backend": backendProp,
			},
			required: []string{"ids", "agent"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				return claimArgv(args, [][2]string{{"agent", "as"}, {"lease", "lease"}, {"status", "status"}, {"backend", "b"}})
			},
		},
		{
			name:  "release_task",
			desc:  "Give back a claim held by 'agent' (or any claim with force) and return the tasks as JSON with a 'changed' flag.",
			write: true,
			props: map[string]any{
				"ids":     listProp("task ids (or NAME:ID)"),
				"agent":   strProp("who holds the claim"),
				"force":   boolProp("release whoever holds it (instead of agent)"),
				"status":  strProp("also move the tasks to this column/status"),
				"backend": backendProp,
			},
			required: []string{"ids"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				argv, err := claimArgv(args, [][2]string{{"agent", "as"}, {"status", "status"}, {"backend", "b"}})
				if err != nil {
					return nil, err
				}
				force, err := argBool(args, "force")
				if force {
					argv = append([]string{"-force"}, argv...)
				}
				return argv, err
			},
		},
		{
			name:  "schema",
			desc:  "Describe tkan as JSON: task fields, every command with its flags, the exit codes, the filter fields (tasks, audit, runs) and the configured actions with the {placeholders} their commands can use. Call it once to learn what exists.",
			props: map[string]any{},
			argv:  func(*app, map[string]any) ([]string, error) { return nil, nil },
		},
		{
			name:  "run_action",
			desc:  "Run an action defined under `actions:` in the config on one or more tasks and return a JSON summary per run (result, exit code, log). Only configured actions are accepted. Use dry_run first to see the exact command. A blocked task is refused unless force is set.",
			write: true,
			props: map[string]any{
				"action":  strProp("action name from the config"),
				"ids":     listProp("task ids (or NAME:ID)"),
				"backend": backendProp,
				"dry_run": boolProp("only print the commands that would run"),
				"force":   boolProp("run on a blocked task too"),
				"agent":   strProp("who claims the task for an action with claim: true"),
			},
			required: []string{"action", "ids"},
			argv: func(a *app, args map[string]any) ([]string, error) {
				name, _, err := argStr(args, "action")
				if err != nil {
					return nil, err
				}
				ids, err := argList(args, "ids")
				if err != nil {
					return nil, err
				}
				if len(ids) == 0 {
					return nil, errors.New(`argument "ids" must not be empty`)
				}
				if err := positional("action", name); err != nil {
					return nil, err
				}
				if err := positional("ids", ids...); err != nil {
					return nil, err
				}
				// An interactive action would take the terminal, which is the protocol stream.
				for _, s := range a.cfg.AllActions() {
					if strings.EqualFold(s.Name, name) && s.Mode == action.ModeInteractive {
						return nil, fmt.Errorf("action %q is interactive and cannot run over MCP", s.Name)
					}
				}
				argv, err := flagArgs(args, [][2]string{{"backend", "b"}, {"agent", "as"}})
				if err != nil {
					return nil, err
				}
				for _, f := range [][2]string{{"dry_run", "n"}, {"force", "force"}} {
					on, err := argBool(args, f[0])
					if err != nil {
						return nil, err
					}
					if on {
						argv = append(argv, "-"+f[1])
					}
				}
				if !slices.Contains(argv, "-n") {
					argv = append(argv, "-json")
				}
				return append(append(argv, name), ids...), nil
			},
		},
	}
}

// mcpCommand runs the subcommand behind a tool.
var mcpCommands = map[string]func(context.Context, *app, []string, io.Writer, io.Writer) int{
	"list_tasks": cmdList,
	"get_task":   cmdShow,
	"count":      cmdCount,
	"statuses": func(_ context.Context, a *app, args []string, out, errw io.Writer) int {
		return cmdStatuses(a, args, out, errw)
	},
	"add_task":   cmdAdd,
	"edit_task":  cmdEdit,
	"set_status": cmdSetStatus,
	"run_action": cmdRun,

	"next_task":       cmdNext,
	"claim_next_task": cmdNext,
	"claim_task":      cmdClaim,
	"release_task":    cmdRelease,
}

// schema is set here: it lists the commands, which include mcp, so a
// package-level initializer would be a cycle.
func init() { mcpCommands["schema"] = cmdSchema }

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// cmdMCP serves the tools over stdio (JSON-RPC 2.0, one message per line)
// until stdin closes. Stdout carries only protocol frames.
func cmdMCP(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errw)
	readOnly := fs.Bool("read-only", false, "offer only list_tasks, get_task, count and statuses; refuse every write and run")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	if a == nil {
		return exitOK
	}
	a.audit.SetSource("mcp")
	a.source = "mcp"
	s := &mcpServer{a: a, readOnly: *readOnly, tools: mcpTools(), out: out, errw: errw}
	dec := json.NewDecoder(stdin)
	for ctx.Err() == nil {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return exitOK
			}
			s.reply(nil, nil, &rpcError{-32700, "parse error: " + err.Error()})
			return exitError // the stream is out of sync
		}
		s.handle(ctx, req)
	}
	return exitInterrupted
}

type mcpServer struct {
	a        *app
	readOnly bool
	tools    []mcpTool
	out      io.Writer
	errw     io.Writer
}

func (s *mcpServer) reply(id json.RawMessage, result any, e *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
	if err != nil {
		fmt.Fprintln(s.errw, "tkan: mcp:", err)
		return
	}
	s.out.Write(append(b, '\n'))
}

func (s *mcpServer) visible() []mcpTool {
	var v []mcpTool
	for _, t := range s.tools {
		if !(s.readOnly && t.write) {
			v = append(v, t)
		}
	}
	return v
}

func (s *mcpServer) handle(ctx context.Context, req rpcRequest) {
	notification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = mcpDefaultVersion
		}
		s.reply(req.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tkan", "version": version},
		}, nil)
	case "ping":
		s.reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.visible() {
			list = append(list, map[string]any{"name": t.name, "description": t.desc, "inputSchema": t.schema()})
		}
		s.reply(req.ID, map[string]any{"tools": list}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.reply(req.ID, nil, &rpcError{-32602, "invalid params: " + err.Error()})
			return
		}
		s.reply(req.ID, s.call(ctx, p.Name, p.Arguments), nil)
	default:
		if !notification { // notifications (initialized, cancelled, ...) get no reply
			s.reply(req.ID, nil, &rpcError{-32601, "method not found: " + req.Method})
		}
	}
}

func mcpText(text string, isErr bool) map[string]any {
	r := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if isErr {
		r["isError"] = true
	}
	return r
}

// call runs one tool. Failures are tool results (isError), so the model sees
// the message and the CLI exit code meaning.
func (s *mcpServer) call(ctx context.Context, name string, args map[string]any) map[string]any {
	var tool *mcpTool
	for _, t := range s.visible() {
		if t.name == name {
			tool = &t
			break
		}
	}
	if tool == nil {
		return mcpText(fmt.Sprintf("unknown tool %q", name), true)
	}
	argv, err := tool.argv(s.a, args)
	if err != nil {
		return mcpText("usage error (exit 2): "+err.Error(), true)
	}
	if err := s.a.be.Reload(); err != nil { // the store may have changed behind the cached lists
		fmt.Fprintln(s.errw, "tkan: mcp:", err)
	}
	var o, e bytes.Buffer
	code := mcpCommands[name](ctx, s.a, argv, &o, &e)
	text := strings.TrimSpace(o.String())
	if msg := strings.TrimSpace(e.String()); msg != "" {
		if code == exitOK && text != "" {
			text += "\n\nstderr:\n" + msg
		} else if text != "" {
			text += "\n\n" + msg
		} else {
			text = msg
		}
	}
	if code == exitOK {
		if text == "" {
			text = "ok"
		}
		return mcpText(text, false)
	}
	return mcpText(fmt.Sprintf("%s\n(exit %d: %s)", text, code, exitMeaning(code)), true)
}

func exitMeaning(code int) string {
	for _, e := range exitCodes {
		if e.Code == code {
			return e.Name
		}
	}
	return "error"
}
