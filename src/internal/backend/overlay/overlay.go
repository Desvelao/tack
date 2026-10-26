// Package overlay wraps a backend so that some logical fields (the claim
// fields assignee, lease and claimed_at, say) live in a local JSON file
// instead of the store: a store that cannot hold them, or that the user may
// not write them to (a project shared with others), still gets claims,
// priorities or whatever is overlaid. The overlay replaces the store's value
// on read and takes the writes of those fields; everything else goes to the
// wrapped backend untouched.
package overlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"tack/internal/backend"
	"tack/internal/backend/filestore"
	"tack/internal/task"
)

// ClaimFields are the fields of the `claims` shorthand.
var ClaimFields = []string{"assignee", "lease", "claimed_at"}

// Backend is the wrapper; build it with Wrap.
type Backend struct {
	inner  backend.Backend
	path   string
	fields []string // overlaid logical fields, in task.FieldNames order

	mu       sync.Mutex
	stamp    filestore.Stamp // the overlay file as last read or written
	seen     bool            // stamp is valid
	pruneOff bool            // pruning is not allowed here (read-only dir, say): not tried again
}

// Wrap overlays fields of inner with the JSON file at path. A field must be a
// logical field other than the id.
func Wrap(inner backend.Backend, path string, fields []string) (*Backend, error) {
	if path == "" {
		return nil, errors.New("overlay: no file")
	}
	if len(fields) == 0 {
		return nil, errors.New("overlay: no fields")
	}
	var ordered []string
	for _, f := range task.FieldNames {
		if f == "id" {
			continue
		}
		if slices.Contains(fields, f) {
			ordered = append(ordered, f)
		}
	}
	for _, f := range fields {
		if !slices.Contains(ordered, f) {
			return nil, fmt.Errorf("overlay: %q is not a field that can be overlaid (one of %s)", f, strings.Join(task.FieldNames[1:], ", "))
		}
	}
	return &Backend{inner: inner, path: path, fields: ordered}, nil
}

// entries is the file: task id -> field -> value.
type entries map[string]map[string]string

func (b *Backend) read() (entries, error) {
	data, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return entries{}, nil
	}
	if err != nil {
		return nil, err
	}
	e := entries{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("overlay %s: %w", b.path, err)
		}
	}
	return e, nil
}

// readStamped is read plus the file's Stamp, remembered for Changed.
func (b *Backend) readStamped() (entries, error) {
	st, serr := filestore.StatFile(b.path)
	e, err := b.read()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if serr == nil {
		b.stamp, b.seen = st, true
	} else {
		b.stamp, b.seen = filestore.Stamp{}, true
	}
	b.mu.Unlock()
	return e, nil
}

func (b *Backend) write(e entries) error {
	for id, m := range e {
		if len(m) == 0 {
			delete(e, id)
		}
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	st, err := filestore.WriteAtomic(b.path, append(data, '\n'))
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.stamp, b.seen = st, true
	b.mu.Unlock()
	return nil
}

// locked runs fn with the overlay file locked and read.
func (b *Backend) locked(fn func(entries) error) error {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	unlock, err := filestore.LockFile(b.path+".lock", filestore.LockTimeout)
	if err != nil {
		return fmt.Errorf("lock %s: %w", b.path, err)
	}
	defer unlock()
	e, err := b.readStamped()
	if err != nil {
		return err
	}
	return fn(e)
}

func (b *Backend) has(field string) bool { return slices.Contains(b.fields, field) }

// apply lays the overlay entries over t and marks the fields it set.
func (b *Backend) apply(t task.Task, m map[string]string) task.Task {
	t.Overlaid = nil
	for _, f := range b.fields {
		if v, ok := m[f]; ok {
			*t.FieldPtr(f) = v
			t.Overlaid = append(t.Overlaid, f)
		}
	}
	return t
}

// withoutOverlay is t as the store should see it: overlaid fields set to
// those of like (the store's own values, or t's base).
func (b *Backend) withoutOverlay(t, like task.Task) task.Task {
	t = t.Clone()
	t.Overlaid = nil
	for _, f := range b.fields {
		*t.FieldPtr(f) = *like.FieldPtr(f)
	}
	return t
}

// List returns the wrapped backend's tasks with the overlay laid over them.
func (b *Backend) List(ctx context.Context) ([]task.Task, error) {
	ts, err := b.inner.List(ctx)
	var le *backend.LoadError
	if err != nil && !errors.As(err, &le) {
		return ts, err
	}
	e, rerr := b.readStamped()
	if rerr != nil {
		// Show the tasks as the store has them, with the problem on the side.
		return ts, &backend.LoadError{Errs: []error{rerr}}
	}
	present := make(map[string]bool, len(ts))
	for i := range ts {
		present[ts[i].ID] = true
		ts[i] = b.apply(ts[i], e[ts[i].ID])
	}
	if err == nil { // the store loaded in full, so a missing id really is gone
		for id := range e {
			if !present[id] {
				b.pruneStale(ctx)
				break
			}
		}
	}
	return ts, err
}

// pruneStale drops the entries of tasks the store no longer has (deleted by
// hand, a git pull, another tool): left alone they pile up, and a store that
// reuses ids would give a new task the old assignee and lease. Best effort: the
// list is read again under the lock, after a reload, so a task another process
// has just created is not mistaken for a missing one. A failure is retried on a
// later List, except one that says the overlay cannot be written (permissions,
// a read-only file system), which switches pruning off.
func (b *Backend) pruneStale(ctx context.Context) {
	b.mu.Lock()
	off := b.pruneOff
	b.mu.Unlock()
	if off {
		return
	}
	err := b.locked(func(e entries) error {
		if r, ok := b.inner.(backend.Reloader); ok {
			if err := r.Reload(); err != nil {
				return err
			}
		}
		ts, err := b.inner.List(ctx)
		if err != nil { // a partial list proves nothing
			return err
		}
		present := make(map[string]bool, len(ts))
		for _, t := range ts {
			present[t.ID] = true
		}
		dropped := false
		for id := range e {
			if !present[id] {
				delete(e, id)
				dropped = true
			}
		}
		if !dropped {
			return nil
		}
		return b.write(e)
	})
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
		b.mu.Lock()
		b.pruneOff = true
		b.mu.Unlock()
	}
}

