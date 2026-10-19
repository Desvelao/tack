package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An interrupted `tkan run` kills the action with its children, records the run
// and exits 130, instead of leaving an orphan editing the repository.
func TestRunInterruptKillsTheActionAndRecordsIt(t *testing.T) {
	yml := issuesYAML + `
action_log: {file: actions.jsonl}
actions:
  - name: slow
    timeout: 60s
    command: [sh, -c, 'sleep 60 & echo $! > child; echo $$ > pid; wait']
`
	dir, _, cfg := setup(t, issuesCSV, yml)
	a, err := open(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.be.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200; i++ {
			if b, _ := os.ReadFile(filepath.Join(dir, "pid")); strings.HasSuffix(string(b), "\n") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel() // what SIGINT does through signal.NotifyContext
	}()
	var out, errw bytes.Buffer
	if code := cmdRun(ctx, a, []string{"slow", "A-1"}, &out, &errw); code != exitInterrupted {
		t.Fatalf("exit %d, want %d (%q)", code, exitInterrupted, errw.String())
	}
	if !strings.Contains(errw.String(), "interrupted") {
		t.Errorf("stderr %q", errw.String())
	}
	pidText, _ := os.ReadFile(filepath.Join(dir, "pid"))
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil {
		t.Fatalf("pid file %q: %v", pidText, err)
	}
	childText, _ := os.ReadFile(filepath.Join(dir, "child"))
	child, err := strconv.Atoi(strings.TrimSpace(string(childText)))
	if err != nil {
		t.Fatalf("child file %q: %v", childText, err)
	}
	for _, p := range []int{pid, child} { // the shell and its `sleep` are gone
		for i := 0; alive(p); i++ {
			if i > 100 {
				t.Fatalf("process %d still alive", p)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	logged, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
	if !strings.Contains(string(logged), `"action":"slow"`) || !strings.Contains(string(logged), `"result":"error"`) {
		t.Fatalf("no record of the killed run: %q", logged)
	}
	entries, _ := a.audit.Entries()
	var ran bool
	for _, e := range entries {
		ran = ran || (e.Op == "run" && e.Result == "error")
	}
	if !ran {
		t.Fatalf("no audit entry: %+v", entries)
	}
}

// alive reports whether the process exists and is not a zombie (a container
// without an init never reaps orphans).
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}
