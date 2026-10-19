// Package outcome applies what an action asks for once a run has ended: copy
// the start of its output to the task's notes (capture: notes) and move the
// task (on_success, on_failure). The CLI (`tkan run`) and the board share it.
package outcome

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"tkan/internal/action"
	"tkan/internal/actionlog"
	"tkan/internal/backend"
	"tkan/internal/board"
	"tkan/internal/task"
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
// A task whose status changed during the run (an agent ran `tkan report
// -status`, or someone moved it) is not moved again: what the agent said
// wins. The notes are still appended. after is the task's status afterwards
// ("" when nothing was written).
func Apply(ctx context.Context, be *backend.Set, b board.Board, s action.Spec, t task.Task, rec actionlog.Record, canceled bool) (after string, err error) {
	if canceled || !Wants(s) {
		return "", nil
	}
	var errs []error
	want := s.OnSuccess
	if rec.Result != actionlog.ResultOK {
		want = s.OnFailure
	}
	move := want.Status
	raw := ""
	if move != "" {
		if m, ok := b.Lookup(move); ok {
			raw = m.Raw
		} else {
			errs = append(errs, fmt.Errorf("%s: no status or column %q to move the task to", s.Name, move))
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
	// The command (or an agent's `tkan report`) wrote the store behind this
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
