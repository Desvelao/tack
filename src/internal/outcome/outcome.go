// Package outcome applies what an action asks for once a run has ended: copy
// the start of its output to the task's notes (capture: notes) and move the
// task (on_success, on_failure). The CLI (`tack run`) and the board share it.
package outcome

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/task"
)

// Wants reports whether Apply would do anything for s.
func Wants(s action.Spec) bool {
	return s.Capture != "" || !s.OnSuccess.IsZero() || !s.OnFailure.IsZero()
}

// Apply does what s asks for the run recorded in rec, on t (the task as the
// run started; its status is what on_success and on_failure compare against).
// A canceled run changes nothing. Call it after the claim is released: it
// re-reads the task, so the release is not written over.
//
// A task whose status changed during the run (an agent ran `tack report
// -status`, or someone moved it) is not moved again: what the agent said
// wins. The notes are still appended. after is the task's status afterwards
// ("" when nothing was written).
func Apply(ctx context.Context, be *backend.Set, b board.Board, s action.Spec, t task.Task, rec actionlog.Record, canceled bool, baseDir string) (after string, err error) {
	if canceled || !Wants(s) {
		return "", nil
	}
	after, err = move(ctx, be, b, s, t, rec)
	if cerr := runCommand(ctx, be, s, t, rec, baseDir, after); cerr != nil {
		err = errors.Join(err, cerr)
	}
	return after, err
}

// wanted is the outcome s asks for after rec: on_success or on_failure.
func wanted(s action.Spec, rec actionlog.Record) (o action.Outcome, key string) {
	if rec.Result != actionlog.ResultOK {
		return s.OnFailure, "on_failure"
	}
	return s.OnSuccess, "on_success"
}

// runCommand runs the command of the outcome (if any) for the task as it is
// now, so it sees the status the move wrote. Its failure is only reported: the
// run's result stands. after is the status the move wrote ("" when none).
func runCommand(ctx context.Context, be *backend.Set, s action.Spec, t task.Task, rec actionlog.Record, baseDir, after string) error {
	want, key := wanted(s, rec)
	if len(want.Command) == 0 {
		return nil
	}
	_ = be.Reload()
	if cur, err := be.Find(ctx, t.Backend, t.ID); err == nil {
		t = cur
	} else if after != "" {
		t.Status = after
	}
	exit := "-1"
	if rec.ExitCode != nil {
		exit = strconv.Itoa(*rec.ExitCode)
	}
	cs := want.CommandSpec(s.Name, s).WithRun(rec.RunID, "")
	esc := strings.NewReplacer("{", "{{", "}", "}}")
	cs.Env["TACK_RESULT"], cs.Env["TACK_EXIT"] = esc.Replace(rec.Result), exit
	p := cs.ExpandWith(t, baseDir, map[string]string{"result": rec.Result, "exit": exit})
	if p.Err != nil {
		return fmt.Errorf("%s: %s command: %w", s.Name, key, p.Err)
	}
	j := cs.StartPlan(ctx, t, p)
	<-j.Done()
	if snap := j.Snapshot(); snap.Err != nil {
		msg := strings.TrimSpace(snap.Stderr)
		if msg != "" {
			msg = ": " + firstLine(msg)
		}
		return fmt.Errorf("%s: %s command: %w%s", s.Name, key, snap.Err, msg)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// move does the status move and the notes of Apply.
func move(ctx context.Context, be *backend.Set, b board.Board, s action.Spec, t task.Task, rec actionlog.Record) (after string, err error) {
	var errs []error
	want, _ := wanted(s, rec)
	raw := ""
	if to := want.Status; to != "" {
		if m, ok := b.Lookup(to); ok {
			raw = m.Raw
		} else {
			errs = append(errs, fmt.Errorf("%s: no status or column %q to move the task to", s.Name, to))
		}
	}
	block := ""
	switch {
	case want.AppendNotes != "":
		block = notesBlock(s, rec, want.AppendNotes)
	case s.Capture == action.CaptureNotes:
		block = notesBlock(s, rec, action.NotesStdout)
	}
	if raw == "" && block == "" {
		return "", errors.Join(errs...)
	}
	// The command (or an agent's `tack report`) wrote the store behind this
	// process; drop what the one-shot CLI cached before reading the task.
	_ = be.Reload()
	cur, err := be.Find(ctx, t.Backend, t.ID)
	if err != nil {
		return "", errors.Join(append(errs, fmt.Errorf("%s: %w", s.Name, err))...)
	}
	next := cur
	if block != "" {
		next.Notes = AppendBlock(cur.Notes, block)
	}
	wctx := backend.WithOld(ctx, cur)
	if raw != "" && cur.Status == t.Status && cur.Status != raw {
		next.Status = raw
		wctx = backend.WithExpect(wctx, "status", t.Status)
	}
	err = be.Update(wctx, next)
	if errors.Is(err, backend.ErrPrecondition) && next.Status != cur.Status && block != "" {
		// Moved by somebody meanwhile: keep the notes, leave the status alone.
		if cur, err = be.Find(ctx, t.Backend, t.ID); err == nil {
			next = cur
			next.Notes = AppendBlock(cur.Notes, block)
			err = be.Update(backend.WithOld(ctx, cur), next)
		}
	}
	if err != nil {
		return "", errors.Join(append(errs, fmt.Errorf("%s: %w", s.Name, err))...)
	}
	return next.Status, errors.Join(errs...)
}

// notesBlock is the text added to the notes: a header and the first part of
// what src names (stdout, the run's summary, or why it failed).
func notesBlock(s action.Spec, rec actionlog.Record, src string) string {
	var out string
	switch src {
	case action.NotesSummary:
		out = rec.Summary()
	case action.NotesError:
		out = strings.TrimSpace(rec.Error)
		if out == "" {
			out = strings.TrimSpace(rec.Stderr)
		}
	default:
		out = strings.TrimSpace(rec.Stdout)
	}
	if out == "" {
		return ""
	}
	cut := false
	if max := s.CaptureBytes(); len(out) > max {
		n := max
		for n > 0 && !utf8.RuneStart(out[n]) {
			n--
		}
		out, cut = strings.TrimSpace(out[:n]), true
	}
	head := "[run " + rec.RunID + " " + s.Name + " " + rec.Result
	if rec.ExitCode != nil {
		head += fmt.Sprintf(" exit %d", *rec.ExitCode)
	}
	head += "]"
	if cut {
		out += "\n[output cut]"
	}
	return head + "\n" + out
}

// AppendBlock adds block after the existing notes, a blank line apart.
func AppendBlock(notes, block string) string {
	if block == "" {
		return notes
	}
	if strings.TrimSpace(notes) == "" {
		return block
	}
	return strings.TrimRight(notes, "\n") + "\n\n" + block
}
