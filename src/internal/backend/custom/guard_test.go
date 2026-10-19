package custom

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/task"
)

// guardScript keeps the assignee of task 7 in $PWD/state and applies an update's
// expect under a mkdir lock, like a well-behaved command should.
const guardScript = `#!/bin/sh
req=$(cat)
state="$PWD/state"
case "$req" in
  *'"op":"list"'*)
    a=$(cat "$state" 2>/dev/null)
    echo "{\"tasks\":[{\"id\":\"7\",\"title\":\"S\",\"status\":\"todo\",\"assignee\":\"$a\"}],\"guard\":true,\"fields\":[\"assignee\"]}" ;;
  *'"op":"update"'*)
    echo "$req" > "$PWD/last-update.json"
    until mkdir "$PWD/lock" 2>/dev/null; do sleep 0.01; done
    cur=$(cat "$state" 2>/dev/null)
    case "$req" in
      *'"expect"'*)
        want=$(echo "$req" | sed -n 's/.*"expect":{"field":"assignee","value":"\([^"]*\)"}.*/\1/p')
        if [ "$cur" != "$want" ]; then
          rmdir "$PWD/lock"
          echo '{"error":"assignee is taken","code":"precondition"}'
          exit 0
        fi ;;
    esac
    echo "$req" | sed -n 's/.*"assignee":"\([^"]*\)".*/\1/p' > "$state"
    rmdir "$PWD/lock"
    echo '{}' ;;
esac
`

func TestRequestCarriesVersionAndExpect(t *testing.T) {
	be, dir, err := open(t, guardScript, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := backend.WithExpect(context.Background(), "assignee", "")
	if err := be.Update(ctx, task.Task{ID: "7", Title: "S", Assignee: "me"}); err != nil {
		t.Fatal(err)
	}
	sent, _ := os.ReadFile(filepath.Join(dir, "last-update.json"))
	for _, want := range []string{`"version":1`, `"expect":{"field":"assignee","value":""}`} {
		if !strings.Contains(string(sent), want) {
			t.Errorf("request lacks %s: %s", want, sent)
		}
	}
	// no guard, no expect key
	if err := be.Update(context.Background(), task.Task{ID: "7", Title: "S", Assignee: "me"}); err != nil {
		t.Fatal(err)
	}
	sent, _ = os.ReadFile(filepath.Join(dir, "last-update.json"))
	if strings.Contains(string(sent), "expect") {
		t.Errorf("unguarded update sent expect: %s", sent)
	}
}

func TestPreconditionCodeMapsToErrPrecondition(t *testing.T) {
	be, _, err := open(t, guardScript, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Update(context.Background(), task.Task{ID: "7", Title: "S", Assignee: "a"}); err != nil {
		t.Fatal(err)
	}
	ctx := backend.WithExpect(context.Background(), "assignee", "")
	err = be.Update(ctx, task.Task{ID: "7", Title: "S", Assignee: "b"})
	if !errors.Is(err, backend.ErrPrecondition) {
		t.Fatalf("want ErrPrecondition, got %v", err)
	}
}

func TestGuardedClaimRaceHasOneWinner(t *testing.T) {
	be, _, err := open(t, guardScript, "")
	if err != nil {
		t.Fatal(err)
	}
	var wins, losses atomic.Int32
	var wg sync.WaitGroup
	for _, who := range []string{"a", "b", "c", "d", "e", "f"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := backend.WithExpect(context.Background(), "assignee", "")
			switch err := be.Update(ctx, task.Task{ID: "7", Title: "S", Assignee: who}); {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, backend.ErrPrecondition):
				losses.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 5 {
		t.Fatalf("wins %d, losses %d; want 1 and 5", wins.Load(), losses.Load())
	}
}

func TestGuardAndFieldsComeFromList(t *testing.T) {
	be, _, err := open(t, guardScript, "")
	if err != nil {
		t.Fatal(err)
	}
	g, ok := be.(backend.Guarder)
	if !ok || !g.Guards() {
		t.Fatal(`a list answering "guard":true must make Guards true`)
	}
	fs, ok := be.(backend.FieldStorer)
	if !ok || !fs.Stores("assignee") || fs.Stores("lease") {
		t.Fatal(`"fields":["assignee"] must keep assignee and drop lease`)
	}

	// An old command says neither: no atomic guard, every field kept.
	old, _, err := open(t, script, "")
	if err != nil {
		t.Fatal(err)
	}
	if old.(backend.Guarder).Guards() || !old.(backend.FieldStorer).Stores("lease") {
		t.Fatal("a command that does not report guard/fields keeps the old assumptions")
	}
}

func TestCommandFromSpec(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	be, err := backend.OpenSpec(backend.Spec{Type: "custom", Name: "tool", Command: []string{"./tool.sh"}, BaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := be.List(context.Background()); err != nil || len(tasks) != 1 {
		t.Fatalf("%v %+v", err, tasks)
	}
	_, _, err = open(t, script, "")
	if err != nil {
		t.Fatal(err)
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("command: [./tool.sh]\n"), &n); err != nil {
		t.Fatal(err)
	}
	_, err = backend.OpenSpec(backend.Spec{Type: "custom", Command: []string{"./tool.sh"}, Options: *n.Content[0], BaseDir: dir})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("got %v", err)
	}
}

func TestPatchSendsBase(t *testing.T) {
	be, dir, err := open(t, script, "")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := be.(backend.Patcher)
	if !ok {
		t.Fatal("custom must implement backend.Patcher")
	}
	base := task.Task{ID: "7", Title: "Old", Status: "todo", Backend: "tool"}
	next := base
	next.Status = "done"
	if err := p.Patch(context.Background(), base, next); err != nil {
		t.Fatal(err)
	}
	sent, _ := os.ReadFile(filepath.Join(dir, "last-update.json"))
	if !strings.Contains(string(sent), `"base":{`) || !strings.Contains(string(sent), `"status":"todo"`) || strings.Contains(string(sent), "backend") {
		t.Fatalf("request: %s", sent)
	}
}
