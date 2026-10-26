package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// at sets the clock of claims and leases for the rest of the test.
func at(t *testing.T, ts string) {
	t.Helper()
	c, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	old := now
	now = func() time.Time { return c }
	t.Cleanup(func() { now = old })
}

const claimCSV = "id,title,status,priority,assignee,lease,blocked_by\n" +
	"1,Low,pending,low,,,\n" +
	"2,High,pending,high,,,\n" +
	"3,Done,done,critical,,,\n" +
	"4,Blocked,pending,critical,,,1\n"

func TestClaimReleaseAndLease(t *testing.T) {
	_, file, cfg := setup(t, claimCSV, "")
	at(t, "2026-10-18T12:00:00Z")
	code, out, e := do(t, "-c", cfg, "claim", "2", "-as", "ann", "-lease", "30m", "-status", "in_progress")
	if code != exitOK || !strings.Contains(out, "claimed by ann until 2026-10-18T12:30:00Z") {
		t.Fatalf("claim: %d %q %q", code, out, e)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "2,High,in_progress,high,ann,2026-10-18T12:30:00Z,") {
		t.Fatalf("file:\n%s", b)
	}
	// held by another: exit 4, nothing written
	before, _ := os.ReadFile(file)
	if code, _, e := do(t, "-c", cfg, "claim", "2", "-as", "bob"); code != exitPrecondition || !strings.Contains(e, "claimed by ann until") {
		t.Fatalf("second claim: %d %q", code, e)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("a refused claim wrote:\n%s", after)
	}
	// the holder renews; another may not release it
	if code, _, e := do(t, "-c", cfg, "claim", "2", "-as", "ann", "-lease", "1h"); code != exitOK {
		t.Fatalf("renew: %d %q", code, e)
	}
	if code, _, _ := do(t, "-c", cfg, "release", "2", "-as", "bob"); code != exitPrecondition {
		t.Fatalf("release by another: %d", code)
	}
	// after the lease runs out somebody else takes it over
	at(t, "2026-10-18T13:00:01Z")
	if code, _, e := do(t, "-c", cfg, "claim", "2", "-as", "bob"); code != exitOK {
		t.Fatalf("takeover: %d %q", code, e)
	}
	code, out, e = do(t, "-c", cfg, "release", "2", "-as", "bob", "-status", "pending", "-json")
	var res []struct {
		Assignee, Lease, Status string
		Changed                 bool
	}
	if code != exitOK || json.Unmarshal([]byte(out), &res) != nil || len(res) != 1 || !res[0].Changed || res[0].Assignee != "" || res[0].Lease != "" || res[0].Status != "pending" {
		t.Fatalf("release: %d %q %q", code, out, e)
	}
	if code, out, _ := do(t, "-c", cfg, "release", "2", "-as", "bob"); code != exitOK || !strings.Contains(out, "not claimed") {
		t.Fatalf("release of a free task: %d %q", code, out)
	}
	// usage errors
	t.Setenv("TACK_AGENT", "")
	for _, args := range [][]string{{"claim", "1"}, {"claim", "1", "-as", "x", "-lease", "-1m"}, {"claim", "1", "-as", "x", "-status", "nowhere"}, {"release", "1"}} {
		if code, _, _ := do(t, append([]string{"-c", cfg}, args...)...); code != exitUsage {
			t.Errorf("%v: code %d, want %d", args, code, exitUsage)
		}
	}
	if code, _, _ := do(t, "-c", cfg, "claim", "99", "-as", "x"); code != exitNotFound {
		t.Errorf("unknown id: code %d", code)
	}
	t.Setenv("TACK_AGENT", "env-agent")
	if code, out, _ := do(t, "-c", cfg, "claim", "1"); code != exitOK || !strings.Contains(out, "claimed by env-agent") {
		t.Errorf("TACK_AGENT: %d %q", code, out)
	}
}

func TestClaimLeaseNeedsALeaseColumn(t *testing.T) {
	_, file, cfg := setup(t, "id,title,status,assignee\n1,A,pending,\n", "")
	before, _ := os.ReadFile(file)
	if code, _, e := do(t, "-c", cfg, "claim", "1", "-as", "ann", "-lease", "5m"); code != exitUsage || !strings.Contains(e, "no lease field") {
		t.Fatalf("lease without column: %d %q", code, e)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("wrote:\n%s", after)
	}
	if code, _, e := do(t, "-c", cfg, "claim", "1", "-as", "ann"); code != exitOK {
		t.Fatalf("claim without lease: %d %q", code, e)
	}
	_, _, cfg = setup(t, "id,title,status\n1,A,pending\n", "")
	if code, _, e := do(t, "-c", cfg, "claim", "1", "-as", "ann"); code != exitUsage || !strings.Contains(e, "no assignee field") {
		t.Fatalf("no assignee column: %d %q", code, e)
	}
}

