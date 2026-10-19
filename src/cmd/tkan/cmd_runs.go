package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"tkan/internal/actionlog"
)

// cmdRuns prints the stored action runs (action_log), oldest first.
func cmdRuns(_ context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(errw)
	q := fs.String("q", "", "filter (fields: "+strings.Join(actionlog.Fields, " ")+"; what they mean: tkan help filter runs)")
	fs.Usage = fieldsUsage(fs, "Fields of -q", actionlog.FieldDocs, actionlog.SearchFields)
	limit := fs.Int("limit", 0, "keep only the newest N runs (0 = all)")
	asJSON := fs.Bool("json", false, "JSON output with the full records (command, stdin, stdout, stderr)")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	query, err := actionlog.Parse(*q)
	if err != nil {
		fmt.Fprintln(errw, "tkan: -q:", err)
		return exitUsage
	}
	if !a.actionLog.Enabled() {
		fmt.Fprintln(errw, "tkan: action runs are not stored (set action_log: {file: actions.jsonl} or {dir: actions} in the config; see docs/config.md)")
		return exitError
	}
	recs, err := a.actionLog.Records(true)
	if err != nil {
		return fail(errw, err)
	}
	recs = actionlog.Filter(recs, query)
	if *limit > 0 && len(recs) > *limit {
		recs = recs[len(recs)-*limit:]
	}
	if *asJSON {
		if recs == nil {
			recs = []actionlog.Record{}
		}
		return writeJSON(out, recs)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tACTION\tEVENT\tRESULT\tBACKEND\tID\tTOOK\tTITLE")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Time.Local().Format(time.DateTime), r.Action, orDash(r.Event), r.Result, r.Backend, r.ID, actionlog.FormatDuration(r.Duration()), oneLine(r.Title))
	}
	if err := tw.Flush(); err != nil {
		return exitError
	}
	return exitOK
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
