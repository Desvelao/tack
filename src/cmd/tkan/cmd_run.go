package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"tkan/internal/action"
	"tkan/internal/actionlog"
	"tkan/internal/audit"
	"tkan/internal/backend"
	"tkan/internal/claim"
	"tkan/internal/outcome"
	"tkan/internal/task"
)

// cmdRun starts a configured action on tasks: `tkan run ACTION ID...`. With
// several IDs the runs go one after another, in the order given; every ID is
// resolved first, so an unknown one starts nothing (exit 3). A run that fails
// does not stop the others; the exit code is 1 if any failed.
func cmdRun(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("b", "", "backend name (needed when the ID exists in several)")
	interactive := fs.Bool("i", false, "run with the terminal, whatever the action's mode")
	dry := fs.Bool("n", false, "print the commands that would run and exit")
	force := fs.Bool("force", false, "run on a blocked task too (one whose blocked_by is not finished)")
	asJSON := fs.Bool("json", false, "print a JSON summary of each run (result, exit code, duration, run id, log path); the command's stdout goes to stderr")
	propagate := fs.Bool("propagate-exit", false, "exit with the command's own exit status (1-125) when it fails, instead of 1")
	agent := fs.String("as", os.Getenv("TKAN_AGENT"), "who claims the task for an action with claim: true (default: $TKAN_AGENT, else the action name)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) < 2 {
		fmt.Fprintln(errw, "usage: tkan run ACTION ID... [-b NAME] [-i] [-n] [-force] [-as AGENT] [-json] [-propagate-exit]")
		return exitUsage
	}
	all := a.cfg.AllActions()
	if names := action.Names(all); !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, pos[0]) }) {
		if len(names) == 0 {
			fmt.Fprintf(errw, "tkan: no action %q (the config defines no `actions:`; see docs/config.md)\n", pos[0])
		} else {
			fmt.Fprintf(errw, "tkan: no action %q (actions: %s)\n", pos[0], strings.Join(names, ", "))
		}
		return exitUsage
	}
	// Resolve every ID first so a bad one starts nothing; a task named twice runs once.
	var tasks []task.Task
	seen := map[string]bool{}
	for _, id := range pos[1:] {
		t, err := a.find(ctx, *backendName, id)
		if err != nil {
			return fail(errw, err)
		}
		if k := t.Backend + ":" + t.ID; !seen[k] {
			seen[k] = true
			tasks = append(tasks, t)
		}
	}
	// Each task runs the action as its backend defines it (or the top-level one).
	specs := make([]action.Spec, len(tasks))
	for i, t := range tasks {
		s, ok := action.Find(action.For(all, t.Backend), pos[0])
		if !ok {
			fmt.Fprintf(errw, "tkan: action %q does not apply to the tasks of backend %s (%s:%s); nothing started\n", pos[0], t.Backend, t.Backend, t.ID)
			return exitUsage
		}
		specs[i] = s
		if i == 0 || !slices.ContainsFunc(specs[:i], func(o action.Spec) bool { return o.Backend == s.Backend }) {
			for _, w := range s.Warnings() {
				fmt.Fprintf(errw, "tkan: warning: action %s: %s\n", s.Name, w)
			}
		}
	}
	if !*force {
		if code, ok := refuseBlocked(ctx, a, tasks, errw); !ok {
			return code
		}
	}
	if code, ok := refuseClaimed(a, tasks, specs, *agent, errw); !ok {
		return code
	}
	baseDir := a.cfg.Dir()
	if *dry {
		for i, t := range tasks {
			p := specs[i].Expand(t, baseDir)
			if p.Err != nil {
				return fail(errw, p.Err)
			}
			fmt.Fprintf(out, "cd %s && %s\n", action.ShellWord(p.Dir), p.Line())
		}
		return exitOK
	}

	// With -json, stdout carries only the summary: the command's own goes to stderr.
	childOut := out
	if *asJSON {
		childOut = errw
	}
	var failed []string
	var summaries []runSummary
	exitCode := exitOK
	for i, t := range tasks {
		spec := specs[i]
		if len(tasks) > 1 {
			fmt.Fprintf(errw, "tkan: %s on %s:%s (%d of %d)\n", spec.Name, t.Backend, t.ID, i+1, len(tasks))
		}
		res := runClaimed(ctx, a, spec, t, *agent, *interactive, childOut, errw)
		if *asJSON {
			summaries = append(summaries, a.summary(ctx, spec, t, res))
		}
		if ctx.Err() != nil { // interrupted: the command is dead and recorded
			fmt.Fprintf(errw, "tkan: interrupted: %s canceled", spec.Name)
			if left := len(tasks) - i - 1; left > 0 {
				fmt.Fprintf(errw, " (%d not started)", left)
			}
			fmt.Fprintln(errw)
			if *asJSON {
				writeJSON(out, summaries)
			}
			return exitInterrupted
		}
		if res.err == nil {
			continue
		}
		code := exitError
		if res.rec == nil { // never ran: claim refused, bad directory, ...
			code = failCode(res.err)
		} else if c := res.rec.ExitCode; *propagate && c != nil && *c >= 1 && *c <= 125 {
			code = *c
		}
		if exitCode == exitOK {
			exitCode = code
		}
		fmt.Fprintln(errw, "tkan:", res.err)
		failed = append(failed, t.Backend+":"+t.ID)
	}
	if *asJSON {
		if code := writeJSON(out, summariesOut(summaries, len(tasks))); code != exitOK {
			return code
		}
	}
	if len(failed) > 1 {
		fmt.Fprintf(errw, "tkan: %s failed on %d of %d tasks: %s\n", specs[0].Name, len(failed), len(tasks), strings.Join(failed, " "))
	}
	return exitCode
}

