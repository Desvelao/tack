package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"tkan/internal/task"
)

// Named is a configured backend instance.
type Named struct {
	Name    string
	Type    string
	Backend Backend
}

// Set combines the configured backends. Task IDs are only unique within one
// backend, so every task carries the Backend name it came from and writes are
// routed by it.
type Set struct {
	items []Named

	mu       sync.Mutex
	audit    Auditor                // nil = no audit trail
	listener WriteListener          // nil = no write listener
	needOld  bool                   // look the previous task up for updates (see SetWriteListener)
	written  map[string]bool        // tasks this Set wrote since TakeWritten -> exists after the write (only with a listener)
	caching  bool                   // see CacheLists
	cache    map[string][]task.Task // backend name -> its last successful list
}

// CacheLists makes List (and Find) reuse each backend's last successful list
// until a write goes through this Set. The one-shot CLI turns it on so a
// command-spawning backend is not run again for every internal lookup; the TUI
// leaves it off because it reloads on purpose.
func (s *Set) CacheLists() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caching, s.cache = true, map[string][]task.Task{}
}

// listBackend lists one backend, through the cache when it is on.
func (s *Set) listBackend(ctx context.Context, it Named) ([]task.Task, error) {
	s.mu.Lock()
	if ts, ok := s.cache[it.Name]; s.caching && ok {
		s.mu.Unlock()
		return append([]task.Task(nil), ts...), nil
	}
	s.mu.Unlock()
	ts, err := it.Backend.List(ctx)
	var le *LoadError
	if errors.As(err, &le) { // part of the store did not load: use the rest, but do not cache it
		return ts, err
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.caching {
		s.cache[it.Name] = append([]task.Task(nil), ts...)
	}
	s.mu.Unlock()
	return ts, nil
}

// invalidate drops cached lists after a write.
func (s *Set) invalidate() {
	s.mu.Lock()
	if s.caching {
		s.cache = map[string][]task.Task{}
	}
	s.mu.Unlock()
}

// NewSet wraps already-opened backends; the Set owns them (Close closes all).
func NewSet(items []Named) *Set { return &Set{items: items} }

// Names lists the backend names in config order.
func (s *Set) Names() []string {
	out := make([]string, len(s.items))
	for i, it := range s.items {
		out[i] = it.Name
	}
	return out
}

// Types lists the backend types in config order, parallel to Names.
func (s *Set) Types() []string {
	out := make([]string, len(s.items))
	for i, it := range s.items {
		out[i] = it.Type
	}
	return out
}

// Get returns the backend with the given name.
func (s *Set) Get(name string) (Backend, bool) {
	for _, it := range s.items {
		if strings.EqualFold(it.Name, name) {
			return it.Backend, true
		}
	}
	return nil, false
}

// canonical returns the configured spelling of name.
func (s *Set) canonical(name string) (string, bool) {
	for _, it := range s.items {
		if strings.EqualFold(it.Name, name) {
			return it.Name, true
		}
	}
	return "", false
}

// PartialError is returned by List when some backends failed. The tasks of the
// healthy backends are still returned next to it, so one unreachable backend
// does not blank the board.
type PartialError struct {
	Names []string // failed backends, config order
	Errs  []error  // parallel to Names
}

func (e *PartialError) Error() string {
	parts := make([]string, len(e.Names))
	for i, n := range e.Names {
		parts[i] = fmt.Sprintf("backend %s: %v", n, e.Errs[i])
	}
	return strings.Join(parts, "; ")
}

// Unwrap exposes the individual errors to errors.Is / errors.As.
func (e *PartialError) Unwrap() []error { return e.Errs }

// ListOne lists the i-th backend (config order) with its tasks tagged Backend,
// so a caller can load the backends one by one.
func (s *Set) ListOne(ctx context.Context, i int) ([]task.Task, error) {
	it := s.items[i]
	ts, err := s.listBackend(ctx, it)
	for j := range ts {
		ts[j].Backend = it.Name
	}
	return ts, err
}

// List returns every backend's tasks (config order order, store order within each),
// tagged with Backend. If some backends fail it returns the others' tasks and
// a *PartialError.
func (s *Set) List(ctx context.Context) ([]task.Task, error) {
	// Backends are queried concurrently (a slow or hung command backend must not
	// delay the others by its whole timeout); results are kept by index so the
	// merged order and the PartialError order stay the config order.
	lists := make([][]task.Task, len(s.items))
	errs := make([]error, len(s.items))
	if len(s.items) == 1 {
		lists[0], errs[0] = s.listBackend(ctx, s.items[0])
	} else {
		var wg sync.WaitGroup
		for i, it := range s.items {
			wg.Add(1)
			go func(i int, it Named) {
				defer wg.Done()
				lists[i], errs[i] = s.listBackend(ctx, it)
			}(i, it)
		}
		wg.Wait()
	}
	var out []task.Task
	var perr *PartialError
	for i, it := range s.items {
		if errs[i] != nil {
			if perr == nil {
				perr = &PartialError{}
			}
			perr.Names = append(perr.Names, it.Name)
			perr.Errs = append(perr.Errs, errs[i])
		}
		for _, t := range lists[i] {
			t.Backend = it.Name
			out = append(out, t)
		}
	}
	if perr != nil {
		return out, perr
	}
	return out, nil
}

// Statuses is the union of the raw statuses of all backends, in order of first appearance.
func (s *Set) Statuses() []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range s.items {
		for _, st := range it.Backend.Statuses() {
			if !seen[st] {
				seen[st] = true
				out = append(out, st)
			}
		}
	}
	return out
}

