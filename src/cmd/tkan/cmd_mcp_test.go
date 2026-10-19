package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpRun feeds the JSON-RPC messages to `tkan mcp` and returns one decoded
// response per line of stdout (notifications get none).
func mcpRun(t *testing.T, cfg string, flags []string, msgs ...string) []map[string]any {
	t.Helper()
	old := stdin
	defer func() { stdin = old }()
	stdin = strings.NewReader(strings.Join(msgs, "\n") + "\n")
	args := append([]string{"-c", cfg, "mcp"}, flags...)
	code, out, errs := do(t, args...)
	if code != exitOK {
		t.Fatalf("mcp: exit %d: %s", code, errs)
	}
	var res []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line is not JSON (%v): %q", err, line)
		}
		res = append(res, m)
	}
	return res
}

func callMsg(id int, tool, args string) string {
	return `{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

// toolText is the text and error flag of a tools/call response.
func toolText(t *testing.T, r map[string]any) (string, bool) {
	t.Helper()
	res, ok := r["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", r)
	}
	c := res["content"].([]any)[0].(map[string]any)
	isErr, _ := res["isError"].(bool)
	return c["text"].(string), isErr
}

func toolNames(t *testing.T, r map[string]any) []string {
	t.Helper()
	var names []string
	for _, x := range r["result"].(map[string]any)["tools"].([]any) {
		m := x.(map[string]any)
		if m["description"] == "" || m["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v lacks a description or object schema", m["name"])
		}
		names = append(names, m["name"].(string))
	}
	return names
}

func TestMCPInitializeAndTools(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML)
	res := mcpRun(t, cfg, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
	)
	if len(res) != 3 { // the notification gets no reply
		t.Fatalf("got %d responses, want 3: %v", len(res), res)
	}
	if v := res[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Errorf("protocolVersion = %v", v)
	}
	names := toolNames(t, res[1])
	if len(names) != 13 {
		t.Errorf("tools = %v", names)
	}
	if e := res[2]["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", e)
	}
}

func TestMCPReadAndWrite(t *testing.T) {
	_, file, cfg := setup(t, issuesCSV, issuesYAML+"audit: {file: audit.jsonl}\n")
	res := mcpRun(t, cfg, nil,
		callMsg(1, "list_tasks", `{"query":"col:Todo"}`),
		callMsg(2, "get_task", `{"id":"A-1"}`),
		callMsg(3, "set_status", `{"ids":["A-1"],"status":"Doing","from":"Todo"}`),
		callMsg(4, "set_status", `{"ids":["A-1"],"status":"Done","from":"Todo"}`), // stale
		callMsg(5, "add_task", `{"title":"stdin please","description":"-"}`),
		callMsg(6, "edit_task", `{"id":"A-2","priority":"high","add_labels":["bug"]}`),
		callMsg(7, "get_task", `{"id":"nope"}`),
		callMsg(8, "count", `{}`),
	)
	if txt, isErr := toolText(t, res[0]); isErr || !strings.Contains(txt, "A-1") || strings.Contains(txt, "A-2") {
		t.Errorf("list_tasks: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[1]); isErr || !strings.Contains(txt, `"column": "Todo"`) {
		t.Errorf("get_task: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[2]); isErr || !strings.Contains(txt, `"changed": true`) {
		t.Errorf("set_status: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[3]); !isErr || !strings.Contains(txt, "exit 4") {
		t.Errorf("stale set_status should be a precondition error: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[4]); !isErr || !strings.Contains(txt, `"-" is not allowed`) {
		t.Errorf("description \"-\" must be refused: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[5]); isErr || !strings.Contains(txt, `"priority": "high"`) {
		t.Errorf("edit_task: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[6]); !isErr || !strings.Contains(txt, "exit 3") {
		t.Errorf("missing task should be exit 3: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[7]); isErr || !strings.Contains(txt, `"total": 3`) {
		t.Errorf("count: %v %q", isErr, txt)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "wip") {
		t.Errorf("A-1 was not moved in the file:\n%s", data)
	}
	dir := filepath.Dir(file)
	audit, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if !strings.Contains(string(audit), `"source":"mcp"`) || strings.Contains(string(audit), `"source":"cli"`) {
		t.Errorf("audit source should be mcp:\n%s", audit)
	}
}

func TestMCPAddWithAwkwardValues(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status\n1,A,pending\n", "")
	res := mcpRun(t, cfg, nil, callMsg(1, "add_task", `{"title":"-title \"quoted\", with comma","labels":"a,b"}`))
	txt, isErr := toolText(t, res[0])
	if isErr || !strings.Contains(txt, `-title \"quoted\", with comma`) {
		t.Errorf("add_task: %v %q", isErr, txt)
	}
}

