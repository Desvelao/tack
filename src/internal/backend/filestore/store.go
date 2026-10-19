package filestore

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"tkan/internal/backend"
	"tkan/internal/task"
)

// Codec turns the content of a whole file into tasks and back.
type Codec interface {
	// Decode parses the file content; empty content means no tasks.
	Decode(data []byte) ([]task.Task, error)
	// Encode renders the tasks. prev is the content last read or written (nil
	// for none) so a codec can keep its layout, e.g. the shape of the document.
	Encode(tasks []task.Task, prev []byte) ([]byte, error)
}

// Store implements backend.Backend, backend.Watcher and backend.Reloader over
// one file read and written through a Codec. Writes re-read the file first
// (outside edits are merged), hold a cross-process lock on <file>.lock and
// replace the file atomically.
type Store struct {
	// LockTimeout overrides the default wait for the file lock (tests shorten it).
	LockTimeout time.Duration

	mu        sync.Mutex
	typ       string
	path      string
	codec     Codec
	poll      backend.PollMode
	tasks     []task.Task
	raw       []byte
	stamp     Stamp
	lastCheck time.Time // poll every N seconds: when the file was last examined
	stale     bool      // poll none/N: a change was seen, the next List re-reads
	now       func() time.Time
}

// NewStore loads path, which must already exist. typ names the backend in errors.
func NewStore(typ, path string, poll backend.PollMode, codec Codec) (*Store, error) {
	s := &Store{typ: typ, path: path, codec: codec, poll: poll, now: time.Now}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetClock replaces the time source used by poll: N (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) Close() error { return nil }

func (s *Store) List(_ context.Context) ([]task.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// With poll: none / N the copy in memory is served until Changed saw a
	// change or Reload was asked for.
	if s.poll.Kind == backend.PollWatch || s.stale {
		if err := s.refresh(); err != nil {
			return nil, err
		}
		s.stale = false
	}
	return clone(s.tasks), nil
}

// Statuses returns the distinct raw status values in order of first appearance.
func (s *Store) Statuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, t := range s.tasks {
		if !seen[t.Status] {
			seen[t.Status] = true
			out = append(out, t.Status)
		}
	}
	return out
}

func (s *Store) Update(ctx context.Context, t task.Task) error {
	return s.store(ctx, t, func(cur task.Task) task.Task { return KeepExtras(t, cur) })
}

// Patch writes only what differs between base and t (see backend.Patcher).
func (s *Store) Patch(ctx context.Context, base, t task.Task) error {
	return s.store(ctx, t, func(cur task.Task) task.Task { return task.Merge(cur, base, t) })
}

// store replaces the task t.ID with what merge makes of the stored one, after
// the file was read again under the lock.
func (s *Store) store(ctx context.Context, t task.Task, merge func(cur task.Task) task.Task) error {
	return s.mutate(func() error {
		for i := range s.tasks {
			if s.tasks[i].ID == t.ID {
				if err := backend.CheckExpect(ctx, s.tasks[i]); err != nil {
					return err
				}
				s.tasks[i] = merge(s.tasks[i])
				return nil
			}
		}
		return fmt.Errorf("%w: %s", backend.ErrNotFound, t.ID)
	})
}

// KeepExtras is t, plus the extra keys cur has that t does not carry (columns or
// keys the caller's copy never saw). Shared by the whole-record backends.
func KeepExtras(t, cur task.Task) task.Task {
	nt := t.Clone()
	for k, v := range cur.Extra {
		if _, ok := nt.Extra[k]; !ok {
			if nt.Extra == nil {
				nt.Extra = map[string]string{}
			}
			nt.Extra[k] = v
		}
	}
	return nt
}

// Create appends a task with ID max(numeric id, high-water mark)+1. The status is stored as given.
func (s *Store) Create(_ context.Context, t task.Task) (task.Task, error) {
	if err := t.Validate(); err != nil {
		return task.Task{}, err
	}
	var created task.Task
	err := s.mutate(func() error {
		t = t.Clone()
		t.ID = NextIDMark(s.tasks, s.path+MarkSuffix)
		s.tasks = append(s.tasks, t)
		created = t.Clone()
		return nil
	})
	return created, err
}