// Statuses are the wrapped backend's, unless status is overlaid.
func (b *Backend) Statuses() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range b.inner.Statuses() {
		seen[s] = true
		out = append(out, s)
	}
	if b.has("status") {
		if e, err := b.read(); err == nil {
			ids := make([]string, 0, len(e))
			for id := range e {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, id := range ids {
				if s, ok := e[id]["status"]; ok && s != "" && !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// Update stores t: overlaid fields in the file, the rest in the wrapped backend.
func (b *Backend) Update(ctx context.Context, t task.Task) error {
	return b.write1(ctx, nil, t)
}

// Patch is Update that writes only what differs between base and t.
func (b *Backend) Patch(ctx context.Context, base, t task.Task) error {
	return b.write1(ctx, &base, t)
}

func (b *Backend) write1(ctx context.Context, base *task.Task, t task.Task) error {
	return b.locked(func(e entries) error {
		m := e[t.ID]
		if m == nil {
			m = map[string]string{}
		}
		// without a base, t is written over the task as it is now (store + overlay)
		var cur *task.Task
		if base == nil {
			c, err := b.storeTask(ctx, t.ID)
			if err != nil {
				return err
			}
			cur = &c
			merged := b.apply(c, m)
			base = &merged
		}
		if f, _, ok := backend.Expected(ctx); ok && (b.has(f) || cur != nil) {
			chk := *base
			if cur == nil { // the caller's base may be stale: the guard sees the file
				chk = b.apply(task.Task{ID: t.ID}, m)
			}
			if err := backend.CheckExpect(ctx, chk); err != nil {
				return err
			}
		}
		// The store's part: what t changes in the fields no overlay holds. The
		// overlaid ones are pinned to the base so they do not change there.
		bs := b.withoutOverlay(*base, *base)
		ts := b.withoutOverlay(t, *base)
		// A guard on an overlaid field was checked above; the store never
		// held that field, so it would refuse it.
		ictx := ctx
		if f, _, ok := backend.Expected(ctx); ok && b.has(f) {
			ictx = backend.WithoutExpect(ctx)
		}
		// Undoes the store's part if the overlay file cannot be written after it:
		// the two are not one transaction, but a failed call should not leave half of
		// an edit behind for the caller to retry on top of.
		var undo func() error
		if !sameFields(bs, ts, b.fields) || !sameExtra(bs.Extra, ts.Extra) {
			if p, ok := b.inner.(backend.Patcher); ok {
				if err := p.Patch(ictx, bs, ts); err != nil {
					return err
				}
				undo = func() error { return p.Patch(backend.WithoutExpect(ctx), ts, bs) }
			} else {
				if cur == nil {
					c, err := b.storeTask(ctx, t.ID)
					if err != nil {
						return err
					}
					cur = &c
				}
				if err := b.inner.Update(ictx, b.withoutOverlay(t, *cur)); err != nil {
					return err
				}
				before := *cur
				undo = func() error { return b.inner.Update(backend.WithoutExpect(ctx), before) }
			}
		}
		// The overlay's part: the fields that differ from the base.
		for _, f := range b.fields {
			v := *t.FieldPtr(f)
			if *base.FieldPtr(f) == v {
				continue
			}
			if v == "" {
				delete(m, f)
			} else {
				m[f] = v
			}
		}
		if len(m) == 0 {
			delete(e, t.ID)
		} else {
			e[t.ID] = m
		}
		err := b.write(e)
		if err != nil && undo != nil {
			if uerr := undo(); uerr != nil {
				return fmt.Errorf("%w (the change to the store could not be undone either: %v)", err, uerr)
			}
		}
		return err
	})
}

// storeTask is the task id as the wrapped backend holds it now.
func (b *Backend) storeTask(ctx context.Context, id string) (task.Task, error) {
	ts, err := b.inner.List(ctx)
	var le *backend.LoadError
	if err != nil && !errors.As(err, &le) {
		return task.Task{}, err
	}
	for _, c := range ts {
		if c.ID == id {
			return c, nil
		}
	}
	return task.Task{}, fmt.Errorf("%w: %s", backend.ErrNotFound, id)
}

func sameFields(a, c task.Task, skip []string) bool {
	for _, f := range task.FieldNames {
		if f == "id" || slices.Contains(skip, f) {
			continue
		}
		if *a.FieldPtr(f) != *c.FieldPtr(f) {
			return false
		}
	}
	return true
}

func sameExtra(a, c map[string]string) bool {
	if len(a) != len(c) {
		return false
	}
	for k, v := range a {
		if cv, ok := c[k]; !ok || cv != v {
			return false
		}
	}
	return true
}

// Create stores t in the wrapped backend and its overlaid fields in the file.
func (b *Backend) Create(ctx context.Context, t task.Task) (task.Task, error) {
	in := b.withoutOverlay(t, task.Task{})
	out, err := b.inner.Create(ctx, in)
	if err != nil {
		return out, err
	}
	m := map[string]string{}
	for _, f := range b.fields {
		if v := *t.FieldPtr(f); v != "" {
			m[f] = v
		}
	}
	// A store that reuses ids may have left an entry of an earlier task: the
	// new one must not inherit it.
	stale := false
	if cur, err := b.read(); err == nil {
		_, stale = cur[out.ID]
	}
	if len(m) > 0 || stale {
		if err := b.locked(func(e entries) error {
			if len(m) > 0 {
				e[out.ID] = m
			} else {
				delete(e, out.ID)
			}
			return b.write(e)
		}); err != nil {
			// Do not leave a task whose overlaid fields were lost: a retry would
			// create it twice.
			if derr := b.inner.Delete(ctx, out.ID); derr != nil {
				return out, fmt.Errorf("%w (and the created task %s could not be removed: %v)", err, out.ID, derr)
			}
			return task.Task{}, err
		}
	}
	return b.apply(out, m), nil
}

// Delete removes the task and its overlay entry.
func (b *Backend) Delete(ctx context.Context, id string) error {
	// The overlay is locked first so that a lock failure leaves both untouched.
	return b.locked(func(e entries) error {
		if err := b.inner.Delete(ctx, id); err != nil {
			return err
		}
		if _, ok := e[id]; !ok {
			return nil
		}
		delete(e, id)
		return b.write(e)
	})
}

// Close closes the wrapped backend.
func (b *Backend) Close() error { return b.inner.Close() }

// Changed reports a change of the wrapped store or of the overlay file.
func (b *Backend) Changed() (bool, error) {
	changed := false
	if w, ok := b.inner.(backend.Watcher); ok {
		c, err := w.Changed()
		if err != nil {
			return false, err
		}
		changed = c
	}
	cur, err := filestore.StatFile(b.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return changed, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen && !cur.Same(b.stamp) {
		changed = true
	}
	return changed, nil
}

// Reload re-reads the wrapped store when it can.
func (b *Backend) Reload() error {
	if r, ok := b.inner.(backend.Reloader); ok {
		return r.Reload()
	}
	return nil
}

// Guards: guards on overlaid fields are checked here under the file lock; any
// other is checked against a fresh read of the store.
func (b *Backend) Guards() bool { return true }

// Stores is true for the overlaid fields, else the wrapped backend's answer.
func (b *Backend) Stores(field string) bool {
	if b.has(field) {
		return true
	}
	if fs, ok := b.inner.(backend.FieldStorer); ok {
		return fs.Stores(field)
	}
	return true
}
