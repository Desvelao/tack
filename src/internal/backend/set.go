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

	mu      sync.Mutex
	audit   Auditor                // nil = no audit trail
	caching bool                   // see CacheLists
	cache   map[string][]task.Task // backend name -> its last successful list
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

// List returns every backend's tasks (config order, store order within each),
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
			continue
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
func (s *Set) SetAuditor(a Auditor) { s.audit = a }

// Audit records an action done on top of the Set (the board's reloads); it
// does nothing without an auditor.
func (s *Set) Audit(op, backend, id string, err error, details string) {
	if s.audit != nil {
		s.audit.Audit(op, backend, id, err, details)
	}
}

// auditWrite records a write with the task's title and the context note.
func (s *Set) auditWrite(ctx context.Context, op, backend string, t task.Task, err error) {
	if s.audit == nil {
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
	s.audit.Audit(op, backend, t.ID, err, strings.Join(parts, " "))
}

// Update stores t in the backend named by t.Backend.
func (s *Set) Update(ctx context.Context, t task.Task) (err error) {
	defer func() { s.auditWrite(ctx, "update", t.Backend, t, err) }()
	b, err := s.route(t.Backend)
	if err != nil {
		return err
	}
	defer s.invalidate()
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
	defer func() { s.auditWrite(ctx, "delete", name, task.Task{ID: id}, err) }()
	b, err := s.route(name)
	if err != nil {
		return err
	}
	defer s.invalidate()
	return b.Delete(ctx, id)
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
		if tasks, err = s.listBackend(ctx, it); err != nil {
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
		// IDs are only unique per backend: with one backend unavailable the same
		// ID may exist there, so a bare ID cannot be resolved safely (acting on
		// the healthy match could modify the wrong task).
		return task.Task{}, fmt.Errorf("cannot resolve %q while backends are unavailable (%w); use -b NAME or NAME:ID", ref, err)
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
