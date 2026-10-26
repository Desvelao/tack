package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"tack/internal/backend"
	"tack/internal/task"
)

// undoOp reverses one change: restore the task as it was (move, edit), or
// create it again (delete, which may give it a new ID).
type undoOp struct {
	what     string
	prev     task.Task
	after    task.Task // move, edit: the card as the change wrote it
	recreate bool
	group    []task.Task // recreate: all the cards of a delete of marked cards (prev is the first)
	afters   []task.Task // a move of marked cards: the cards as it wrote them, one per group entry
}

const maxUndo = 30

// undoCtx makes the undo write only what the change did: the fields that differ
// between after (the card as the change wrote it) and the earlier version. A
// claim or note somebody added since is not written over.
func undoCtx(ctx context.Context, after task.Task) context.Context {
	if after.ID == "" {
		return ctx
	}
	return backend.WithOld(ctx, after)
}

func (m *Model) pushUndo(op undoOp) {
	m.undo = append(m.undo, op)
	if len(m.undo) > maxUndo {
		m.undo = m.undo[1:]
	}
}

// undoLast reverses the most recent move, edit or delete.
func (m *Model) undoLast() tea.Cmd {
	if len(m.undo) == 0 {
		m.notice = "nothing to undo"
		return nil
	}
	op := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	ctx := backend.WithSource(backend.WithNote(context.Background(), "undo of "+op.what), "undo")
	be := m.be
	if op.recreate && len(op.group) > 1 {
		return m.recreateGroup(ctx, op)
	}
	if op.recreate {
		var t task.Task
		return m.write(func() (err error) { t, err = be.Create(ctx, op.prev); return }, func(m *Model, err error) {
			if err != nil {
				m.fail("undo failed: " + err.Error())
				return
			}
			if m.async != nil {
				m.showLocal(t, false)
			} else if m.reload() != nil {
				return
			}
			m.selectID(t.Backend, t.ID)
			m.ok("undid " + op.what)
			if t.ID != op.prev.ID {
				m.notice += fmt.Sprintf(" (restored as #%s, the old #%s is gone)", t.ID, op.prev.ID)
			}
		})
	}
	if len(op.group) > 1 {
		return m.restoreGroup(ctx, op)
	}
	cur, shown := m.cardOf(op.prev)
	hidden := false
	if m.async != nil { // show the old card at once; the write confirms it
		m.showLocal(op.prev, false)
		hidden = !m.selectID(op.prev.Backend, op.prev.ID) && m.filter != ""
	}
	return m.write(func() error { return be.Update(undoCtx(ctx, op.after), op.prev) }, func(m *Model, err error) {
		if err != nil {
			m.fail("undo failed: " + err.Error())
			if shown {
				m.showLocal(cur, false)
			}
			return
		}
		if m.async == nil {
			if m.reload() != nil {
				return
			}
			hidden = !m.selectID(op.prev.Backend, op.prev.ID) && m.filter != ""
		}
		if hidden {
			m.ok("undid " + op.what + hiddenHint)
			return
		}
		m.ok("undid " + op.what)
	})
}
