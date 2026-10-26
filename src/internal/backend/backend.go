// Package backend defines how tack talks to a task store, a registry that
// selects the implementation by type name, and Set, which combines several
// configured backends into one.
package backend

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"tack/internal/task"
)

var (
	ErrNotFound    = errors.New("task not found")
	ErrUnsupported = errors.New("operation not supported by this backend")
	// ErrPrecondition: the guard given with WithExpect did not hold when the
	// write was about to be applied (somebody else changed the field first).
	ErrPrecondition = errors.New("precondition failed")
)

type expectKey struct{}

type expectation struct{ field, value string }

// WithExpect makes an Update (or Patch) conditional: it is applied only if the
// stored task still has value in the logical field (status, assignee, ...),
// else it fails with ErrPrecondition and changes nothing. Backends check it
// inside their own lock or transaction, so of several writers racing on the
// same expectation exactly one wins (a custom command only if it reports
// "guard":true).
func WithExpect(ctx context.Context, field, value string) context.Context {
	return context.WithValue(ctx, expectKey{}, expectation{field, value})
}

// WithoutExpect drops the guard of WithExpect, for a wrapper that has checked
// it itself on data the wrapped backend does not hold.
func WithoutExpect(ctx context.Context) context.Context {
	return context.WithValue(ctx, expectKey{}, nil)
}

// Expected returns the guard set by WithExpect.
func Expected(ctx context.Context) (field, value string, ok bool) {
	e, ok := ctx.Value(expectKey{}).(expectation)
	return e.field, e.value, ok
}

// CheckExpect is nil without a guard or when cur satisfies it, else an error
// wrapping ErrPrecondition. cur is the task as the store holds it right now.
func CheckExpect(ctx context.Context, cur task.Task) error {
	field, want, ok := Expected(ctx)
	if !ok {
		return nil
	}
	p := cur.FieldPtr(field)
	if p == nil {
		return fmt.Errorf("%w: unknown field %q", ErrPrecondition, field)
	}
	if *p != want {
		return fmt.Errorf("%w: %s is %q, expected %q", ErrPrecondition, field, *p, want)
	}
	return nil
}

// Backend is the only thing the TUI and CLI depend on.
type Backend interface {
	// List returns every task in store order.
	List(ctx context.Context) ([]task.Task, error)
	// Statuses returns the raw status values currently present in the store,
	// in order of first appearance. The board package maps them to columns.
	Statuses() []string
	// Update replaces the task with the same ID.
	Update(ctx context.Context, t task.Task) error
	// Create stores a new task and returns it with its assigned ID.
	Create(ctx context.Context, t task.Task) (task.Task, error)
	// Delete removes a task.
	Delete(ctx context.Context, id string) error
	// Close releases resources.
	Close() error
}

// Watcher is optionally implemented by backends that can cheaply tell whether
// the underlying store changed since the last read (enables live refresh).
type Watcher interface {
	Changed() (bool, error)
}

// LoadError is returned by List together with the tasks that did load, when
// part of the store could not be read (one malformed file of many). Callers
// that can use a partial list keep the tasks and show the error; the writes of
// the healthy part keep working.
type LoadError struct{ Errs []error }

func (e *LoadError) Error() string {
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

// Unwrap exposes the individual errors to errors.Is / errors.As.
func (e *LoadError) Unwrap() []error { return e.Errs }

// Patcher is optionally implemented by backends that can write only what an
// edit changed. Patch stores t over the task as the store holds it now, but
// only the fields (and extra keys) that differ between base, the task as the
// caller read it, and t. A change someone else made to another field in the
// meantime survives, where Update would write the caller's old copy over it.
// Patch and Update honour a guard set with WithExpect inside the store's lock.
type Patcher interface {
	Patch(ctx context.Context, base, t task.Task) error
}

// Guarder is optionally implemented by backends that may or may not check a
// WithExpect guard atomically (a custom command: only if it said so). Set.Update
// asks it instead of assuming, and skips its best-effort read-and-check when true.
type Guarder interface {
	Guards() bool
}

// FieldStorer is optionally implemented by backends whose store has a fixed
// set of columns (csv): Stores reports whether a value written to the
// logical field is kept. Backends without it store every field.
type FieldStorer interface {
	Stores(field string) bool
}

// Reloader is optionally implemented by backends that may serve a cached copy
// (see the csv `poll` option); Reload re-reads the store now.
type Reloader interface {
	Reload() error
}

// Spec is one entry of the config's `backend:` list, handed to a Factory.
type Spec struct {
	Type    string    // backend type, lower case ("csv", "custom")
	Name    string    // unique name of this entry (given or generated)
	Options yaml.Node // the entry's `options:` block; Kind == 0 when absent
	Command []string  // the entry's `command:` (custom backends); nil when absent
	BaseDir string    // directory of the config file; relative paths resolve against it
	Lazy    bool      // the caller loads in the background: do not list while opening
}

// Resolve returns p made absolute against the config directory.
func (s Spec) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) || strings.Contains(p, "://") {
		return p
	}
	return filepath.Join(s.BaseDir, p)
}