func (s *Store) Delete(_ context.Context, id string) error {
	return s.mutate(func() error {
		for i := range s.tasks {
			if s.tasks[i].ID == id {
				s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: %s", backend.ErrNotFound, id)
	})
}

// Changed reports whether the file differs from what was last read or written.
func (s *Store) Changed() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.poll.Kind {
	case backend.PollNone:
		return false, nil
	case backend.PollEvery:
		now := s.now()
		if !s.lastCheck.IsZero() && now.Sub(s.lastCheck) < s.poll.Interval {
			return false, nil
		}
		s.lastCheck = now
	}
	cur, err := StatFile(s.path)
	if err != nil {
		return false, err
	}
	changed := cur != s.stamp
	if changed {
		s.stale = true
	}
	return changed, nil
}

// Reload implements backend.Reloader: re-read the file now, whatever poll says.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	s.stale = false
	return nil
}

func (s *Store) mutate(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	timeout := s.LockTimeout
	if timeout <= 0 {
		timeout = LockTimeout
	}
	unlock, err := LockFile(s.path+".lock", timeout)
	if err != nil {
		return fmt.Errorf("lock %s: %w", s.path, err)
	}
	defer unlock()
	if err := s.refresh(); err != nil {
		return err
	}
	// If fn or the write fails the file is unchanged, so put the in-memory copy
	// back: otherwise List would show an unsaved change.
	snapshot := clone(s.tasks)
	if err := fn(); err != nil {
		s.tasks = snapshot
		return err
	}
	if err := s.write(); err != nil {
		s.tasks = snapshot
		return err
	}
	return nil
}

func (s *Store) refresh() error {
	cur, err := StatFile(s.path)
	if err != nil {
		return err
	}
	if cur == s.stamp {
		return nil
	}
	return s.reload()
}

// reload parses the file; a file that fails leaves the last good state alone.
func (s *Store) reload() error {
	st, err := StatFile(s.path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	tasks, err := s.codec.Decode(data)
	if err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	// Update and Delete find a task by ID, so an empty or repeated ID would
	// leave a task unreachable (or make a write hit the wrong one).
	seen := map[string]int{}
	for i, t := range tasks {
		switch prev, dup := seen[t.ID]; {
		case strings.TrimSpace(t.ID) == "":
			return fmt.Errorf("%s: task %d has an empty id; every task needs a unique id", s.path, i+1)
		case dup:
			return fmt.Errorf("%s: task %d repeats id %q of task %d; ids must be unique", s.path, i+1, t.ID, prev)
		}
		seen[t.ID] = i + 1
	}
	s.tasks, s.raw, s.stamp = tasks, data, st
	return nil
}

func (s *Store) write() error {
	data, err := s.codec.Encode(s.tasks, s.raw)
	if err != nil {
		return fmt.Errorf("%s: %w", s.typ, err)
	}
	cur, err := WriteAtomic(s.path, data)
	if err != nil {
		return err
	}
	s.raw, s.stamp = data, cur
	return nil
}

func clone(in []task.Task) []task.Task {
	out := make([]task.Task, len(in))
	for i, t := range in {
		out[i] = t.Clone()
	}
	return out
}

// NextID returns max(numeric id)+1 as a string; non-numeric ids are ignored.
func NextID(tasks []task.Task) string {
	max := 0
	for _, x := range tasks {
		if n, err := strconv.Atoi(x.ID); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1)
}

// MarkSuffix is appended to a store's path to name its high-water mark file.
const MarkSuffix = ".nextid"

// NextIDMark is NextID, but never an id the store handed out before: the
// highest id given out lives in the file at markPath, which is updated here.
// Call it under the store lock. A missing or unreadable mark degrades to
// NextID, and a mark that cannot be written is ignored (the id is still valid).
func NextIDMark(tasks []task.Task, markPath string) string {
	id := NextID(tasks)
	n, _ := strconv.Atoi(id)
	if data, err := os.ReadFile(markPath); err == nil {
		if m, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && m >= n {
			n = m + 1
			id = strconv.Itoa(n)
		}
	}
	_, _ = WriteAtomic(markPath, []byte(id+"\n"))
	return id
}

var (
	_ backend.Backend  = (*Store)(nil)
	_ backend.Watcher  = (*Store)(nil)
	_ backend.Reloader = (*Store)(nil)
)