// runSummary is one run as `tkan run -json` prints it.
type runSummary struct {
	Backend      string `json:"backend"`
	ID           string `json:"id"`
	Action       string `json:"action"`
	RunID        string `json:"run_id,omitempty"`
	Result       string `json:"result"` // ok | error
	ExitCode     *int   `json:"exit_code,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	Truncated    bool   `json:"truncated,omitempty"`
	Log          string `json:"log,omitempty"` // the action log (file or directory) holding the full record
	Error        string `json:"error,omitempty"`
	StatusBefore string `json:"status_before"`
	StatusAfter  string `json:"status_after,omitempty"`
}

// summariesOut is one object for one task, a list for several.
func summariesOut(s []runSummary, n int) any {
	if n == 1 && len(s) == 1 {
		return s[0]
	}
	if s == nil {
		return []runSummary{}
	}
	return s
}

func (a *app) summary(ctx context.Context, spec action.Spec, t task.Task, res runResult) runSummary {
	s := runSummary{Backend: t.Backend, ID: t.ID, Action: spec.Name, Result: actionlog.ResultOK, StatusBefore: t.Status, Log: a.actionLog.Path()}
	if res.err != nil {
		s.Result, s.Error = actionlog.ResultError, strings.TrimPrefix(res.err.Error(), spec.Name+": ")
	}
	if r := res.rec; r != nil {
		s.RunID, s.ExitCode, s.DurationMS, s.Truncated = r.RunID, r.ExitCode, r.DurationMS, r.Truncated
	}
	_ = a.be.Reload() // the command may have written the store behind the cached lists
	if now, err := a.be.Find(context.WithoutCancel(ctx), t.Backend, t.ID); err == nil {
		s.StatusAfter = now.Status
	}
	return s
}

// failCode is the exit status of an error that is not a failed command.
func failCode(err error) int {
	var sink strings.Builder
	return fail(&sink, err)
}

// refuseBlocked checks that none of tasks is blocked (a blocked_by task not in
// a finished column). If one is, nothing runs: ok is false and code is
// exitPrecondition. A backend that fails to load only hides its tasks, which
// then do not block (like unknown ids).
func refuseBlocked(ctx context.Context, a *app, tasks []task.Task, errw io.Writer) (int, bool) {
	if !slices.ContainsFunc(tasks, func(t task.Task) bool { return t.BlockedBy != "" }) {
		return exitOK, true // no dependencies: no need to list everything
	}
	all, err := a.be.List(ctx)
	var perr *backend.PartialError
	if err != nil && !errors.As(err, &perr) {
		return fail(errw, err), false
	}
	b, err := a.board()
	if err != nil {
		return fail(errw, err), false
	}
	deps := b.Deps(all)
	var blocked []string
	for _, t := range tasks {
		if open := deps.Blockers(t); len(open) > 0 {
			blocked = append(blocked, fmt.Sprintf("%s:%s (blocked by %s)", t.Backend, t.ID, strings.Join(open, ", ")))
		}
	}
	if len(blocked) == 0 {
		return exitOK, true
	}
	fmt.Fprintf(errw, "tkan: precondition failed: not ready: %s; nothing started (-force runs anyway)\n", strings.Join(blocked, "; "))
	return exitPrecondition, false
}

// refuseClaimed checks, before anything starts, that every task of a
// `claim: true` action is free or already held by agent, and that its backend
// can store the claim. A task held by another agent gives exitPrecondition.
func refuseClaimed(a *app, tasks []task.Task, specs []action.Spec, agent string, errw io.Writer) (int, bool) {
	at := now()
	var held []string
	for i, t := range tasks {
		if !specs[i].Claim {
			continue
		}
		agent := agentFor(agent, specs[i])
		r := claim.Request{Agent: agent, Lease: specs[i].ClaimLease(false)}
		if err := claim.Check(a.be, t, r); err != nil {
			fmt.Fprintf(errw, "tkan: action %s has claim: true, but %v\n", specs[i].Name, err)
			return exitUsage, false
		}
		if holder, until, ok := claim.Holder(t, at); ok && holder != agent {
			s := fmt.Sprintf("%s:%s (claimed by %s", t.Backend, t.ID, holder)
			if !until.IsZero() {
				s += " until " + until.UTC().Format(time.RFC3339)
			}
			held = append(held, s+")")
		}
	}
	if len(held) == 0 {
		return exitOK, true
	}
	fmt.Fprintf(errw, "tkan: precondition failed: %s; nothing started\n", strings.Join(held, "; "))
	return exitPrecondition, false
}

// agentFor is who claims for spec: the -as / $TKAN_AGENT name, else the
// action's own name, so the fleet shows which agent holds a task.
func agentFor(agent string, spec action.Spec) string {
	if a := strings.TrimSpace(agent); a != "" {
		return a
	}
	if n := strings.TrimSpace(spec.Name); n != "" {
		return n
	}
	return "tkan-run"
}

// runResult is how one run went: rec is nil when the command never started.
type runResult struct {
	rec *actionlog.Record
	err error
}

// runClaimed is runOne, with the task claimed for agent around the run when
// spec has claim: true. The claim lasts as long as the run may; the release
// afterwards is guarded, so a claim taken over meanwhile is left alone. What
// the action asks for (capture, on_success, on_failure) is applied after the
// release, so it is not written over.
func runClaimed(ctx context.Context, a *app, spec action.Spec, t task.Task, agent string, interactive bool, out, errw io.Writer) runResult {
	spec = spec.WithRun(actionlog.NewRunID(), a.actionLog.Path())
	claimed := false
	if spec.Claim {
		agent = agentFor(agent, spec)
		r := claim.Request{Agent: agent, Lease: spec.ClaimLease(interactive || spec.Mode == action.ModeInteractive)}
		c, err := claim.Take(ctx, a.be, t, r, now())
		if err != nil {
			return runResult{err: err}
		}
		t, claimed = c, true
	}
	rec, runErr := runOne(ctx, a, spec, t, interactive, out, errw)
	if claimed {
		// Released even when the run was interrupted: the context may be canceled.
		if _, _, err := claim.Give(context.WithoutCancel(ctx), a.be, t, agent, false, ""); err != nil {
			fmt.Fprintf(errw, "tkan: release %s:%s: %v\n", t.Backend, t.ID, err)
		}
	}
	if rec != nil && outcome.Wants(spec) {
		if b, err := a.board(); err != nil {
			fmt.Fprintln(errw, "tkan:", err)
		} else if _, err := outcome.Apply(context.WithoutCancel(ctx), a.be, b, spec, t, *rec, ctx.Err() != nil); err != nil {
			fmt.Fprintln(errw, "tkan:", err)
		}
	}
	return runResult{rec: rec, err: runErr}
}

// runOne runs spec on t and records it (audit trail, action log, hooks); the
// error is the run's failure, if any. The record is nil when nothing started.
func runOne(ctx context.Context, a *app, spec action.Spec, t task.Task, interactive bool, out, errw io.Writer) (*actionlog.Record, error) {
	baseDir := a.cfg.Dir()
	a.hooks.FireAction(t, spec.Name, a.src(), nil)
	var rec actionlog.Record
	if interactive || spec.Mode == action.ModeInteractive {
		cmd, err := spec.Cmd(t, baseDir)
		if err != nil {
			return nil, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, errw
		started := time.Now()
		var runErr error
		var exit *int
		if err := cmd.Run(); err != nil {
			runErr = fmt.Errorf("%s: %w", spec.Name, err)
		}
		if st := cmd.ProcessState; st != nil && st.Exited() {
			c := st.ExitCode()
			exit = &c
		}
		rec = actionlog.Interactive(spec, t, spec.Expand(t, baseDir), started, time.Now(), runErr, a.src())
		rec.ExitCode = exit
	} else {
		// stdout and stderr appear while the command runs. stderr is kept for the
		// end, as before (on failure the error message already carries it).
		so := &streamWriter{w: out}
		j := spec.StartStreaming(ctx, t, baseDir, so, nil)
		<-j.Done()
		snap := j.Snapshot()
		so.finish()
		if s := strings.TrimSpace(snap.Stderr); s != "" && snap.Err == nil {
			fmt.Fprintln(errw, s)
		}
		rec = actionlog.FromJob(j, a.src())
	}
	a.audit.Record(audit.Entry{Op: audit.OpRun, Backend: t.Backend, ID: t.ID, Result: auditResult(rec), Details: rec.Summary()})
	if err := a.actionLog.Append(rec); err != nil {
		fmt.Fprintln(errw, "tkan:", err)
	}
	a.hooks.FireAction(t, spec.Name, a.src(), &rec)
	if rec.Error != "" {
		return &rec, fmt.Errorf("%s: %s", spec.Name, rec.Error)
	}
	return &rec, nil
}

// auditResult maps a run record to the audit trail's result values.
func auditResult(r actionlog.Record) string {
	if r.Result == actionlog.ResultOK {
		return audit.ResultOK
	}
	return audit.ResultError
}

// streamWriter passes the command's stdout through as it arrives and, at the
// end, makes sure the output ends with a newline (as the printed result always
// did). Leading and trailing blank lines are not trimmed any more.
type streamWriter struct {
	mu   sync.Mutex
	w    io.Writer
	last byte
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > 0 {
		s.last = p[len(p)-1]
	}
	_, _ = s.w.Write(p) // a closed pipe must not make the command fail
	return len(p), nil
}

func (s *streamWriter) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != 0 && s.last != '\n' {
		fmt.Fprintln(s.w)
	}
}
