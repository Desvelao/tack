// Package markdownbackend stores each task in its own Markdown file inside a
// directory: the file name (without .md) is the task ID, the YAML frontmatter
// holds the fields (title, status, priority, ...; other keys are kept in
// Task.Extra) and the body is the description.
//
//	---
//	title: Fix the login
//	status: todo
//	labels: [auth, bug]
//	---
//
//	Steps to reproduce...
package markdownbackend

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/backend/filestore"
	yamlbackend "tkan/internal/backend/yaml"
	"tkan/internal/task"
)

// Registered without extension: a directory has none, so select it with
// `type: markdown` (or `tkan init -backend markdown`).
func init() { backend.RegisterType("markdown", Open) }

const ext = ".md"

type options struct {
	Source string            `yaml:"source"`
	Fields map[string]string `yaml:"fields"`
	Poll   any               `yaml:"poll"`
}

// Backend implements backend.Backend, backend.Watcher and backend.Reloader
// over a directory of Markdown files.
type Backend struct {
	mu          sync.Mutex
	dir         string
	poll        backend.PollMode
	m           *filestore.Mapping
	tasks       []task.Task
	files       map[string]filestore.Stamp // file name -> version last read or written
	lockTimeout time.Duration
	lastCheck   time.Time
	stale       bool
	now         func() time.Time
}

// Open loads the directory named by spec. It must already exist.
func Open(spec backend.Spec) (backend.Backend, error) {
	var o options
	if spec.Options.Kind != 0 {
		if err := spec.Options.Decode(&o); err != nil {
			return nil, fmt.Errorf("markdown options: %w", err)
		}
	}
	if strings.TrimSpace(o.Source) == "" {
		return nil, fmt.Errorf("markdown options: source is required (directory of the .md files)")
	}
	for f := range o.Fields {
		if f = strings.ToLower(strings.TrimSpace(f)); f == "id" || f == "description" {
			return nil, fmt.Errorf("markdown options: field %q cannot be mapped (it is the file name / the body)", f)
		}
	}
	poll, err := backend.ParsePoll("markdown", o.Poll)
	if err != nil {
		return nil, err
	}
	m, err := filestore.NewMapping("markdown", o.Fields)
	if err != nil {
		return nil, err
	}
	b := &Backend{dir: spec.Resolve(o.Source), poll: poll, m: m, now: time.Now}
	if st, err := os.Stat(b.dir); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, fmt.Errorf("markdown options: source %s is not a directory", b.dir)
	}
	if err := b.reload(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Backend) Close() error { return nil }

func (b *Backend) List(_ context.Context) ([]task.Task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.poll.Kind == backend.PollWatch || b.stale {
		if err := b.refresh(); err != nil {
			return nil, err
		}
		b.stale = false
	}
	return clone(b.tasks), nil
}

func (b *Backend) Statuses() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, t := range b.tasks {
		if !seen[t.Status] {
			seen[t.Status] = true
			out = append(out, t.Status)
		}
	}
	return out
}

func (b *Backend) Update(_ context.Context, t task.Task) error {
	return b.mutate(func() error {
		for i := range b.tasks {
			if b.tasks[i].ID != t.ID {
				continue
			}
			nt := t.Clone()
			for k, v := range b.tasks[i].Extra {
				if _, ok := nt.Extra[k]; !ok {
					if nt.Extra == nil {
						nt.Extra = map[string]string{}
					}
					nt.Extra[k] = v
				}
			}
			if err := b.writeTask(nt); err != nil {
				return err
			}
			b.tasks[i] = nt
			return nil
		}
		return fmt.Errorf("%w: %s", backend.ErrNotFound, t.ID)
	})
}

// Create writes <max numeric id + 1>.md. The status is stored as given.
func (b *Backend) Create(_ context.Context, t task.Task) (task.Task, error) {
	if err := t.Validate(); err != nil {
		return task.Task{}, err
	}
	var created task.Task
	err := b.mutate(func() error {
		t = t.Clone()
		t.ID = filestore.NextID(b.tasks)
		if _, err := os.Stat(filepath.Join(b.dir, t.ID+ext)); err == nil {
			return fmt.Errorf("%s%s already exists", t.ID, ext)
		}
		if err := b.writeTask(t); err != nil {
			return err
		}
		b.tasks = append(b.tasks, t)
		created = t.Clone()
		return nil
	})
	return created, err
}

func (b *Backend) Delete(_ context.Context, id string) error {
	return b.mutate(func() error {
		for i := range b.tasks {
			if b.tasks[i].ID != id {
				continue
			}
			name := id + ext
			if err := os.Remove(filepath.Join(b.dir, name)); err != nil {
				return err
			}
			delete(b.files, name)
			b.tasks = append(b.tasks[:i], b.tasks[i+1:]...)
			return nil
		}
		return fmt.Errorf("%w: %s", backend.ErrNotFound, id)
	})
}

// Changed reports whether the directory differs from what was last read or written.
func (b *Backend) Changed() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.poll.Kind {
	case backend.PollNone:
		return false, nil
	case backend.PollEvery:
		now := b.now()
		if !b.lastCheck.IsZero() && now.Sub(b.lastCheck) < b.poll.Interval {
			return false, nil
		}
		b.lastCheck = now
	}
	cur, err := b.stat()
	if err != nil {
		return false, err
	}
	changed := !sameFiles(cur, b.files)
	if changed {
		b.stale = true
	}
	return changed, nil
}

