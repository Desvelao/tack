package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"tkan/internal/audit"
)

// auditOut is an entry as printed by -json, with how its session relates to this run.
type auditOut struct {
	audit.Entry
	SessionKind string `json:"session_kind"` // current | previous | the session id
}

// cmdAudit prints the audit trail (oldest first), filtered with the query language.
func cmdAudit(_ context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(errw)
	q := fs.String("q", "", "filter query; fields: "+strings.Join(audit.Fields, " ")+" (what they mean: tkan help filter audit)")
	fs.Usage = fieldsUsage(fs, "Fields of -q", audit.FieldDocs, audit.SearchFields)
	limit := fs.Int("limit", 0, "show only the last N matching entries (0 = all)")
	asJSON := fs.Bool("json", false, "JSON output")
	if code, ok := parseNoArgs(fs, args, errw); !ok {
		return code
	}
	if *limit < 0 {
		fmt.Fprintln(errw, "audit: -limit must be zero or more")
		return exitUsage
	}
	query, err := audit.Parse(*q)
	if err != nil {
		fmt.Fprintln(errw, "audit:", err)
		return exitUsage
	}
	entries, err := a.audit.Entries()
	if err != nil {
		return fail(errw, err)
	}
	sess := a.audit.SessionsOf(entries)
	entries = sess.Filter(entries, query)
	if *limit > 0 && len(entries) > *limit {
		entries = entries[len(entries)-*limit:]
	}
	if a.audit.Path() == "" {
		fmt.Fprintln(errw, "audit: no audit.file is configured, so a command only sees its own entries (see docs/config.md)")
	}
	if *asJSON {
		res := make([]auditOut, len(entries))
		for i, e := range entries {
			res[i] = auditOut{Entry: e, SessionKind: sess.Kind(e.Session)}
		}
		return writeJSON(out, res)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSESSION\tOP\tRESULT\tBACKEND\tID\tDETAILS")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format(time.DateTime), sess.Kind(e.Session), e.Op, e.Result, e.Backend, e.ID, oneLine(e.Details))
	}
	tw.Flush()
	return exitOK
}

// oneLine keeps a multi-line detail on its table row.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