// Stores reports whether the backend named name keeps a value written to the
// logical field (see FieldStorer); false for an unknown backend.
func (s *Set) Stores(name, field string) bool {
	b, err := s.route(name)
	if err != nil {
		return false
	}
	if fs, ok := b.(FieldStorer); ok {
		return fs.Stores(field)
	}
	return true
}

func (s *Set) route(name string) (Backend, error) {
	if name == "" && len(s.items) > 0 {
		return s.items[0].Backend, nil
	}
	b, ok := s.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (configured: %s)", name, strings.Join(s.Names(), ", "))
	}
	return b, nil
}

// Auditor receives one call per audited action; op is create, update, delete
// or reload, err is nil on success. The audit package's Log implements it.
type Auditor interface {
	Audit(op, backend, id string, err error, details string)
}

type noteKey struct{}

// WithNote attaches a human note (for example "moved to done") to the audit
// entries of the writes made with the returned context.
func WithNote(ctx context.Context, note string) context.Context {
	return context.WithValue(ctx, noteKey{}, note)
}

// SetAuditor makes the Set report its writes to a; nil turns auditing off.
func (s *Set) SetAuditor(a Auditor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = a
}

// auditor is the current Auditor (nil = none); the field is guarded because
// writes made by goroutines (hooks, the board's commands) may race with SetAuditor.
func (s *Set) auditor() Auditor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audit
}

// Write operations passed to a WriteListener (the same words as the audit trail).
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpDelete = "delete"
)

// WriteEvent describes one finished write to a WriteListener. Old is the task before
// the write (HasOld false when unknown or a create); New is the task after it,
// or the task as it was asked for when Err is set. A delete only has Old.
type WriteEvent struct {
	Op     string
	Old    task.Task
	HasOld bool
	New    task.Task
	Err    error
	Source string // see WithSource
}

// WriteListener is called once per write through the Set, after it ended (success or
// failure), on the writing goroutine: it must return quickly (start work, do
// not wait for it). Unlike the Auditor it receives whole tasks.
type WriteListener func(WriteEvent)

// SetWriteListener makes the Set report its writes to h; nil turns it off. With needOld
// an update whose caller did not pass the previous task (WithOld) looks it up
// first, which lists the backend once more.
func (s *Set) SetWriteListener(h WriteListener, needOld bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listener, s.needOld = h, needOld && h != nil
	s.written = nil
	if h != nil {
		s.written = map[string]bool{}
	}
}

// TakeWritten returns the tasks (as Key(backend, id)) the Set wrote since the
// last call, so a reload can tell its own writes from outside changes. The
// value says whether the task exists after the last write (false: deleted), so
// an outside change on top of an own write is still seen. Empty unless a listener
// is set.
func (s *Set) TakeWritten() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.written
	if w != nil {
		s.written = map[string]bool{}
	}
	return w
}

// Key identifies a task across backends.
func Key(backend, id string) string { return backend + "\x00" + id }

type oldKey struct{}
type sourceKey struct{}

// WithOld tells the Set the task as it was before an Update, so it does not
// have to list the backend to find out.
func WithOld(ctx context.Context, t task.Task) context.Context {
	return context.WithValue(ctx, oldKey{}, t)
}

// WithSource names where a write comes from ("undo" for the board's undo); it
// becomes the source of the write's event. Without it the listener's owner
// supplies its default ("tui" or "cli").
func WithSource(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, sourceKey{}, src)
}

// SourceOf is the source set by WithSource ("" if none).
func SourceOf(ctx context.Context) string {
	src, _ := ctx.Value(sourceKey{}).(string)
	return src
}

// notify calls the listener and remembers the write as the Set's own.
func (s *Set) notify(ctx context.Context, ev WriteEvent) {
	s.mu.Lock()
	h := s.listener
	if h != nil && ev.Err == nil {
		t := ev.New
		if ev.Op == OpDelete {
			t = ev.Old
		}
		s.written[Key(t.Backend, t.ID)] = ev.Op != OpDelete
	}
	s.mu.Unlock()
	if h == nil {
		return
	}
	ev.Source = SourceOf(ctx)
	h(ev)
}

