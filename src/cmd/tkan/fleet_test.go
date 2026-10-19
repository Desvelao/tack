package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const fleetCSV = "id,title,status,assignee,lease,claimed_at\n" +
	"1,Free,pending,,,\n" +
	"2,Running,pending,claude,2026-10-19T12:10:00Z,2026-10-19T11:55:00Z\n" +
	"3,Stale,pending,codex,2026-10-19T11:00:00Z,2026-10-19T10:30:00Z\n" +
	"4,Mine,pending,ann,,\n"

func TestFleet(t *testing.T) {
	_, _, cfg := setup(t, fleetCSV, "board:\n  agents: [claude, codex, aider]\n")
	at(t, "2026-10-19T12:00:00Z")
	code, out, e := do(t, "-c", cfg, "fleet", "-json")
	if code != exitOK {
		t.Fatalf("fleet: %d %q", code, e)
	}
	var rows []fleetOut
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err, out)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Agent+":"+r.State+":"+r.ID)
	}
	if want := "claude:busy:2 codex:expired:3 ann:held:4 aider:idle:"; strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
	if rows[0].Elapsed != 300 || rows[0].Left != 600 {
		t.Fatalf("claude: elapsed %d left %d", rows[0].Elapsed, rows[0].Left)
	}
	code, out, _ = do(t, "-c", cfg, "fleet")
	if code != exitOK || !strings.Contains(out, "claude") || !strings.Contains(out, "expired") || !strings.Contains(out, "5m") {
		t.Fatalf("text: %d\n%s", code, out)
	}
	if code, _, _ = do(t, "-c", cfg, "fleet", "extra"); code != exitUsage {
		t.Fatalf("argument: %d", code)
	}
}

func TestFleetEmptyJSONIsAnArray(t *testing.T) {
	_, _, cfg := setup(t, "id,title,status\n1,A,pending\n", "")
	if code, out, _ := do(t, "-c", cfg, "fleet", "-json"); code != exitOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("%d %q", code, out)
	}
}