func TestNextPicksAndClaims(t *testing.T) {
	_, _, cfg := setup(t, claimCSV, "")
	at(t, "2026-10-18T12:00:00Z")
	// done and blocked tasks are skipped; the most urgent ready one wins
	if code, out, e := do(t, "-c", cfg, "next"); code != exitOK || out != "csv1:2 High\n" {
		t.Fatalf("next: %d %q %q", code, out, e)
	}
	if code, out, _ := do(t, "-c", cfg, "next", "-q", "priority=low", "-json"); code != exitOK || !strings.Contains(out, `"id": "1"`) {
		t.Fatalf("next -q: %d %q", code, out)
	}
	if code, out, e := do(t, "-c", cfg, "next", "-claim", "-as", "ann", "-lease", "10m", "-to", "in_progress"); code != exitOK || !strings.Contains(out, "csv1:2 claimed by ann until 2026-10-18T12:10:00Z High") {
		t.Fatalf("next -claim: %d %q %q", code, out, e)
	}
	// claimed tasks are not offered again
	if code, out, e := do(t, "-c", cfg, "next", "-claim", "-as", "bob"); code != exitOK || !strings.Contains(out, "csv1:1 ") {
		t.Fatalf("second next -claim: %d %q %q", code, out, e)
	}
	if code, _, e := do(t, "-c", cfg, "next", "-claim", "-as", "cy"); code != exitNotFound || !strings.Contains(e, "no task") {
		t.Fatalf("nothing left: %d %q", code, e)
	}
	// an expired lease makes the task available again
	at(t, "2026-10-18T12:10:00Z")
	if code, out, _ := do(t, "-c", cfg, "next"); code != exitOK || out != "csv1:2 High\n" {
		t.Fatalf("expired lease: %d %q", code, out)
	}
	if code, _, _ := do(t, "-c", cfg, "next", "-lease", "1m"); code != exitUsage {
		t.Fatalf("-lease without -claim: %d", code)
	}
}

// Parallel agents taking work with `next -claim` never get the same task.
func TestNextClaimRace(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("id,title,status,assignee,lease\n")
	const n = 6
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&rows, "%d,T%d,pending,,\n", i, i)
	}
	_, file, cfg := setup(t, rows.String(), "")
	type got struct {
		code int
		out  string
	}
	res := make([]got, n+2)
	var wg sync.WaitGroup
	for i := range res {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, out, _ := do(t, "-c", cfg, "next", "-claim", "-as", fmt.Sprintf("agent%d", i), "-json")
			res[i] = got{code, out}
		}(i)
	}
	wg.Wait()
	ids := map[string]int{}
	misses := 0
	for i, r := range res {
		switch r.code {
		case exitOK:
			var o struct{ ID, Assignee string }
			if err := json.Unmarshal([]byte(r.out), &o); err != nil || o.Assignee != fmt.Sprintf("agent%d", i) {
				t.Fatalf("agent%d: %q %v", i, r.out, err)
			}
			if prev, dup := ids[o.ID]; dup {
				t.Fatalf("agents %d and %d both got task %s", prev, i, o.ID)
			}
			ids[o.ID] = i
		case exitNotFound:
			misses++
		default:
			t.Fatalf("agent%d: code %d %q", i, r.code, r.out)
		}
	}
	if len(ids) != n || misses != 2 {
		t.Fatalf("%d tasks claimed, %d agents without one; want %d and 2\n%s", len(ids), misses, n, mustRead(t, file))
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunClaimsAndReleases(t *testing.T) {
	dir, file, cfg := setup(t, "id,title,status,assignee,lease\n1,A,pending,,\n2,B,pending,zed,\n", `actions:
  - name: look
    claim: true
    command: [sh, -c, 'cat work.csv > seen.csv']
  - name: boom
    claim: true
    command: [sh, -c, 'exit 3']
`)
	at(t, "2026-10-18T12:00:00Z")
	if code, _, e := do(t, "-c", cfg, "run", "look", "1", "-as", "ann"); code != exitOK {
		t.Fatalf("run: %d %q", code, e)
	}
	// during the run the task was claimed, with a lease of timeout + grace
	if seen := mustRead(t, filepath.Join(dir, "seen.csv")); !strings.Contains(seen, "1,A,pending,ann,2026-10-18T12:00:15Z") {
		t.Fatalf("during the run:\n%s", seen)
	}
	if after := mustRead(t, file); !strings.Contains(after, "1,A,pending,,,\n") {
		t.Fatalf("not released after the run:\n%s", after)
	}
	// a failing run releases too
	if code, _, _ := do(t, "-c", cfg, "run", "boom", "1", "-as", "ann"); code != exitError {
		t.Fatalf("failing run: %d", code)
	}
	if after := mustRead(t, file); !strings.Contains(after, "1,A,pending,,,\n") {
		t.Fatalf("not released after a failure:\n%s", after)
	}
	// a task claimed by another agent starts nothing
	if code, _, e := do(t, "-c", cfg, "run", "look", "1", "2", "-as", "ann"); code != exitPrecondition || !strings.Contains(e, "claimed by zed") || !strings.Contains(e, "nothing started") {
		t.Fatalf("claimed task: %d %q", code, e)
	}
}

func TestShowPrintsClaimState(t *testing.T) {
	_, _, cfg := setup(t, claimCSV, "")
	at(t, "2026-10-18T12:00:00Z")
	if code, out, e := do(t, "-c", cfg, "show", "1"); code != exitOK || strings.Contains(out, "lease:") {
		t.Fatalf("unclaimed: %d %q %q", code, out, e)
	}
	if code, _, e := do(t, "-c", cfg, "claim", "2", "-as", "ann", "-lease", "30m"); code != exitOK {
		t.Fatal(code, e)
	}
	code, out, e := do(t, "-c", cfg, "show", "2")
	if code != exitOK || !strings.Contains(out, "assignee:    ann") || !strings.Contains(out, "lease:       2026-10-18T12:30:00Z") {
		t.Fatalf("claimed: %d %q %q", code, out, e)
	}
}

func TestConfigListsActionsHooksAndLimits(t *testing.T) {
	_, _, cfg := setup(t, issuesCSV, issuesYAML+actionsYAML+"hook_max_depth: 2\nhooks:\n  - name: ping\n    on: [task.moved]\n    command: [true]\n")
	code, out, e := do(t, "-c", cfg, "config")
	if code != exitOK {
		t.Fatalf("%d %q", code, e)
	}
	for _, want := range []string{"actions:", "name: note", "mode: interactive", "hooks:", "name: ping", "task.moved", "hook_max_depth: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("config output lacks %q:\n%s", want, out)
		}
	}
}