// Reload implements backend.Reloader.
func (b *Backend) Reload() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.reload(); err != nil {
		return err
	}
	b.stale = false
	return nil
}

func (b *Backend) mutate(fn func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	timeout := b.lockTimeout
	if timeout <= 0 {
		timeout = filestore.LockTimeout
	}
	unlock, err := filestore.LockFile(filepath.Join(b.dir, ".tkan.lock"), timeout)
	if err != nil {
		return fmt.Errorf("lock %s: %w", b.dir, err)
	}
	defer unlock()
	if err := b.refresh(); err != nil {
		return err
	}
	// A failed operation may have written some state; re-read to be exact.
	if err := fn(); err != nil {
		_ = b.reload()
		return err
	}
	return nil
}

func (b *Backend) refresh() error {
	cur, err := b.stat()
	if err != nil {
		return err
	}
	if sameFiles(cur, b.files) {
		return nil
	}
	return b.reload()
}

func sameFiles(a, b map[string]filestore.Stamp) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// stat returns the version of every task file (hidden files are not tasks).
func (b *Backend) stat() (map[string]filestore.Stamp, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	out := map[string]filestore.Stamp{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.EqualFold(filepath.Ext(name), ext) {
			continue
		}
		st, err := os.Stat(filepath.Join(b.dir, name)) // follows symlinks
		if err != nil || st.IsDir() {
			continue
		}
		out[name] = filestore.Stamp{Mod: st.ModTime(), Size: st.Size()}
	}
	return out, nil
}

// reload reads every file; if one fails the last good state stays.
func (b *Backend) reload() error {
	files, err := b.stat()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		return idLess(strings.TrimSuffix(names[i], filepath.Ext(names[i])), strings.TrimSuffix(names[j], filepath.Ext(names[j])))
	})
	b.m.Reset()
	var tasks []task.Task
	seen := map[string]string{}
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(b.dir, n))
		if err != nil {
			return err
		}
		id := strings.TrimSuffix(n, filepath.Ext(n))
		if prev, dup := seen[id]; dup {
			return fmt.Errorf("%s: %s and %s have the same id %q", b.dir, prev, n, id)
		}
		seen[id] = n
		t, err := b.parse(id, data)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(b.dir, n), err)
		}
		tasks = append(tasks, t)
	}
	b.tasks, b.files = tasks, files
	return nil
}

// idLess orders numeric ids first (by value), then the rest alphabetically.
func idLess(a, b string) bool {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		return na < nb || na == nb && a < b
	case ea == nil:
		return true
	case eb == nil:
		return false
	}
	return a < b
}

// split separates the frontmatter from the body. Files without frontmatter are
// all body.
func split(data []byte) (front, body string) {
	s := strings.ReplaceAll(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", s
	}
	rest := s[4:]
	if strings.HasPrefix(rest, "---\n") || rest == "---" { // empty frontmatter
		return "", strings.TrimPrefix(strings.TrimPrefix(rest, "---"), "\n")
	}
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return rest[:i+1], rest[i+5:]
	}
	if strings.HasSuffix(rest, "\n---") {
		return rest[:len(rest)-3], ""
	}
	return "", s // no closing line: not frontmatter
}

func (b *Backend) parse(id string, data []byte) (task.Task, error) {
	front, body := split(data)
	var t task.Task
	if strings.TrimSpace(front) != "" {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(front), &n); err != nil {
			return t, fmt.Errorf("frontmatter: %w", err)
		}
		if len(n.Content) > 0 {
			r, err := yamlbackend.MappingRecord(n.Content[0])
			if err != nil {
				return t, fmt.Errorf("frontmatter: %w", err)
			}
			t = b.m.Task(r)
		}
	}
	t.ID = id
	if t.Description == "" {
		t.Description = strings.Trim(body, "\n")
	}
	if strings.TrimSpace(t.Title) == "" {
		t.Title = id
	}
	return t, nil
}

// writeTask writes the task's file atomically and records its new version.
func (b *Backend) writeTask(t task.Task) error {
	rec := b.m.Record(t)
	kept := rec[:0]
	for _, f := range rec {
		if f.Key != "id" && f.Key != "description" { // the file name and the body
			kept = append(kept, f)
		}
	}
	node, err := yamlbackend.RecordNode(kept)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	buf.WriteString("---\n")
	if d := strings.Trim(t.Description, "\n"); d != "" {
		buf.WriteString("\n" + d + "\n")
	}
	name := t.ID + ext
	st, err := filestore.WriteAtomic(filepath.Join(b.dir, name), buf.Bytes())
	if err != nil {
		return err
	}
	b.files[name] = st
	return nil
}

func clone(in []task.Task) []task.Task {
	out := make([]task.Task, len(in))
	for i, t := range in {
		out[i] = t.Clone()
	}
	return out
}

var (
	_ backend.Watcher  = (*Backend)(nil)
	_ backend.Reloader = (*Backend)(nil)
)
