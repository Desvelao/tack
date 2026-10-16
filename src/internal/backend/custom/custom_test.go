package custom

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/task"
)

// script is a tiny fake tool: it keeps one task in $DIR/state and answers the
// protocol with fixed JSON, enough to exercise every code path.
const script = `#!/bin/sh
req=$(cat)
case "$req" in
  *'"op":"list"'*)   echo '{"tasks":[{"id":"7","title":"Seven","status":"todo","extra":{"k":"v"}}]}' ;;
  *'"op":"create"'*) echo '{"task":{"id":"8","title":"New","status":"todo"}}' ;;
  *'"op":"update"'*) echo "$req" > "$PWD/last-update.json"; echo '{}' ;;
  *'"op":"delete"'*'"id":"7"'*) echo '{}' ;;
  *'"op":"delete"'*) echo '{"error":"no such task","code":"not_found"}' ;;
esac
`

func open(t *testing.T, script, extra string) (backend.Backend, string, error) {
	t.Helper()
	return openIn(t, t.TempDir(), script, extra)
}

// openIn is open with the tool installed in dir.
func openIn(t *testing.T, dir, script, extra string) (backend.Backend, string, error) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("command: [./tool.sh]\n"+extra), &n); err != nil {
		t.Fatal(err)
	}
	be, err := backend.OpenSpec(backend.Spec{Type: "custom", Name: "tool", Options: *n.Content[0], BaseDir: dir})
	return be, dir, err
}

func TestProtocolRoundTrip(t *testing.T) {
	be, dir, err := open(t, script, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tasks, err := be.List(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "7" || tasks[0].Extra["k"] != "v" {
		t.Fatalf("%v %+v", err, tasks)
	}
	if got := be.Statuses(); len(got) != 1 || got[0] != "todo" {
		t.Fatalf("statuses %v", got)
	}
	created, err := be.Create(ctx, task.Task{Title: "New", Status: "todo", Backend: "ignored"})
	if err != nil || created.ID != "8" {
		t.Fatalf("%v %+v", err, created)
	}
	tasks[0].Status = "done"
	tasks[0].Backend = "tool"
	if err := be.Update(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	sent, _ := os.ReadFile(filepath.Join(dir, "last-update.json"))
	if !strings.Contains(string(sent), `"status":"done"`) || strings.Contains(string(sent), "backend") {
		t.Fatalf("request: %s", sent)
	}
	if err := be.Delete(ctx, "7"); err != nil {
		t.Fatal(err)
	}
	if err := be.Delete(ctx, "99"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFailures(t *testing.T) {
	if _, _, err := open(t, "#!/bin/sh\necho boom >&2\nexit 3\n", ""); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("exit status: %v", err)
	}
	if _, _, err := open(t, "#!/bin/sh\necho not json\n", ""); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("bad json: %v", err)
	}
	if _, _, err := open(t, "#!/bin/sh\necho '{\"error\":\"nope\"}'\n", ""); err == nil || err.Error() != "nope" {
		t.Fatalf("structured error: %v", err)
	}
	if _, _, err := open(t, "#!/bin/sh\nsleep 5\n", "timeout: 100ms\n"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := backend.OpenSpec(backend.Spec{Type: "custom"}); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("missing command: %v", err)
	}
	if _, _, err := open(t, script, "timeout: soon\n"); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("bad timeout: %v", err)
	}
}

func TestDeclaredStatusesWin(t *testing.T) {
	be, _, err := open(t, "#!/bin/sh\necho '{\"tasks\":[],\"statuses\":[\"a\",\"b\"]}'\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(be.Statuses(), ","); got != "a,b" {
		t.Fatalf("%s", got)
	}
}

func TestExitStatusThreeIsNotFound(t *testing.T) {
	_, _, err := open(t, "#!/bin/sh\necho gone >&2\nexit 3\n", "")
	if !errors.Is(err, backend.ErrNotFound) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("got %v", err)
	}
	if _, _, err := open(t, "#!/bin/sh\necho oops >&2\nexit 2\n", ""); err == nil || errors.Is(err, backend.ErrNotFound) || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("exit 2: %v", err)
	}
}

func TestListRejectsEmptyAndDuplicateIDs(t *testing.T) {
	_, _, err := open(t, "#!/bin/sh\necho '{\"tasks\":[{\"id\":\"\",\"title\":\"x\"}]}'\n", "")
	if err == nil || !strings.Contains(err.Error(), "no id") {
		t.Fatalf("empty id: %v", err)
	}
	_, _, err = open(t, "#!/bin/sh\necho '{\"tasks\":[{\"id\":\"1\"},{\"id\":\"1\"}]}'\n", "")
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("duplicate id: %v", err)
	}
}

