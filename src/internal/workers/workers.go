// Package workers turns the claims on the tasks (assignee, lease, claimed_at)
// into one row per agent and task: who is busy on what, since when, until
// when. It is pure: no backend, no UI. The board and `tack workers` draw it.
package workers

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"tack/internal/claim"
	"tack/internal/task"
)

// State is what an agent is doing.
type State string

const (
	Busy    State = "busy"    // holds a task: a lease still running, or a listed agent without one
	Expired State = "expired" // holds a task whose lease ran out: stale, anybody may take it over
	Idle    State = "idle"    // a listed agent that holds nothing
)

// Row is one agent on one task; an idle agent has no task.
type Row struct {
	Agent string
	State State
	Task  *task.Task
	Since time.Time // when the claim started; zero when unknown
	Until time.Time // when the lease runs out; zero when it does not expire
}

// Elapsed is how long the claim has held at now (0 when the start is unknown).
func (r Row) Elapsed(now time.Time) time.Duration {
	if r.Since.IsZero() || now.Before(r.Since) {
		return 0
	}
	return now.Sub(r.Since)
}

// Left is how long until the lease runs out at now (0 when it does not
// expire or has run out).
func (r Row) Left(now time.Time) time.Duration {
	if r.Until.IsZero() || !now.Before(r.Until) {
		return 0
	}
	return r.Until.Sub(now)
}

// Build lists the claims in tasks at now, plus the agents (board.agents) that
// hold nothing. Rows come busiest first: busy, expired, idle, then by
// agent and task.
func Build(tasks []task.Task, agents []string, now time.Time) []Row {
	listed := map[string]string{} // lower-case name -> name as configured
	for _, a := range agents {
		if a = strings.TrimSpace(a); a != "" {
			listed[strings.ToLower(a)] = a
		}
	}
	var rows []Row
	seen := map[string]bool{}
	for i := range tasks {
		t := &tasks[i]
		name, until, held := claim.Holder(*t, now)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		cfg, isAgent := listed[key]
		if !isAgent && held && until.IsZero() {
			continue // a plain assignee (a person): not a listed agent, no lease
		}
		seen[key] = true
		if isAgent {
			name = cfg
		}
		r := Row{Agent: name, Task: t, Until: until}
		if at, err := time.Parse(time.RFC3339, strings.TrimSpace(t.ClaimedAt)); err == nil {
			r.Since = at
		}
		if held {
			r.State = Busy
		} else {
			r.State = Expired
		}
		rows = append(rows, r)
	}
	for key, name := range listed {
		if !seen[key] {
			rows = append(rows, Row{Agent: name, State: Idle})
		}
	}
	rank := map[State]int{Busy: 0, Expired: 1, Idle: 2}
	slices.SortFunc(rows, func(a, b Row) int {
		if c := rank[a.State] - rank[b.State]; c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(a.Agent), strings.ToLower(b.Agent)); c != 0 {
			return c
		}
		return strings.Compare(taskKey(a), taskKey(b))
	})
	return rows
}

func taskKey(r Row) string {
	if r.Task == nil {
		return ""
	}
	return r.Task.Backend + ":" + r.Task.ID
}

// Default avatars by state. A busy agent cycles through its frames.
var (
	busyFrames = []string{"🤖", "🦾"}
	idleGlyph  = "💤"
	staleGlyph = "⌛"
)

// Avatar is the picture of the row at frame n: the agent's own frames
// (board.avatars, matched case-insensitively) cycle while it is busy and the
// first one stands still otherwise; without them the defaults apply.
func Avatar(r Row, avatars map[string][]string, n int) string {
	var own []string
	for name, frames := range avatars {
		if strings.EqualFold(name, r.Agent) && len(frames) > 0 {
			own = frames
			break
		}
	}
	switch r.State {
	case Busy:
		if own == nil {
			own = busyFrames
		}
		if n < 0 {
			n = 0
		}
		return own[n%len(own)]
	case Expired:
		if own != nil {
			return own[0]
		}
		return staleGlyph
	}
	if own != nil {
		return own[0]
	}
	return idleGlyph
}

// Duration formats d compactly: 45s, 12m, 1h05m, 2d03h.
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h" + pad2(int(d%time.Hour/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d" + pad2(int(d%(24*time.Hour)/time.Hour)) + "h"
}

func pad2(n int) string { return fmt.Sprintf("%02d", n) }
