package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/backend"
	"tack/internal/outcome"
)

// cmdReport lets an agent started by `tack run` say how it went: it moves the
// task (-status) and/or appends a note, tied to the run by $TACK_RUN_ID.
func cmdReport(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	status := fs.String("status", "", "move the task to this column name or raw status value")
	note := fs.String("note", "", "append this text to the task's notes (- reads it from stdin)")
	runID := fs.String("run", os.Getenv("TACK_RUN_ID"), "run this report belongs to (default: $TACK_RUN_ID)")
	asJSON := fs.Bool("json", false, "print the updated task as JSON (like show -json)")
	pos, err := parseArgs(fs, args)
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if err != nil || len(pos) != 1 || !(given["status"] || given["note"]) {
		fmt.Fprintln(errw, "usage: tack report ID [-status S] [-note TEXT|-] [-run RUN_ID] [-b NAME] [-json]  (at least one of -status, -note)")
		return exitUsage
	}
	if given["note"] && *note == "-" {
		used := false
		text, err := readText("-", &used)
		if err != nil {
			fmt.Fprintf(errw, "report: -note: %v\n", err)
			return exitUsage
		}
		*note = text
	}
	*note = strings.TrimSpace(*note)
	if given["note"] && *note == "" && !given["status"] {
		fmt.Fprintln(errw, "report: -note is empty")
		return exitUsage
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	raw := ""
	if given["status"] {
		m, ok := b.Lookup(*status)
		if !ok {
			return unknownStatus(errw, b, *status)
		}
		raw = m.Raw
	}
	t, err := a.find(ctx, *backendName, pos[0])
	if err != nil {
		return fail(errw, err)
	}
	old, next := t, t
	if raw != "" {
		next.Status = raw
	}
	if *note != "" {
		line := "[report " + now().UTC().Format(time.RFC3339)
		if *runID != "" {
			line += " run " + *runID
		}
		line += "] " + *note
		add := func(notes string) string { return outcome.AppendBlock(notes, line) }
		next.Notes = add(next.Notes)
		if next, err = updateAppendingNotes(ctx, a, ctx, old, next, add); err != nil {
			return fail(errw, err)
		}
	} else if err := a.be.Update(backend.WithOld(ctx, old), next); err != nil {
		return fail(errw, err)
	}
	if raw != "" {
		warnOverLimitOf(ctx, a, raw, errw)
	}
	res := audit.ResultOK
	a.audit.Record(audit.Entry{Op: audit.OpReport, Backend: t.Backend, ID: t.ID, Result: res, Details: reportDetails(*runID, raw, *note)})
	rec := actionlog.Record{Time: now(), Ended: now(), Source: "report", Action: "report", Mode: "cli", Backend: t.Backend, ID: t.ID,
		Title: t.Title, Result: actionlog.ResultOK, RunID: *runID, ReportStatus: raw, ReportNote: *note}
	if err := a.actionLog.Append(rec); err != nil {
		fmt.Fprintln(errw, "tack:", err)
	}
	if *asJSON {
		return writeJSON(out, withColumn(b, next))
	}
	fmt.Fprintf(out, "%s:%s reported", t.Backend, t.ID)
	if raw != "" {
		fmt.Fprintf(out, " -> %s", raw)
	}
	fmt.Fprintln(out)
	return exitOK
}

func reportDetails(runID, status, note string) string {
	parts := []string{"report"}
	if runID != "" {
		parts = append(parts, "run "+runID)
	}
	if status != "" {
		parts = append(parts, "status "+status)
	}
	if note != "" {
		parts = append(parts, fmt.Sprintf("note %d B", len(note))) // the text stays out of the audit file
	}
	return strings.Join(parts, " ")
}
