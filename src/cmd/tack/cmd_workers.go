package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"tack/internal/backend"
	"tack/internal/workers"
)

// workersOut is one row of `tack workers -json`: the contract for scripts and
// plugins that draw the workers their own way.
type workersOut struct {
	Agent     string `json:"agent"`
	State     string `json:"state"`
	Backend   string `json:"backend,omitempty"`
	ID        string `json:"id,omitempty"`
	Title     string `json:"title,omitempty"`
	ClaimedAt string `json:"claimed_at,omitempty"`
	Lease     string `json:"lease,omitempty"`
	Elapsed   int    `json:"elapsed_sec,omitempty"` // seconds since the claim started
	Left      int    `json:"left_sec,omitempty"`    // seconds until the lease runs out
	// Overlaid names the task fields kept in a local overlay (see docs/config.md).
	Overlaid []string `json:"overlaid,omitempty"`
}

// cmdWorkers: `tack workers [-json]`: who holds which task, since when, until when.
func cmdWorkers(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("workers", flag.ContinueOnError)
	fs.SetOutput(errw)
	asJSON := fs.Bool("json", false, "JSON output")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	tasks, err := a.be.List(ctx)
	if err != nil {
		var perr *backend.PartialError
		if !errors.As(err, &perr) {
			return fail(errw, err)
		}
		fmt.Fprintln(errw, "tack: warning:", err)
	}
	at := now()
	rows := workers.Build(tasks, a.spec.Agents, at)
	res := make([]workersOut, len(rows))
	for i, r := range rows {
		o := workersOut{Agent: r.Agent, State: string(r.State)}
		if t := r.Task; t != nil {
			o.Backend, o.ID, o.Title, o.ClaimedAt, o.Lease, o.Overlaid = t.Backend, t.ID, t.Title, t.ClaimedAt, t.Lease, t.Overlaid
			o.Elapsed, o.Left = int(r.Elapsed(at).Seconds()), int(r.Left(at).Seconds())
		}
		res[i] = o
	}
	if *asJSON {
		if res == nil {
			res = []workersOut{}
		}
		return writeJSON(out, res)
	}
	if len(res) == 0 {
		fmt.Fprintln(out, "no agent holds a task (list agents in board.agents to see them idle)")
		return exitOK
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tSTATE\tTASK\tSINCE\tELAPSED\tLEFT")
	for i, r := range rows {
		o := res[i]
		task, since, elapsed, left := "-", "-", "-", "-"
		if r.Task != nil {
			task = o.Backend + ":" + o.ID + " " + o.Title
			if !r.Since.IsZero() {
				since, elapsed = r.Since.Local().Format("01-02 15:04"), workers.Duration(r.Elapsed(at))
			}
			if !r.Until.IsZero() {
				left = workers.Duration(r.Left(at))
				if r.State == workers.Expired {
					left = "expired"
				}
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", agentName(r), r.State, strings.TrimSpace(task), since, elapsed, left)
	}
	w.Flush()
	return exitOK
}

// agentName is the agent's name, marked when the claim lives in a local overlay.
func agentName(r workers.Row) string {
	if r.Task != nil {
		return r.Task.Marked("assignee", r.Agent)
	}
	return r.Agent
}
