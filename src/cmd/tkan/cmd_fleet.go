package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"tkan/internal/backend"
	"tkan/internal/fleet"
)

// fleetOut is one row of `tkan fleet -json`: the contract for scripts and
// plugins that draw the fleet their own way.
type fleetOut struct {
	Agent     string `json:"agent"`
	State     string `json:"state"`
	Backend   string `json:"backend,omitempty"`
	ID        string `json:"id,omitempty"`
	Title     string `json:"title,omitempty"`
	ClaimedAt string `json:"claimed_at,omitempty"`
	Lease     string `json:"lease,omitempty"`
	Elapsed   int    `json:"elapsed_sec,omitempty"` // seconds since the claim started
	Left      int    `json:"left_sec,omitempty"`    // seconds until the lease runs out
}

// cmdFleet: `tkan fleet [-json]`: who holds which task, since when, until when.
func cmdFleet(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
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
		fmt.Fprintln(errw, "tkan: warning:", err)
	}
	at := now()
	rows := fleet.Build(tasks, a.spec.Agents, at)
	res := make([]fleetOut, len(rows))
	for i, r := range rows {
		o := fleetOut{Agent: r.Agent, State: string(r.State)}
		if t := r.Task; t != nil {
			o.Backend, o.ID, o.Title, o.ClaimedAt, o.Lease = t.Backend, t.ID, t.Title, t.ClaimedAt, t.Lease
			o.Elapsed, o.Left = int(r.Elapsed(at).Seconds()), int(r.Left(at).Seconds())
		}
		res[i] = o
	}
	if *asJSON {
		if res == nil {
			res = []fleetOut{}
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
				since, elapsed = r.Since.Local().Format("01-02 15:04"), fleet.Duration(r.Elapsed(at))
			}
			if !r.Until.IsZero() {
				left = fleet.Duration(r.Left(at))
				if r.State == fleet.Expired {
					left = "expired"
				}
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Agent, r.State, strings.TrimSpace(task), since, elapsed, left)
	}
	w.Flush()
	return exitOK
}
