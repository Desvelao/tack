package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/claim"
	"tack/internal/query"
	"tack/internal/task"
)

// now is the clock of claims and leases (replaced in tests).
var now = time.Now

// agentFlag adds -as, defaulting to $TACK_AGENT.
func agentFlag(fs *flag.FlagSet) *string {
	return fs.String("as", os.Getenv("TACK_AGENT"), "who claims: becomes the assignee (default: $TACK_AGENT)")
}

// claimRequest checks the claim flags against the board; status is a column
// name or raw value ("" = keep). It prints the problem and returns ok false.
func claimRequest(a *app, cmd, agent string, lease time.Duration, status string, errw io.Writer) (claim.Request, int, bool) {
	if strings.TrimSpace(agent) == "" {
		fmt.Fprintf(errw, "%s: who claims? give -as NAME or set TACK_AGENT\n", cmd)
		return claim.Request{}, exitUsage, false
	}
	if lease < 0 {
		fmt.Fprintf(errw, "%s: -lease must not be negative\n", cmd)
		return claim.Request{}, exitUsage, false
	}
	r := claim.Request{Agent: agent, Lease: lease}
	if status != "" {
		b, err := a.board()
		if err != nil {
			return r, fail(errw, err), false
		}
		m, ok := b.Lookup(status)
		if !ok {
			return r, unknownStatus(errw, b, status), false
		}
		r.Status = m.Raw
	}
	return r, exitOK, true
}

func claimText(t task.Task) string {
	s := fmt.Sprintf("%s:%s claimed by %s", t.Backend, t.ID, t.Assignee)
	if t.Lease != "" {
		s += " until " + t.Lease
	}
	return s
}

// cmdClaim: `tack claim ID... -as AGENT [-lease D] [-status S]`. Every ID is
// resolved and checked first; the writes then go one by one, each guarded, and
// the first that loses a race stops the command (exit 4) after reporting what
// was already claimed.
func cmdClaim(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	agent := agentFlag(fs)
	lease := fs.Duration("lease", 0, "the claim runs out after this long (e.g. 30m); 0 = until released")
	status := fs.String("status", "", "also move the tasks to this column/status")
	asJSON := fs.Bool("json", false, "print the claimed tasks as JSON")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) < 1 {
		fmt.Fprintln(errw, "usage: tack claim ID... [-as AGENT] [-lease D] [-status S] [-b NAME] [-json]")
		return exitUsage
	}
	r, code, ok := claimRequest(a, "claim", *agent, *lease, *status, errw)
	if !ok {
		return code
	}
	tasks, err := a.findAll(ctx, *backendName, pos)
	if err != nil {
		return fail(errw, err)
	}
	for _, t := range tasks {
		if err := claim.Check(a.be, t, r); err != nil {
			fmt.Fprintln(errw, "claim:", err)
			return exitUsage
		}
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	var results []taskOut
	for _, t := range tasks {
		c, err := claim.Take(ctx, a.be, t, r, now())
		if err != nil {
			if *asJSON && len(results) > 0 {
				writeJSON(out, results)
			}
			if len(results) > 0 {
				fmt.Fprintf(errw, "claim: stopped after claiming %d of %d tasks\n", len(results), len(tasks))
			}
			return fail(errw, err)
		}
		results = append(results, withColumn(b, c))
		if !*asJSON {
			fmt.Fprintln(out, claimText(c))
		}
	}
	if r.Status != "" {
		warnOverLimitOf(ctx, a, r.Status, errw)
	}
	if *asJSON {
		return writeJSON(out, results)
	}
	return exitOK
}

