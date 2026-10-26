package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const resultActionsYAML = `actions:
  - name: ok
    command: [sh, -c, 'echo "run=$TACK_RUN_ID log=$TACK_ACTION_LOG"; echo more']
    capture: notes
    on_success: Done
  - name: bad
    command: [sh, -c, 'echo oops; exit 7']
    on_failure: Doing
  - name: big
    command: [sh, -c, 'head -c 6000 /dev/zero | tr "\0" x']
    capture: notes
    capture_max: 1
  - name: ghost
    command: [true]
    on_success: nowhere
`

func TestRunJSONAndPropagateExit(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+resultActionsYAML+"action_log: {file: logs/actions.jsonl}\n")
	code, out, errs := do(t, "-c", cfg, "run", "ok", "A-1", "-json")
	if code != exitOK {
		t.Fatalf("run: %d %s", code, errs)
	}
	var s runSummary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("stdout must be only the summary: %v\n%s", err, out)
	}
	if s.Result != "ok" || s.RunID == "" || s.ExitCode == nil || *s.ExitCode != 0 || s.StatusBefore != "open" || s.Log == "" || s.ID != "A-1" {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(errs, "run="+s.RunID) { // the command's stdout went to stderr
		t.Fatalf("child output should go to stderr, with the run id: %q", errs)
	}
	// failure: exit 1 by default, the command's own status with -propagate-exit
	code, out, _ = do(t, "-c", cfg, "run", "bad", "A-2", "-json")
	if code != exitError {
		t.Fatalf("default exit %d", code)
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil || s.Result != "error" || *s.ExitCode != 7 {
		t.Fatalf("%v %+v", err, s)
	}
	if code, _, _ = do(t, "-c", cfg, "run", "bad", "A-2", "-propagate-exit"); code != 7 {
		t.Fatalf("-propagate-exit exit %d", code)
	}
	// several tasks: a list, the first failure decides the code
	code, out, _ = do(t, "-c", cfg, "run", "bad", "A-1", "A-2", "-json", "-propagate-exit")
	var list []runSummary
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 2 || code != 7 {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
}

func TestRunCaptureAndMoves(t *testing.T) {
	dir, _, cfg := setup(t, issuesCSV, issuesYAML+resultActionsYAML)
	if code, _, errs := do(t, "-c", cfg, "run", "ok", "A-1"); code != exitOK {
		t.Fatalf("%d %s", code, errs)
	}
	_, out, _ := do(t, "-c", cfg, "show", "A-1", "-json")
	var got struct{ Notes, Status, Column string }
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Notes, "[run ") || !strings.Contains(got.Notes, "run=") || !strings.Contains(got.Notes, "more") || got.Column != "Done" {
		t.Fatalf("%+v", got)
	}
	// on_failure moves, nothing captured without capture:
	do(t, "-c", cfg, "run", "bad", "A-1")
	_, out, _ = do(t, "-c", cfg, "show", "A-1", "-json")
	json.Unmarshal([]byte(out), &got)
	if got.Column != "Doing" || strings.Contains(got.Notes, "oops") {
		t.Fatalf("%+v", got)
	}
	// capture_max cuts the output
	do(t, "-c", cfg, "run", "big", "A-2")
	_, out, _ = do(t, "-c", cfg, "show", "A-2", "-json")
	json.Unmarshal([]byte(out), &got)
	if !strings.Contains(got.Notes, "[output cut]") || len(got.Notes) > 1400 {
		t.Fatalf("notes %d bytes: %.200q", len(got.Notes), got.Notes)
	}
	// an unknown on_success status warns, the run still succeeds
	if code, _, errs := do(t, "-c", cfg, "run", "ghost", "A-3"); code != exitOK || !strings.Contains(errs, "nowhere") {
		t.Fatalf("%d %q", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "work.csv")); !strings.Contains(string(b), "A-3") {
		t.Fatal("csv lost a row")
	}
}

func TestReport(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+"action_log: {file: actions.jsonl}\naudit: {file: audit.jsonl}\n")
	t.Setenv("TACK_RUN_ID", "r-1")
	code, out, errs := do(t, "-c", cfg, "report", "A-1", "-status", "Doing", "-note", "half done")
	if code != exitOK {
		t.Fatalf("%d %s", code, errs)
	}
	_, out, _ = do(t, "-c", cfg, "show", "A-1", "-json")
	if !strings.Contains(out, "run r-1] half done") || !strings.Contains(out, `"column": "Doing"`) {
		t.Fatalf("%s", out)
	}
	if _, out, _ = do(t, "-c", cfg, "runs", "-q", "run=r-1"); !strings.Contains(out, "r-1") || !strings.Contains(out, "RUN") {
		t.Fatalf("runs -q run=r-1:\n%s", out)
	}
	if _, out, _ = do(t, "-c", cfg, "runs", "-q", "run=other"); strings.Contains(out, "r-1") {
		t.Fatalf("runs -q run=other matched:\n%s", out)
	}
	_, out, _ = do(t, "-c", cfg, "runs", "-json")
	if !strings.Contains(out, `"report_status": "wip"`) || !strings.Contains(out, `"run_id": "r-1"`) {
		t.Fatalf("%s", out)
	}
	if code, _, _ = do(t, "-c", cfg, "report", "A-1"); code != exitUsage {
		t.Fatalf("no -status/-note: %d", code)
	}
	if code, _, _ = do(t, "-c", cfg, "report", "A-1", "-status", "nope"); code != exitUsage {
		t.Fatalf("bad status: %d", code)
	}
	if code, _, _ = do(t, "-c", cfg, "report", "ZZ", "-note", "x"); code != exitNotFound {
		t.Fatalf("unknown task: %d", code)
	}
}