func TestNextClaimSkipsABackendThatCannotKeepClaims(t *testing.T) {
	dir, _, cfg := setup(t, "id,title,status,priority\n1,Urgent but unclaimable,pending,critical\n", `backend:
  - {type: csv, name: plain, options: {source: work.csv}}
  - {type: csv, name: claims, options: {source: claims.csv}}
`)
	if err := os.WriteFile(filepath.Join(dir, "claims.csv"), []byte("id,title,status,priority,assignee,lease\n7,Claimable,pending,low,,\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	at(t, "2026-10-18T12:00:00Z")
	code, out, e := do(t, "-c", cfg, "next", "-claim", "-as", "ann")
	if code != exitOK || !strings.Contains(out, "claims:7") {
		t.Fatalf("next -claim: %d %q %q", code, out, e)
	}
	// Nothing claimable anywhere: still the setup error, not "empty queue".
	code, _, e = do(t, "-c", cfg, "next", "-claim", "-as", "ann")
	if code != exitUsage || !strings.Contains(e, "backend plain has no assignee field") {
		t.Fatalf("second: %d %q", code, e)
	}
}

func TestRepeatedIDsAreProcessedOnce(t *testing.T) {
	_, file, cfg := setup(t, claimCSV, "")
	at(t, "2026-10-18T12:00:00Z")
	if code, out, e := do(t, "-c", cfg, "claim", "2", "2", "-as", "ann", "-lease", "30m"); code != exitOK || strings.Count(out, "claimed by ann") != 1 {
		t.Fatalf("claim 2 2: %d %q %q", code, out, e)
	}
	if code, _, e := do(t, "-c", cfg, "set-status", "2", "csv1:2", "in_progress"); code != exitOK {
		t.Fatalf("set-status: %d %q", code, e)
	}
	if code, out, e := do(t, "-c", cfg, "release", "2", "2", "-as", "ann"); code != exitOK || strings.Count(out, "released") > 1 {
		t.Fatalf("release: %d %q %q", code, out, e)
	}
	if code, out, e := do(t, "-c", cfg, "delete", "1", "1"); code != exitOK || strings.Contains(e, "stopped") {
		t.Fatalf("delete 1 1: %d %q %q", code, out, e)
	}
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "\n1,Low") {
		t.Fatalf("task 1 not deleted:\n%s", b)
	}
}

func TestBlockedByCycleIsNamed(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status,blocked_by\n1,A,pending,2\n2,B,pending,1\n3,C,pending,\n", "")
	code, out, e := do(t, "-c", cfg, "list", "-ids")
	if code != exitOK || !strings.Contains(e, "blocked_by cycle") || !strings.Contains(e, "csv1:1 -> csv1:2 -> csv1:1") || !strings.Contains(out, "3") {
		t.Fatalf("list: %d out %q err %q", code, out, e)
	}
	// run names the ring when it refuses a blocked task
	_, _, cfg2 := setup(t, "id,title,status,blocked_by\n1,A,pending,2\n2,B,pending,1\n", "actions:\n  - name: x\n    command: [true]\n")
	code, _, e = do(t, "-c", cfg2, "run", "x", "1")
	if code != exitPrecondition || !strings.Contains(e, "circular: csv1:1 -> csv1:2 -> csv1:1") {
		t.Fatalf("run: %d %q", code, e)
	}
}