// Factory opens a backend from its config entry.
type Factory func(s Spec) (Backend, error)

type registration struct {
	factory Factory
	exts    []string
}

var registry = map[string]registration{}

// RegisterType binds a backend type name ("csv") to a factory. exts are the
// lowercase file extensions or URL schemes (".csv", "gh://") `tack init`
// uses to guess the type of a source. Call it from the backend's init().
func RegisterType(typ string, f Factory, exts ...string) {
	typ = strings.ToLower(typ)
	if _, dup := registry[typ]; dup {
		panic("backend: duplicate registration for " + typ)
	}
	registry[typ] = registration{factory: f, exts: exts}
}

// TypeForLocation guesses the backend type of a source from its extension or
// scheme; "" when none matches.
func TypeForLocation(location string) string {
	key := strings.ToLower(filepath.Ext(location))
	if i := strings.Index(location, "://"); i > 0 {
		key = strings.ToLower(location[:i+3])
	}
	for typ, r := range registry {
		for _, e := range r.exts {
			if e == key {
				return typ
			}
		}
	}
	return ""
}

// OpenSpec opens the backend described by a config entry.
func OpenSpec(s Spec) (Backend, error) {
	r, ok := registry[strings.ToLower(s.Type)]
	if !ok {
		return nil, fmt.Errorf("unknown backend type %q (supported: %s)", s.Type, strings.Join(Supported(), ", "))
	}
	return r.factory(s)
}

// Supported lists registered backend types, sorted.
func Supported() []string {
	keys := make([]string, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Example is what a backend contributes to a generated sample configuration.
type Example struct {
	// Entry is YAML text for keys of the backend entry itself, written before
	// `options:` (the custom backend's `command:`); empty for most backends. When
	// set, the entry starts with it instead of `type:`.
	Entry string
	// Section is the YAML text of the entry's `options:` body (for example
	// "source: {{source}}\nheader: true\nfields: ..."), unindented and without a
	// trailing blank line. The token {{source}} is replaced by the (quoted)
	// source path, relative to the generated file.
	Section string
	// Statuses are the raw status values observed in the data, in order of
	// first appearance. Empty when the data does not exist or has none.
	Statuses []string
	// Extras are source columns that map to no field; the generated config
	// mentions them as extra filterable fields.
	Extras []string
	// Notes are human-readable remarks about what was or was not detected.
	Notes []string
}

// ExampleFunc inspects location (which may not exist yet) and describes a
// configuration suited to it.
type ExampleFunc func(location string) (Example, error)

var examples = map[string]ExampleFunc{}

// RegisterExample binds an example generator to a backend name ("csv").
// Every backend should register one; `tack init` uses it.
func RegisterExample(name string, f ExampleFunc) {
	name = strings.ToLower(name)
	if _, dup := examples[name]; dup {
		panic("backend: duplicate example for " + name)
	}
	examples[name] = f
}

// ExampleFor returns the example for the named backend.
func ExampleFor(name, location string) (Example, error) {
	f, ok := examples[strings.ToLower(name)]
	if !ok {
		names := make([]string, 0, len(examples))
		for n := range examples {
			names = append(names, n)
		}
		sort.Strings(names)
		return Example{}, fmt.Errorf("no sample configuration for backend %q (supported: %s)", name, strings.Join(names, ", "))
	}
	return f(location)
}