func TestRunKeepsWhatTheCommandReported(t *testing.T) {
	dir, _, cfg := setup(t, issuesCSV, issuesYAML+"actions:\n  - name: self\n    on_success: Done\n    command: [sh, mover.sh]\n")
	// The command moves the card behind tack's back (as an agent's `tack report` does).
	file := filepath.Join(dir, "work.csv")
	script := "sed -i 's/Ship it,open/Ship it,wip/' " + file + "\n"
	if err := os.WriteFile(filepath.Join(dir, "mover.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errs := do(t, "-c", cfg, "run", "self", "A-1", "-json")
	if code != exitOK || strings.Contains(errs, "precondition") {
		t.Fatalf("%d %q", code, errs)
	}
	var s runSummary
	if err := json.Unmarshal([]byte(out), &s); err != nil || s.StatusAfter != "wip" {
		t.Fatalf("status_after should show the command's move: %v %+v", err, s)
	}
}

func TestRunAppendNotesOutcomes(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+`actions:
  - name: sum
    command: [sh, -c, 'echo hidden']
    on_success: {status: Done, append_notes: summary}
  - name: fail
    command: [sh, -c, 'echo boom >&2; exit 4']
    on_fail: {status: Doing, append_notes: error}
`)
	var got struct{ Notes, Column string }
	show := func(id string) {
		_, out, _ := do(t, "-c", cfg, "show", id, "-json")
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
	}
	do(t, "-c", cfg, "run", "sum", "A-1")
	if show("A-1"); got.Column != "Done" || !strings.Contains(got.Notes, "exit 0") || strings.Contains(got.Notes, "hidden") {
		t.Fatalf("%+v", got)
	}
	do(t, "-c", cfg, "run", "fail", "A-2")
	if show("A-2"); got.Column != "Doing" || !strings.Contains(got.Notes, "boom") {
		t.Fatalf("%+v", got)
	}
}

func TestRunOutcomeCommand(t *testing.T) {
	dir := t.TempDir()
	okF, badF := filepath.Join(dir, "ok.txt"), filepath.Join(dir, "bad.txt")
	y := `actions:
  - name: ok
    command: [true]
    on_success:
      status: Done
      command: [sh, -c, 'echo "$1 $2 $TACK_RESULT" > "$3"', sh, '{id}', '{status}', ` + okF + `]
    on_failure:
      command: [sh, -c, 'echo no > "$1"', sh, ` + badF + `]
  - name: bad
    command: [sh, -c, 'exit 3']
    on_success:
      command: [sh, -c, 'echo no > "$1"', sh, ` + badF + `]
    on_failure:
      status: Doing
      command: [sh, -c, 'echo "$TACK_EXIT {exit}" > "$1"', sh, ` + badF + `]
  - name: broken
    command: [true]
    on_success:
      status: Done
      command: [sh, -c, 'exit 9']
`
	_, _, cfg := setup(t, issuesCSV, issuesYAML+y)
	if code, _, e := do(t, "-c", cfg, "run", "ok", "A-1"); code != exitOK {
		t.Fatalf("ok: %d %s", code, e)
	}
	b, err := os.ReadFile(okF)
	if err != nil || strings.TrimSpace(string(b)) != "A-1 closed ok" {
		t.Fatalf("success command: %v %q", err, b)
	}
	if _, err := os.Stat(badF); err == nil {
		t.Fatal("on_failure command ran after a success")
	}
	if code, _, _ := do(t, "-c", cfg, "run", "bad", "A-2"); code != exitError {
		t.Fatalf("bad: %d", code)
	}
	if b, err := os.ReadFile(badF); err != nil || strings.TrimSpace(string(b)) != "3 3" {
		t.Fatalf("failure command: %v %q", err, b)
	}
	// a failing outcome command is reported but the run and the move stand
	code, _, e := do(t, "-c", cfg, "run", "broken", "A-3")
	if code != exitOK || !strings.Contains(e, "on_success command") {
		t.Fatalf("broken: %d %q", code, e)
	}
	if _, out, _ := do(t, "-c", cfg, "show", "A-3", "-json"); !strings.Contains(out, "Done") && !strings.Contains(strings.ToLower(out), "done") {
		t.Fatalf("not moved: %s", out)
	}
}

func TestRunInteractiveFailingBeforeStartIsRecorded(t *testing.T) {
	dir, _, cfg := setup(t, issuesCSV, issuesYAML+`action_log: {file: logs/actions.jsonl}
actions:
  - name: piped
    command: [cat]
    stdin: json
    on_failure: Doing
`)
	code, _, errs := do(t, "-c", cfg, "run", "piped", "A-1", "-i")
	if code != exitError || !strings.Contains(errs, "stdin") {
		t.Fatalf("%d %q", code, errs)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "logs", "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Time, Ended, Result, Error, Action string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &rec); err != nil {
		t.Fatalf("one record expected: %v\n%s", err, raw)
	}
	if rec.Result != "error" || rec.Action != "piped" || rec.Time != rec.Ended || !strings.Contains(rec.Error, "stdin") {
		t.Fatalf("%+v", rec)
	}
	var got struct{ Column string }
	_, out, _ := do(t, "-c", cfg, "show", "A-1", "-json")
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Column != "Doing" {
		t.Fatalf("on_failure should apply: %v %+v", err, got)
	}
}

func TestRunWorkdirOverride(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+resultActionsYAML)
	code, out, _ := do(t, "-c", cfg, "run", "-n", "ok", "A-1", "-workdir", "/srv/elsewhere")
	if code != exitOK || !strings.Contains(out, "cd /srv/elsewhere && ") {
		t.Errorf("-workdir: %d %q", code, out)
	}
	dir := t.TempDir()
	if code, _, e := do(t, "-c", cfg, "run", "ok", "A-1", "-workdir", filepath.Join(dir, "nope")); code != exitError || !strings.Contains(e, "working directory") {
		t.Errorf("missing workdir: %d %q", code, e)
	}
	if code, _, e := do(t, "-c", cfg, "run", "ok", "A-1", "-workdir", dir); code != exitOK {
		t.Errorf("existing workdir: %d %q", code, e)
	}
}