// cmdRelease: `tack release ID... [-as AGENT | -force] [-status S]`.
func cmdRelease(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	agent := agentFlag(fs)
	force := fs.Bool("force", false, "release whoever holds the claim")
	status := fs.String("status", "", "also move the tasks to this column/status")
	asJSON := fs.Bool("json", false, "print the released tasks as JSON (with a changed flag)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) < 1 {
		fmt.Fprintln(errw, "usage: tack release ID... [-as AGENT | -force] [-status S] [-b NAME] [-json]")
		return exitUsage
	}
	if !*force && strings.TrimSpace(*agent) == "" {
		fmt.Fprintln(errw, "release: whose claim? give -as NAME, set TACK_AGENT or use -force")
		return exitUsage
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	raw := ""
	if *status != "" {
		m, ok := b.Lookup(*status)
		if !ok {
			return unknownStatus(errw, b, *status)
		}
		raw = m.Raw
	}
	tasks, err := a.findAll(ctx, *backendName, pos)
	if err != nil {
		return fail(errw, err)
	}
	type result struct {
		taskOut
		Changed bool `json:"changed"`
	}
	var results []result
	for i, t := range tasks {
		r, changed, err := claim.Give(ctx, a.be, t, *agent, *force, raw)
		if err != nil {
			if *asJSON && len(results) > 0 {
				writeJSON(out, results)
			}
			if i > 0 {
				fmt.Fprintf(errw, "release: stopped after %d of %d tasks\n", i, len(tasks))
			}
			return fail(errw, err)
		}
		results = append(results, result{withColumn(b, r), changed})
		if !*asJSON {
			if changed {
				fmt.Fprintf(out, "%s:%s released\n", r.Backend, r.ID)
			} else {
				fmt.Fprintf(out, "%s:%s not claimed\n", r.Backend, r.ID)
			}
		}
	}
	if raw != "" {
		warnOverLimitOf(ctx, a, raw, errw)
	}
	if *asJSON {
		return writeJSON(out, results)
	}
	return exitOK
}

// nextCandidates are the tasks an agent may take, most urgent first: in a
// column that is not finished, not blocked and not claimed (or the claim ran
// out). Ties keep the store order.
func nextCandidates(b board.Board, tasks []taskOut, at time.Time) []taskOut {
	var out []taskOut
	for _, o := range tasks {
		c := b.ColumnOf(o.Status)
		if c < 0 || b.Columns[c].Done || len(o.Blockers) > 0 || !claim.Free(o.Task, at) {
			continue
		}
		out = append(out, o)
	}
	keys := []board.SortKey{{Field: "priority"}}
	sort.SliceStable(out, func(i, j int) bool {
		return query.CompareBy(out[i].lookup(), out[j].lookup(), keys) < 0
	})
	return out
}

// cmdNext: `tack next [-q QUERY] [-v VIEW] [-claim -as AGENT ...]` prints the
// task to work on next; with -claim it also claims it, trying the next
// candidate when another agent wins the race. Exit 3 when there is none.
func cmdNext(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	fs.SetOutput(errw)
	status := fs.String("status", "", "only tasks in this column (or with this raw status value)")
	q := fs.String("q", "", "filter query (syntax and fields: tack help filter)")
	view := fs.String("v", "", "saved view (board.views) to apply; combined with -q")
	doClaim := fs.Bool("claim", false, "claim the task too (needs -as or TACK_AGENT)")
	agent := agentFlag(fs)
	lease := fs.Duration("lease", 0, "with -claim: the claim runs out after this long (e.g. 30m)")
	moveTo := fs.String("to", "", "with -claim: also move the task to this column/status")
	asJSON := fs.Bool("json", false, "print the task as JSON")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	if !*doClaim && (*lease != 0 || *moveTo != "") {
		fmt.Fprintln(errw, "next: -lease and -to need -claim")
		return exitUsage
	}
	var r claim.Request
	if *doClaim {
		var code int
		var ok bool
		if r, code, ok = claimRequest(a, "next", *agent, *lease, *moveTo, errw); !ok {
			return code
		}
	}
	filtered, _, code, ok := selectTasks(ctx, a, *status, *view, *q, errw)
	if !ok {
		return code
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	emit := func(o taskOut) int {
		if *asJSON {
			return writeJSON(out, o)
		}
		if *doClaim {
			fmt.Fprintf(out, "%s %s\n", claimText(o.Task), o.Title)
		} else {
			fmt.Fprintf(out, "%s:%s %s\n", o.Backend, o.ID, o.Title)
		}
		return exitOK
	}
	// A backend that cannot keep a claim only rules out its own tasks: the next
	// candidates may live in another one.
	unclaimable, raced := map[string]bool{}, false
	var skipped []string
	for _, o := range nextCandidates(b, filtered, now()) {
		if !*doClaim {
			return emit(o)
		}
		if unclaimable[o.Backend] {
			continue
		}
		if err := claim.Check(a.be, o.Task, r); err != nil {
			unclaimable[o.Backend] = true
			skipped = append(skipped, err.Error())
			continue
		}
		c, err := claim.Take(ctx, a.be, o.Task, r, now())
		if errors.Is(err, backend.ErrPrecondition) {
			raced = true
			continue // somebody was faster: take the next one
		}
		if err != nil {
			return fail(errw, err)
		}
		if r.Status != "" {
			warnOverLimitOf(ctx, a, r.Status, errw)
		}
		res := withColumn(b, c)
		res.Blockers = o.Blockers
		return emit(res)
	}
	if len(skipped) > 0 && !raced {
		// Every candidate sat in a backend that cannot hold the claim: that is a
		// setup problem, not an empty queue.
		for _, msg := range skipped {
			fmt.Fprintln(errw, "next:", msg)
		}
		return exitUsage
	}
	fmt.Fprintln(errw, "tack: no task is ready and unclaimed")
	return exitNotFound
}
