package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/claim"
	"tack/internal/task"
)

// userAgent is who the board claims for: $TACK_AGENT, else the login name.
func userAgent() string {
	for _, k := range []string{"TACK_AGENT", "USER", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "me"
}

// claimSelected (`c`) claims the selected card for the person at the board; one
// held by somebody else is refused with the claim's own message.
func (m *Model) claimSelected() tea.Cmd {
	t, ok := m.selected()
	if !ok {
		return nil
	}
	r := claim.Request{Agent: userAgent()}
	if err := claim.Check(m.be, t, r); err != nil {
		m.fail("error: " + err.Error())
		return nil
	}
	if _, _, err := claim.Claim(t, r, m.clock()); err != nil { // held by somebody else
		m.fail(err.Error())
		return nil
	}
	be, now := m.be, m.clock()
	var c task.Task
	return m.write(func() (err error) { c, err = claim.Take(context.Background(), be, t, r, now); return }, func(m *Model, err error) {
		if err != nil {
			m.fail("error: " + err.Error())
			return
		}
		if m.async != nil {
			m.showLocal(c, false)
		} else if m.reload() != nil {
			return
		}
		m.ok(fmt.Sprintf("claimed #%s as %s", c.ID, r.Agent))
	})
}

// releaseSelected (`C`) gives back the claim of the selected card.
func (m *Model) releaseSelected() tea.Cmd {
	t, ok := m.selected()
	if !ok {
		return nil
	}
	return m.releaseCard(t)
}

// releaseCard gives back the claim on t. A claim of the person at the board, or
// one that ran out (a crashed agent's), goes at once; one somebody else still
// holds asks first, as it takes their task away.
func (m *Model) releaseCard(t task.Task) tea.Cmd {
	if strings.TrimSpace(t.Assignee) == "" && strings.TrimSpace(t.Lease) == "" {
		m.notice = "#" + t.ID + " is not claimed"
		return nil
	}
	if holder, _, held := claim.Holder(t, m.clock()); held && !strings.EqualFold(holder, userAgent()) {
		m.relTarget, m.mode = t, modeConfirmRelease
		return nil
	}
	return m.giveCard(t, false)
}

// giveCard writes the release. Without force it is the holder's own (the
// assignee, whoever that is, when the lease ran out); with force it is taken
// from whoever holds it. The write is guarded on the assignee, so a claim
// changed since the card was drawn is not released by mistake.
func (m *Model) giveCard(t task.Task, force bool) tea.Cmd {
	be := m.be
	var c task.Task
	var changed bool
	return m.write(func() (err error) {
		c, changed, err = claim.Give(context.Background(), be, t, t.Assignee, force, "")
		return
	}, func(m *Model, err error) {
		if err != nil {
			m.fail("error: " + err.Error())
			return
		}
		if m.async != nil {
			m.showLocal(c, false)
		} else if m.reload() != nil {
			return
		}
		if !changed {
			m.notice = "#" + t.ID + " is not claimed"
			return
		}
		m.ok(fmt.Sprintf("released #%s (was %s)", t.ID, t.Assignee))
	})
}

// handleConfirmRelease answers the question "take #N from X?".
func (m Model) handleConfirmRelease(key string) (tea.Model, tea.Cmd) {
	t := m.relTarget
	m.mode = modeBoard
	if key != "y" && key != "Y" {
		return m, nil
	}
	if !m.idle() {
		m.notice = m.busyNotice()
		return m, nil
	}
	cmd := m.giveCard(t, true)
	return m, cmd
}