// previous finds the task as stored now; ok false when unknown.
func (s *Set) previous(ctx context.Context, name, id string) (task.Task, bool) {
	if old, ok := ctx.Value(oldKey{}).(task.Task); ok {
		return old, true
	}
	s.mu.Lock()
	need := s.needOld
	s.mu.Unlock()
	if !need {
		return task.Task{}, false
	}
	if name == "" && len(s.items) > 0 {
		name = s.items[0].Name
	}
	for _, it := range s.items {
		if !strings.EqualFold(it.Name, name) {
			continue
		}
		ts, err := s.listBackend(ctx, it)
		if err != nil {
			return task.Task{}, false
		}
		for _, t := range ts {
			if t.ID == id {
				t.Backend = it.Name
				return t, true
			}
		}
	}
	return task.Task{}, false
}

// Audit records an action done on top of the Set (the board's reloads); it
// does nothing without an auditor.
func (s *Set) Audit(op, backend, id string, err error, details string) {
	if a := s.auditor(); a != nil {
		a.Audit(op, backend, id, err, details)
	}
}

// auditWrite records a write with the task's title and the context note.
func (s *Set) auditWrite(ctx context.Context, op, backend string, t task.Task, err error) {
	a := s.auditor()
	if a == nil {
		return
	}
	if backend == "" && len(s.items) > 0 {
		backend = s.items[0].Name
	} else if c, ok := s.canonical(backend); ok {
		backend = c
	}
	var parts []string
	if note, _ := ctx.Value(noteKey{}).(string); note != "" {
		parts = append(parts, note)
	}
	if t.Title != "" {
		parts = append(parts, fmt.Sprintf("%q", t.Title))
	}
	if op != "delete" && t.Status != "" {
		parts = append(parts, "status="+t.Status)
	}
	a.Audit(op, backend, t.ID, err, strings.Join(parts, " "))
}