func TestRunWorkdirEnv(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+resultActionsYAML)
	t.Setenv("TACK_T_WD", "/srv/env")
	if code, out, _ := do(t, "-c", cfg, "run", "-n", "ok", "A-1", "-workdir", "$TACK_T_WD/x"); code != exitOK || !strings.Contains(out, "cd /srv/env/x && ") {
		t.Errorf("env in -workdir: %d %q", code, out)
	}
	if code, _, e := do(t, "-c", cfg, "run", "-n", "ok", "A-1", "-workdir", "$TACK_T_WD_UNSET/x"); code != exitUsage || !strings.Contains(e, "is not set") {
		t.Errorf("unset: %d %q", code, e)
	}
}

// Agents running `tack report` / `tack edit -append-notes` on one task at the
// same moment must all leave their note behind.
func TestConcurrentNoteAppendsKeepEveryNote(t *testing.T) {
	_, file, cfg := setup(t, "id,title,status,notes\n1,A,pending,start\n", "")
	const n = 6
	var wg sync.WaitGroup
	codes := make([]int, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[i], _, _ = do(t, "-c", cfg, "report", "1", "-note", fmt.Sprintf("report-%d", i))
		}()
		go func() {
			defer wg.Done()
			codes[n+i], _, _ = do(t, "-c", cfg, "edit", "1", "-append-notes", fmt.Sprintf("edit-%d", i))
		}()
	}
	wg.Wait()
	data, _ := os.ReadFile(file)
	for i, c := range codes {
		if c != exitOK {
			t.Errorf("writer %d exited %d", i, c)
		}
	}
	for i := 0; i < n; i++ {
		for _, want := range []string{fmt.Sprintf("report-%d", i), fmt.Sprintf("edit-%d", i)} {
			if !strings.Contains(string(data), want) {
				t.Errorf("note %q lost:\n%s", want, data)
			}
		}
	}
	if !strings.Contains(string(data), "start") {
		t.Errorf("the first note was lost:\n%s", data)
	}
}
