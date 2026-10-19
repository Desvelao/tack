package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const resultActionsYAML = `actions:
  - name: ok
    command: [sh, -c, 'echo "run=$TKAN_RUN_ID log=$TKAN_ACTION_LOG"; echo more']
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
	t.Setenv("TKAN_RUN_ID", "r-1")
	code, out, errs := do(t, "-c", cfg, "report", "A-1", "-status", "Doing", "-note", "half done")
	if code != exitOK {
		t.Fatalf("%d %s", code, errs)
	}
	_, out, _ = do(t, "-c", cfg, "show", "A-1", "-json")
	if !strings.Contains(out, "run r-1] half done") || !strings.Contains(out, `"column": "Doing"`) {
		t.Fatalf("%s", out)
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
	// The command moves the card behind tkan's back (as an agent's `tkan report` does).
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
