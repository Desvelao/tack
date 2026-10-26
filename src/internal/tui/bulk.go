package tui

import (
	"context"
	"fmt"
	"slices"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/backend"
	"tack/internal/task"
)

// deleteMany deletes the cards one by one in one background write. The ones
// that fail come back on the board (and stay marked); the others are one undo.
func (m *Model) deleteMany(ts []task.Task) tea.Cmd {
	be := m.be
	m.showLocalMany(ts, true)
	errs := make([]error, len(ts))
	return m.write(func() error {
		for i, t := range ts {
			errs[i] = be.Delete(context.Background(), t.Backend, t.ID)
		}
		return nil
	}, func(m *Model, _ error) {
		var gone, failed []task.Task
		var firstErr error
		marks := map[cardKey]bool{}
		for i, t := range ts {
			if errs[i] != nil {
				failed = append(failed, t)
				marks[keyOf(t)] = true
				if firstErr == nil {
					firstErr = fmt.Errorf("#%s: %w", t.ID, errs[i])
				}
				continue
			}
			gone = append(gone, t)
		}
		if len(marks) == 0 {
			marks = nil
		}
		m.marks = marks
		if len(failed) > 0 {
			m.showLocalMany(failed, false)
		}
		if len(gone) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("delete of %d cards", len(gone)), prev: gone[0], recreate: true, group: gone})
			if m.async == nil && m.reload() != nil {
				return
			}
		}
		if len(failed) > 0 {
			m.fail(fmt.Sprintf("deleted %d of %d cards; %v", len(gone), len(ts), firstErr))
			return
		}
		m.ok(fmt.Sprintf("deleted %d cards (u undoes)", len(gone)))
	})
}

// recreateGroup is the undo of deleteMany: it creates the cards again.
func (m *Model) recreateGroup(ctx context.Context, op undoOp) tea.Cmd {
	be := m.be
	made := make([]task.Task, len(op.group))
	errs := make([]error, len(op.group))
	return m.write(func() error {
		for i, t := range op.group {
			made[i], errs[i] = be.Create(ctx, t)
		}
		return nil
	}, func(m *Model, _ error) {
		var failed []task.Task
		renamed := false
		for i, t := range op.group {
			if errs[i] != nil {
				failed = append(failed, t)
				continue
			}
			if made[i].ID != t.ID {
				renamed = true
			}
			if m.async != nil {
				m.showLocal(made[i], false)
			}
		}
		if m.async == nil && m.reload() != nil {
			return
		}
		if len(failed) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("delete of %d cards", len(failed)), prev: failed[0], recreate: true, group: failed})
			m.fail(fmt.Sprintf("undo restored %d of %d cards: %v", len(op.group)-len(failed), len(op.group), errs[slices.IndexFunc(errs, func(e error) bool { return e != nil })]))
			return
		}
		m.ok("undid " + op.what)
		if renamed {
			m.notice += " (some got new IDs)"
		}
	})
}

// moveMany moves the cards to column target in one background write. The ones
// that fail go back (and stay marked); the others are one undo.
func (m *Model) moveMany(ts []task.Task, target int) tea.Cmd {
	be := m.be
	name := m.board.Columns[target].Name
	moved := make([]task.Task, len(ts))
	for i, t := range ts {
		moved[i] = t
		moved[i].Status = m.board.WriteValue(target)
	}
	m.showLocalMany(moved, false)
	errs := make([]error, len(ts))
	return m.write(func() error {
		for i := range ts {
			ctx := backend.WithOld(backend.WithNote(context.Background(), "moved to "+name), ts[i])
			errs[i] = be.Update(ctx, moved[i])
		}
		return nil
	}, func(m *Model, _ error) {
		var done, doneAfter, failed []task.Task
		var firstErr error
		marks := map[cardKey]bool{}
		for i, t := range ts {
			if errs[i] != nil {
				failed = append(failed, t)
				marks[keyOf(t)] = true
				if firstErr == nil {
					firstErr = fmt.Errorf("#%s: %w", t.ID, errs[i])
				}
				continue
			}
			done, doneAfter = append(done, t), append(doneAfter, moved[i])
		}
		if len(marks) == 0 {
			marks = nil
		}
		m.marks = marks
		if len(failed) > 0 {
			m.showLocalMany(failed, false)
		}
		if len(done) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("move of %d cards", len(done)), prev: done[0], group: done, afters: doneAfter})
			if m.async == nil && m.reload() != nil {
				return
			}
		}
		if len(failed) > 0 {
			m.fail(fmt.Sprintf("moved %d of %d cards to %s; %v", len(done), len(ts), name, firstErr))
			return
		}
		m.ok(fmt.Sprintf("moved %d cards to %s (u undoes)", len(done), name))
		if lim, over := m.board.Over(target, m.columnTotal(target)); over {
			m.notice += fmt.Sprintf(" (over its limit of %d)", lim)
		}
	})
}

// restoreGroup is the undo of moveMany: it writes the earlier versions back.
func (m *Model) restoreGroup(ctx context.Context, op undoOp) tea.Cmd {
	be := m.be
	if m.async != nil {
		m.showLocalMany(op.group, false)
	}
	errs := make([]error, len(op.group))
	return m.write(func() error {
		for i, t := range op.group {
			var after task.Task
			if i < len(op.afters) {
				after = op.afters[i]
			}
			errs[i] = be.Update(undoCtx(ctx, after), t)
		}
		return nil
	}, func(m *Model, _ error) {
		var failed, failedAfter []task.Task
		var firstErr error
		for i, t := range op.group {
			if errs[i] != nil {
				failed = append(failed, t)
				if i < len(op.afters) {
					failedAfter = append(failedAfter, op.afters[i])
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("#%s: %w", t.ID, errs[i])
				}
			}
		}
		if len(failed) > 0 {
			m.pushUndo(undoOp{what: fmt.Sprintf("move of %d cards", len(failed)), prev: failed[0], group: failed, afters: failedAfter})
			if m.async == nil {
				m.reload()
			}
			m.fail(fmt.Sprintf("undo restored %d of %d cards; %v", len(op.group)-len(failed), len(op.group), firstErr))
			return
		}
		if m.async == nil && m.reload() != nil {
			return
		}
		m.ok("undid " + op.what)
	})
}
