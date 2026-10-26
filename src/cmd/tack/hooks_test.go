package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tack/internal/hook"
)

const hooksYAML = `hooks:
  - name: note
    on: [task.moved]
    when: status=open
    env: {TOKEN: s3cret}
    command: [sh, -c, 'echo "$0 $1" >> hooked.txt', '{id}', '{old_status}']
  - name: ping
    on: [reload.fail]
    stdin: event
    env: {URL: 'https://hooks.example.com/secret/path'}
    command: [sh, -c, 'curl -fsS -X POST --data @- "$URL"']
`

func TestHooksList(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+hooksYAML)
	code, out, errs := do(t, "-c", cfg, "hooks")
	if code != exitOK || !strings.Contains(out, "note") || !strings.Contains(out, "status=open") ||
		!strings.Contains(out, "curl -fsS -X POST") || strings.Contains(out, "secret") {
		t.Fatalf("exit %d:\n%s%s", code, out, errs)
	}
	code, out, _ = do(t, "-c", cfg, "hooks", "-json")
	if code != exitOK || !strings.Contains(out, `"name": "ping"`) || !strings.Contains(out, `"timeout": "10s"`) {
		t.Fatalf("json:\n%s", out)
	}
}

func TestHooksTest(t *testing.T) {
	t.Setenv(hook.DepthEnv, "")
	dir, _, cfg := setup(t, issuesCSV, issuesYAML+hooksYAML+"audit: {file: audit.jsonl}\n")
	code, out, errs := do(t, "-c", cfg, "hooks", "test", "note", "A-1", "-old-status", "wip", "-n")
	if code != exitOK || !strings.Contains(out, "A-1 wip") || !strings.Contains(out, "env 'TOKEN=***'") || strings.Contains(out, "s3cret") {
		t.Fatalf("dry run exit %d:\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooked.txt")); err == nil {
		t.Fatal("-n ran the hook")
	}
	if code, _, errs := do(t, "-c", cfg, "hooks", "test", "note", "A-2"); code != exitPrecondition || !strings.Contains(errs, "when:") {
		t.Fatalf("filtered exit %d: %s", code, errs)
	}
	if code, _, errs := do(t, "-c", cfg, "hooks", "test", "note", "A-1", "-old-status", "wip"); code != exitOK {
		t.Fatalf("run exit %d: %s", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hooked.txt")); string(b) != "A-1 wip\n" {
		t.Fatalf("hook output %q", b)
	}
	_, out, _ = do(t, "-c", cfg, "audit")
	if !strings.Contains(out, "hook:task.moved") || !strings.Contains(out, "note: ") {
		t.Fatalf("audit:\n%s", out)
	}
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"nope", "A-1"}, exitUsage},
		{[]string{"note"}, exitUsage},
		{[]string{"note", "A-1", "-event", "task.create"}, exitUsage},
		{[]string{"note", "Z-9"}, exitNotFound},
		{[]string{"ping", "A-1"}, exitUsage},
	} {
		if code, _, errs := do(t, append([]string{"-c", cfg, "hooks", "test"}, c.args...)...); code != c.code {
			t.Fatalf("%v: exit %d want %d: %s", c.args, code, c.code, errs)
		}
	}
	code, out, _ = do(t, "-c", cfg, "hooks", "test", "ping", "-n")
	if code != exitOK || !strings.Contains(out, "curl -fsS -X POST") || !strings.Contains(out, "stdin: event") || strings.Contains(out, "secret") {
		t.Fatalf("curl hook dry run exit %d:\n%s", code, out)
	}
}

func TestHelpEvents(t *testing.T) {
	code, out, _ := do(t, "help", "events")
	if code != exitOK || !strings.Contains(out, "task.delete.fail") || !strings.Contains(out, "{old_status}") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestWarnsOnWhenWithDeleteFail(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+`hooks:
  - name: gone
    on: [task.delete]
    when: status=open
    command: ["true"]
`)
	if _, _, errs := do(t, "-c", cfg, "count"); !strings.Contains(errs, "hooks[0] gone: when: on task.delete.fail") {
		t.Fatalf("stderr = %q", errs)
	}
}