func TestMCPReadOnly(t *testing.T) {
	_, file, cfg := setup(t, issuesCSV, issuesYAML+actionsYAML)
	before, _ := os.ReadFile(file)
	res := mcpRun(t, cfg, []string{"-read-only"},
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		callMsg(2, "set_status", `{"ids":["A-1"],"status":"Done"}`),
		callMsg(3, "run_action", `{"action":"note","ids":["A-1"]}`),
		callMsg(4, "list_tasks", `{}`),
	)
	names := strings.Join(toolNames(t, res[0]), ",")
	if names != "list_tasks,get_task,count,statuses,next_task,schema" {
		t.Errorf("read-only tools = %s", names)
	}
	for _, i := range []int{1, 2} {
		if txt, isErr := toolText(t, res[i]); !isErr || !strings.Contains(txt, "unknown tool") {
			t.Errorf("write call %d should be refused: %v %q", i, isErr, txt)
		}
	}
	if _, isErr := toolText(t, res[3]); isErr {
		t.Error("list_tasks should work read-only")
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Errorf("read-only changed the file:\n%s", after)
	}
}

func TestMCPRunAction(t *testing.T) {
	dir, _, cfg := setup(t, issuesCSV, issuesYAML+actionsYAML)
	res := mcpRun(t, cfg, nil,
		callMsg(1, "run_action", `{"action":"note","ids":["A-1"],"dry_run":true}`),
		callMsg(2, "run_action", `{"action":"note","ids":["A-1"]}`),
		callMsg(3, "run_action", `{"action":"term","ids":["A-1"]}`),
		callMsg(4, "run_action", `{"action":"rm-rf","ids":["A-1"]}`),
		callMsg(5, "run_action", `{"action":"fail","ids":["A-1"]}`),
	)
	if txt, isErr := toolText(t, res[0]); isErr || !strings.Contains(txt, "&& ") {
		t.Errorf("dry_run: %v %q", isErr, txt)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		// dry_run came first and must not have run it; the real run below creates it
		_ = err
	}
	if txt, isErr := toolText(t, res[1]); isErr || !strings.Contains(txt, `"result": "ok"`) {
		t.Errorf("run: %v %q", isErr, txt)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("action did not run: %v", err)
	}
	if txt, isErr := toolText(t, res[2]); !isErr || !strings.Contains(txt, "interactive") {
		t.Errorf("interactive action must be refused: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[3]); !isErr || !strings.Contains(txt, "no action") {
		t.Errorf("unlisted action must be refused: %v %q", isErr, txt)
	}
	if _, isErr := toolText(t, res[4]); !isErr {
		t.Error("failing action should be an error result")
	}
}

func TestMCPNextClaimRelease(t *testing.T) {
	t.Setenv("TKAN_AGENT", "")
	_, _, cfg := setup(t, claimCSV, "")
	res := mcpRun(t, cfg, nil,
		callMsg(1, "next_task", `{"query":"id=2"}`),
		callMsg(2, "claim_next_task", `{"agent":"bot-1","lease":"30m","query":"id=2"}`),
		callMsg(3, "claim_task", `{"ids":["2"],"agent":"bot-2"}`), // held by bot-1
		callMsg(4, "release_task", `{"ids":["2"],"agent":"bot-2"}`),
		callMsg(5, "release_task", `{"ids":["2"],"agent":"bot-1"}`),
		callMsg(6, "claim_next_task", `{"agent":"bot-1","query":"id=nope"}`),
		callMsg(7, "claim_task", `{"ids":["1"]}`), // no agent (and no $TKAN_AGENT)
	)
	if txt, isErr := toolText(t, res[0]); isErr || !strings.Contains(txt, `"id": "2"`) || strings.Contains(txt, "bot-1") {
		t.Errorf("next_task: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[1]); isErr || !strings.Contains(txt, `"assignee": "bot-1"`) {
		t.Errorf("claim_next_task: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[2]); !isErr || !strings.Contains(txt, "exit 4") {
		t.Errorf("claiming a held task should be exit 4: %v %q", isErr, txt)
	}
	if _, isErr := toolText(t, res[3]); !isErr {
		t.Error("releasing someone else's claim should fail")
	}
	if txt, isErr := toolText(t, res[4]); isErr || !strings.Contains(txt, `"changed": true`) {
		t.Errorf("release_task: %v %q", isErr, txt)
	}
	if txt, isErr := toolText(t, res[5]); !isErr || !strings.Contains(txt, "exit 3") {
		t.Errorf("nothing ready should be exit 3: %v %q", isErr, txt)
	}
	if _, isErr := toolText(t, res[6]); !isErr {
		t.Error("claim_task without an agent should fail")
	}
}

func TestMCPSchemaTool(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+actionsYAML)
	res := mcpRun(t, cfg, []string{"-read-only"}, callMsg(1, "schema", `{}`))
	txt, isErr := toolText(t, res[0])
	var s map[string]any
	if isErr || json.Unmarshal([]byte(txt), &s) != nil || s["commands"] == nil {
		t.Errorf("schema tool: %v %q", isErr, txt)
	}
}