func TestTimeoutKillsChildrenOfTheScript(t *testing.T) {
	// The script's background child keeps the pipes open; the whole group must die promptly.
	start := time.Now()
	_, _, err := open(t, "#!/bin/sh\nsleep 30 &\nwait\n", "timeout: 200ms\n")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestOutputIsBounded(t *testing.T) {
	w := &capWriter{max: 10}
	n, err := w.Write([]byte("0123456789abcdef"))
	if n != 16 || err != nil || w.buf.String() != "0123456789" || !w.truncated {
		t.Fatalf("n=%d err=%v buf=%q truncated=%v", n, err, w.buf.String(), w.truncated)
	}
	if n, _ := w.Write([]byte("more")); n != 4 || w.buf.Len() != 10 {
		t.Fatalf("after cap: n=%d len=%d", n, w.buf.Len())
	}
}

func TestBackgroundChildDoesNotTurnSuccessIntoAnError(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	_ = pidFile
	// the wrapper exits 0 with a valid answer but leaves a child holding stdout open
	script := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\necho '{\"tasks\":[{\"id\":\"1\",\"title\":\"Ok\",\"status\":\"todo\"}]}'\n"
	start := time.Now()
	be, _, err := open(t, script, "")
	if err != nil {
		t.Fatalf("a valid answer was rejected: %v", err)
	}
	tasks, err := be.List(context.Background())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("list: %v %+v", err, tasks)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestLeftoverChildrenAreKilledAfterTheCall(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\necho '{\"tasks\":[]}'\n"
	if _, _, err := openIn(t, dir, script, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	alive := func() bool { // a killed child may linger as a zombie until init reaps it
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			return false
		}
		f := strings.Fields(string(stat[strings.LastIndex(string(stat), ")")+1:]))
		return len(f) > 0 && f[0] != "Z" && f[0] != "X"
	}
	for i := 0; i < 20 && alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if alive() {
		exec.Command("kill", "-9", pid).Run()
		t.Fatalf("background child %s survived the call", pid)
	}
}

func TestCancellationIsReportedAsCancelled(t *testing.T) {
	be, _, err := open(t, "#!/bin/sh\necho '{\"tasks\":[]}'\n", "")
	if err != nil {
		t.Fatal(err)
	}
	// swap in a slow tool and cancel the caller's context
	b := be.(*Backend)
	if _, err := b.List(context.Background()); err != nil { // consume the list Open primed
		t.Fatal(err)
	}
	b.command = []string{"sh", "-c", "sleep 30"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err = b.List(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestListRequiresATasksArray(t *testing.T) {
	for name, body := range map[string]string{
		"empty object": `{}`,
		"typo":         `{"taks":[{"id":"1","title":"x"}]}`,
		"null":         `{"tasks":null}`,
		"only status":  `{"statuses":["a"]}`,
	} {
		_, _, err := open(t, "#!/bin/sh\ncat >/dev/null\necho '"+body+"'\n", "")
		if err == nil || !strings.Contains(err.Error(), `"tasks" array`) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// an explicit empty array is a valid, empty board
	be, _, err := open(t, "#!/bin/sh\ncat >/dev/null\necho '{\"tasks\":[]}'\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := be.List(context.Background()); err != nil || len(tasks) != 0 {
		t.Fatalf("empty array: %v %+v", err, tasks)
	}
}

// counterScript answers list with a title that changes whenever $PWD/bump exists
// (it is consumed by the call), so a test controls when the "store" changes.
const counterScript = `#!/bin/sh
cat >/dev/null
echo '{"tasks":[{"id":"1","title":"'$(cat "$PWD/title")'","status":"todo"}]}'
`

func watcher(t *testing.T, be backend.Backend) backend.Watcher {
	t.Helper()
	w, ok := be.(backend.Watcher)
	if !ok {
		t.Fatal("custom backend is not a Watcher")
	}
	return w
}

func TestPollUnsetNeverReportsChanges(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "title"), []byte("a"), 0o644)
	be, _, err := openIn(t, dir, counterScript, "")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	os.WriteFile(filepath.Join(dir, "title"), []byte("b"), 0o644)
	time.Sleep(100 * time.Millisecond)
	if c, err := watcher(t, be).Changed(); c || err != nil {
		t.Fatalf("Changed = %v, %v without poll", c, err)
	}
}

func TestPollReportsOutsideChanges(t *testing.T) {
	dir := t.TempDir()
	title := filepath.Join(dir, "title")
	os.WriteFile(title, []byte("a"), 0o644)
	be, _, err := openIn(t, dir, counterScript, "poll: 50ms\n")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	w := watcher(t, be)
	if _, err := be.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Changed() // starts the poller
	time.Sleep(200 * time.Millisecond)
	if c, _ := w.Changed(); c {
		t.Fatal("reported a change although nothing changed")
	}
	os.WriteFile(title, []byte("b"), 0o644)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, err := w.Changed(); err != nil {
			t.Fatal(err)
		} else if c {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("change was not noticed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// After a reload the new state is the baseline, so no further change is reported.
	tasks, err := be.List(context.Background())
	if err != nil || tasks[0].Title != "b" {
		t.Fatalf("%v %+v", err, tasks)
	}
	time.Sleep(200 * time.Millisecond)
	if c, _ := w.Changed(); c {
		t.Fatal("still reporting a change after List")
	}
}

func TestPollSurfacesCommandFailure(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "broken")
	s := "#!/bin/sh\ncat >/dev/null\nif [ -e " + flag + " ]; then echo boom >&2; exit 1; fi\necho '{\"tasks\":[]}'\n"
	be, _, err := openIn(t, dir, s, "poll: 50ms\n")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	w := watcher(t, be)
	w.Changed()
	os.WriteFile(flag, nil, 0o644)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := w.Changed(); err != nil {
			if !strings.Contains(err.Error(), "boom") {
				t.Fatalf("error %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("poll failure not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestInvalidPollIsRejected(t *testing.T) {
	for _, v := range []string{"abc", "0s", "-1s"} {
		if _, _, err := open(t, script, "poll: "+v+"\n"); err == nil || !strings.Contains(err.Error(), "invalid poll") {
			t.Errorf("poll %q: %v", v, err)
		}
	}
}

func TestCloseStopsThePoller(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "title"), []byte("a"), 0o644)
	be, _, err := openIn(t, dir, counterScript, "poll: 50ms\n")
	if err != nil {
		t.Fatal(err)
	}
	watcher(t, be).Changed()
	done := make(chan struct{})
	go func() { be.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
}