// Update stores t in the backend named by t.Backend.
func (s *Set) Update(ctx context.Context, t task.Task) (err error) {
	var old task.Task
	var hasOld bool
	defer func() {
		s.auditWrite(ctx, "update", t.Backend, t, err)
		if c, ok := s.canonical(t.Backend); ok {
			t.Backend = c
		}
		s.notify(ctx, WriteEvent{Op: OpUpdate, Old: old, HasOld: hasOld, New: t, Err: err})
	}()
	// One rule for every backend, as for Create.
	if err := t.Validate(); err != nil {
		return err
	}
	b, err := s.route(t.Backend)
	if err != nil {
		return err
	}
	old, hasOld = s.previous(ctx, t.Backend, t.ID)
	defer s.invalidate()
	if _, _, guarded := Expected(ctx); guarded {
		if !guardsAtomically(b) {
			// The backend cannot check the guard under a lock of its own (custom
			// without "guard":true): check against a fresh read. Best effort, not
			// atomic with the write.
			cur, err := b.List(ctx)
			var le *LoadError
			if err != nil && !errors.As(err, &le) {
				return err
			}
			found := false
			for _, c := range cur {
				if c.ID == t.ID {
					if err := CheckExpect(ctx, c); err != nil {
						return err
					}
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: %s", ErrNotFound, t.ID)
			}
		}
	}
	// With the task as the caller read it, write only what it changed, so an edit
	// made meanwhile by an agent or another board to another field is not undone.
	if p, ok := b.(Patcher); ok && hasOld {
		return p.Patch(ctx, old, t)
	}
	return b.Update(ctx, t)
}

// Create stores t in the backend named by t.Backend (default: the first one).
func (s *Set) Create(ctx context.Context, t task.Task) (res task.Task, err error) {
	defer func() {
		at := t
		if err == nil {
			at = res
		}
		s.auditWrite(ctx, "create", at.Backend, at, err)
		if err != nil && at.Backend == "" && len(s.items) > 0 {
			at.Backend = s.items[0].Name
		}
		s.notify(ctx, WriteEvent{Op: OpCreate, New: at, Err: err})
	}()
	// One rule for every backend (custom ones included); csv checks it again.
	if err := t.Validate(); err != nil {
		return task.Task{}, err
	}
	b, err := s.route(t.Backend)
	if err != nil {
		return task.Task{}, err
	}
	name := t.Backend
	if name == "" {
		name = s.items[0].Name
	}
	defer s.invalidate()
	created, err := b.Create(ctx, t)
	if err != nil {
		return created, err
	}
	created.Backend, _ = s.canonical(name)
	return created, nil
}

// Delete removes the task id from the named backend.
func (s *Set) Delete(ctx context.Context, name, id string) (err error) {
	var old task.Task
	var hasOld bool
	defer func() {
		s.auditWrite(ctx, "delete", name, task.Task{ID: id}, err)
		if !hasOld {
			old = task.Task{ID: id}
		}
		if c, ok := s.canonical(name); ok {
			old.Backend = c
		} else if name == "" && len(s.items) > 0 {
			old.Backend = s.items[0].Name
		}
		s.notify(ctx, WriteEvent{Op: OpDelete, Old: old, HasOld: hasOld, Err: err})
	}()
	b, err := s.route(name)
	if err != nil {
		return err
	}
	old, hasOld = s.previous(ctx, name, id)
	defer s.invalidate()
	return b.Delete(ctx, id)
}

// onlyLoadErrors reports whether every failure in perr is a partly loaded store.
func onlyLoadErrors(perr *PartialError) bool {
	for _, err := range perr.Errs {
		var le *LoadError
		if !errors.As(err, &le) {
			return false
		}
	}
	return true
}

// AmbiguousError is returned by Find when an ID exists in several backends.
type AmbiguousError struct {
	ID       string
	Backends []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("task %s exists in several backends (%s); use -b NAME or NAME:%s",
		e.ID, strings.Join(e.Backends, ", "), e.ID)
}

// Find locates a task. ref is "ID" or "NAME:ID"; name (from -b) restricts the
// search to one backend. ErrNotFound when absent, *AmbiguousError when the
// bare ID matches in more than one backend.
func (s *Set) Find(ctx context.Context, name, ref string) (task.Task, error) {
	id := ref
	if i := strings.Index(ref, ":"); i > 0 && name == "" {
		if n, ok := s.canonical(ref[:i]); ok {
			name, id = n, ref[i+1:]
		}
	}
	if name != "" {
		n, ok := s.canonical(name)
		if !ok {
			return task.Task{}, fmt.Errorf("unknown backend %q (configured: %s)", name, strings.Join(s.Names(), ", "))
		}
		name = n
	}
	var tasks []task.Task
	var err error
	if name != "" { // only the named backend is consulted
		var it Named
		for _, x := range s.items {
			if x.Name == name {
				it = x
			}
		}
		var le *LoadError
		if tasks, err = s.listBackend(ctx, it); err != nil && !errors.As(err, &le) {
			return task.Task{}, fmt.Errorf("backend %s: %w", name, err)
		}
		for i := range tasks {
			tasks[i].Backend = name
		}
	} else if tasks, err = s.List(ctx); err != nil {
		var perr *PartialError
		if !errors.As(err, &perr) {
			return task.Task{}, err
		}
		if !onlyLoadErrors(perr) {
			// IDs are only unique per backend: with one backend unavailable the same
			// ID may exist there, so a bare ID cannot be resolved safely (acting on
			// the healthy match could modify the wrong task).
			return task.Task{}, fmt.Errorf("cannot resolve %q while backends are unavailable (%w); use -b NAME or NAME:ID", ref, err)
		}
		// Only some files of a backend did not load: every backend answered.
	}
	var hits []task.Task
	for _, t := range tasks {
		if t.ID == id && (name == "" || t.Backend == name) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return task.Task{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
	case 1:
		return hits[0], nil
	}
	e := &AmbiguousError{ID: id}
	for _, h := range hits {
		e.Backends = append(e.Backends, h.Backend)
	}
	return task.Task{}, e
}

// Changed reports whether any watchable backend changed since its last read.
// Every watcher is polled even if one fails; the errors are joined.
func (s *Set) Changed() (bool, error) {
	changed := false
	var errs []error
	for _, it := range s.items {
		if w, ok := it.Backend.(Watcher); ok {
			c, err := w.Changed()
			if err != nil {
				errs = append(errs, fmt.Errorf("backend %s: %w", it.Name, err))
				continue
			}
			changed = changed || c
		}
	}
	return changed, errors.Join(errs...)
}

// Reload makes every backend that can serve a cached copy re-read its store
// (the board's `r` key). Every reloader runs even if one fails; errors are joined.
func (s *Set) Reload() error {
	var errs []error
	for _, it := range s.items {
		if r, ok := it.Backend.(Reloader); ok {
			if err := r.Reload(); err != nil {
				errs = append(errs, fmt.Errorf("backend %s: %w", it.Name, err))
			}
		}
	}
	s.invalidate()
	return errors.Join(errs...)
}

// Close closes every backend, returning the first error.
func (s *Set) Close() error {
	var first error
	for _, it := range s.items {
		if err := it.Backend.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// guardsAtomically reports whether b checks a WithExpect guard inside its own
// lock or transaction.
func guardsAtomically(b Backend) bool {
	if g, ok := b.(Guarder); ok { // the backend says so itself (custom: only if its command does)
		return g.Guards()
	}
	_, ok := b.(Patcher)
	return ok
}
