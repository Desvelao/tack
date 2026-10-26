package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/backend"
	"tack/internal/board"
)

func claimModel(t *testing.T, csv string) (Model, *backend.Set) {
	t.Helper()
	t.Setenv("TACK_AGENT", "tester")
	p := filepath.Join(t.TempDir(), "t.csv")
	if err := os.WriteFile(p, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	be := openSet(t, p)
	m := New(be, "t.csv", board.DefaultSpec())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return nm.(Model), be
}

func assigneeOf(t *testing.T, be *backend.Set, id string) (string, string) {
	t.Helper()
	ts, _ := be.List(context.Background())
	for _, x := range ts {
		if x.ID == id {
			return x.Assignee, x.Lease
		}
	}
	t.Fatalf("task %s gone", id)
	return "", ""
}

func TestClaimAndReleaseKeys(t *testing.T) {
	m, be := claimModel(t, "id,title,status,assignee,lease\n1,Alpha,pending,,\n")
	m = press(m, "c")
	if who, _ := assigneeOf(t, be, "1"); who != "tester" || !strings.Contains(m.notice, "claimed #1 as tester") {
		t.Fatalf("claim: %q %q", who, m.notice)
	}
	if v := m.View(); !strings.Contains(v, "tester") {
		t.Errorf("the card does not show the claim:\n%s", v)
	}
	m = press(m, "C")
	if who, _ := assigneeOf(t, be, "1"); who != "" || !strings.Contains(m.notice, "released #1") {
		t.Fatalf("release: %q %q", who, m.notice)
	}
	if m = press(m, "C"); !strings.Contains(m.notice, "not claimed") {
		t.Errorf("releasing a free card: %q", m.notice)
	}
}

func TestClaimKeyRefusesAnotherAgentsClaim(t *testing.T) {
	m, be := claimModel(t, "id,title,status,assignee,lease\n1,Alpha,pending,bob,\n")
	m = press(m, "c")
	if who, _ := assigneeOf(t, be, "1"); who != "bob" || !strings.Contains(m.notice, "bob") {
		t.Fatalf("claim over bob's: %q %q", who, m.notice)
	}
}

func TestReleasingSomebodyElsesClaimAsksFirst(t *testing.T) {
	m, be := claimModel(t, "id,title,status,assignee,lease\n1,Alpha,pending,bob,\n")
	m = press(m, "C")
	if m.mode != modeConfirmRelease || !strings.Contains(m.View(), "take csv1:1 from bob? (y/N)") {
		t.Fatalf("no question: mode %v\n%s", m.mode, m.View())
	}
	m = press(m, "n")
	if who, _ := assigneeOf(t, be, "1"); who != "bob" || m.mode != modeBoard {
		t.Fatalf("declined, yet %q mode %v", who, m.mode)
	}
	m = press(m, "C", "y")
	if who, _ := assigneeOf(t, be, "1"); who != "" || !strings.Contains(m.notice, "released #1 (was bob)") {
		t.Fatalf("forced release: %q %q", who, m.notice)
	}
}

func TestReleaseKeyClearsAnExpiredClaimAtOnce(t *testing.T) {
	m, be := claimModel(t, "id,title,status,assignee,lease\n1,Alpha,pending,crashed,2020-01-01T00:00:00Z\n")
	m = press(m, "C")
	if who, lease := assigneeOf(t, be, "1"); who != "" || lease != "" || m.mode != modeBoard {
		t.Fatalf("expired claim kept: %q %q mode %v", who, lease, m.mode)
	}
}

func TestWorkersViewReleasesTheSelectedRow(t *testing.T) {
	m, be := claimModel(t, "id,title,status,assignee,lease\n1,Alpha,pending,crashed,2020-01-01T00:00:00Z\n")
	m = press(m, "W", "C")
	if who, _ := assigneeOf(t, be, "1"); who != "" {
		t.Fatalf("workers C kept the claim: %q (%s)", who, m.notice)
	}
}
