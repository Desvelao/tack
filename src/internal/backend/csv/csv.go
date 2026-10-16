// Package csvbackend stores tasks in a CSV file.
//
// Which source column holds each logical field (id, title, status, ...) comes
// from the `options:` of the `type: csv` entry in tkan.yaml; see docs/config.md. Without
// it the header must contain columns named like the fields. Required fields:
// id, title, status. Columns that are not mapped are kept in Task.Extra and
// written back unchanged.
package csvbackend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tkan/internal/backend"
	"tkan/internal/task"
)

func init() { backend.RegisterType("csv", Open, ".csv") }

// lockTimeout is how long a write waits for another tkan holding the file lock.
const lockTimeout = 5 * time.Second

const bom = "\xef\xbb\xbf"

var logicalFields = []string{"id", "title", "description", "status", "priority", "notes", "type", "assignee", "due", "start", "labels"}

var required = map[string]bool{"id": true, "title": true, "status": true}

// colRef points at a source column by header name or by 1-based position.
type colRef struct {
	name string
	pos  int
}

func (r colRef) String() string {
	if r.pos > 0 {
		return "position " + strconv.Itoa(r.pos)
	}
	return strconv.Quote(r.name)
}

type schema struct {
	header bool
	fields map[string]colRef // logical field -> source column
}

type yamlSchema struct {
	Source string         `yaml:"source"`
	Header *bool          `yaml:"header"`
	Fields map[string]any `yaml:"fields"`
}

func parseSchema(spec backend.Spec) (s schema, source string, err error) {
	s = schema{header: true, fields: map[string]colRef{}}
	var ys yamlSchema
	if spec.Options.Kind != 0 {
		if err := spec.Options.Decode(&ys); err != nil {
			return s, "", fmt.Errorf("csv options: %w", err)
		}
	}
	if strings.TrimSpace(ys.Source) == "" {
		return s, "", fmt.Errorf("csv options: source is required (path of the CSV file)")
	}
	source = spec.Resolve(ys.Source)
	if ys.Header != nil {
		s.header = *ys.Header
	}
	known := map[string]bool{}
	for _, f := range logicalFields {
		known[f] = true
	}
	for f, v := range ys.Fields {
		f = strings.ToLower(strings.TrimSpace(f))
		if !known[f] {
			return s, "", fmt.Errorf("csv options: unknown field %q (valid: %s)", f, strings.Join(logicalFields, ", "))
		}
		switch x := v.(type) {
		case int:
			if x < 1 {
				return s, "", fmt.Errorf("csv options: field %q: position must be >= 1", f)
			}
			s.fields[f] = colRef{pos: x}
		case string:
			x = strings.TrimSpace(x)
			if n, err := strconv.Atoi(x); err == nil && n >= 1 {
				s.fields[f] = colRef{pos: n}
			} else if x == "" {
				return s, "", fmt.Errorf("csv options: field %q: empty column name", f)
			} else {
				s.fields[f] = colRef{name: x}
			}
		default:
			return s, "", fmt.Errorf("csv options: field %q must be a column name or a position", f)
		}
	}
	return s, source, nil
}

// ref returns the configured column for a logical field, defaulting to the
// column named like the field (only meaningful when the file has a header).
func (s schema) ref(field string) colRef {
	if r, ok := s.fields[field]; ok {
		return r
	}
	return colRef{name: field}
}

type stamp struct {
	mod  time.Time
	size int64
}

// Backend implements backend.Backend (and backend.Watcher) over a CSV file.
type Backend struct {
	mu          sync.Mutex
	path        string
	sch         schema
	header      []string // nil when the file has no header row
	colField    []string // source column index -> logical field ("" = extra)
	extra       []string // source column index -> unique Extra key (for unmapped columns)
	tasks       []task.Task
	stamp       stamp
	crlf        bool          // file uses \r\n record terminators; preserved on write
	bom         bool          // file starts with a UTF-8 BOM; preserved on write
	lockTimeout time.Duration // 0 = the default (tests shorten it)
	noFinalNL   bool          // file did not end with a newline; preserved on write
}

// Open loads path. The file must already exist.
func Open(spec backend.Spec) (backend.Backend, error) {
	sch, path, err := parseSchema(spec)
	if err != nil {
		return nil, err
	}
	b := &Backend{path: path, sch: sch}
	if err := b.reload(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Backend) Close() error { return nil }

func (b *Backend) List(_ context.Context) ([]task.Task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.refresh(); err != nil {
		return nil, err
	}
	out := make([]task.Task, len(b.tasks))
	for i, t := range b.tasks {
		out[i] = t.Clone()
	}
	return out, nil
}

// Statuses returns the distinct raw status values in order of first appearance.
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
			if b.tasks[i].ID == t.ID {
				// Keep unmapped columns the caller's copy doesn't carry.
				nt := t.Clone()
				for k, v := range b.tasks[i].Extra {
					if _, ok := nt.Extra[k]; !ok {
						if nt.Extra == nil {
							nt.Extra = map[string]string{}
						}
						nt.Extra[k] = v
					}
				}
				b.tasks[i] = nt
				return nil
			}
		}
		return fmt.Errorf("%w: %s", backend.ErrNotFound, t.ID)
	})
}

// Create appends a task with ID max(numeric id)+1. The status is stored as given.
func (b *Backend) Create(_ context.Context, t task.Task) (task.Task, error) {
	if err := t.Validate(); err != nil {
		return task.Task{}, err
	}
	var created task.Task
	err := b.mutate(func() error {
		max := 0
		for _, x := range b.tasks {
			if n, err := strconv.Atoi(x.ID); err == nil && n > max {
				max = n
			}
		}
		t = t.Clone()
		t.ID = strconv.Itoa(max + 1)
		b.tasks = append(b.tasks, t)
		created = t.Clone()
		return nil
	})
	return created, err
}

func (b *Backend) Delete(_ context.Context, id string) error {
	return b.mutate(func() error {
		for i := range b.tasks {
			if b.tasks[i].ID == id {
				b.tasks = append(b.tasks[:i], b.tasks[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: %s", backend.ErrNotFound, id)
	})
}

// Changed reports whether the file differs from what was last read or written.
func (b *Backend) Changed() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, err := statFile(b.path)
	if err != nil {
		return false, err
	}
	return cur != b.stamp, nil
}

// mutate re-reads the file if it changed on disk (so edits made by another
// process are merged, not clobbered), applies fn, then writes atomically.
func (b *Backend) mutate(fn func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Serialise read-modify-write across processes (the TUI plus scripted calls).
	timeout := b.lockTimeout
	if timeout <= 0 {
		timeout = lockTimeout
	}
	unlock, err := lockFile(b.path+".lock", timeout)
	if err != nil {
		return fmt.Errorf("lock %s: %w", b.path, err)
	}
	defer unlock()
	if err := b.refresh(); err != nil {
		return err
	}
	// fn edits b.tasks in place. If it or the write fails the file is unchanged,
	// so put the in-memory copy back: otherwise List would show an unsaved change
	// (the stamp still matches the file) and a later write would persist it.
	snapshot := make([]task.Task, len(b.tasks))
	for i, t := range b.tasks {
		snapshot[i] = t.Clone()
	}
	if err := fn(); err != nil {
		b.tasks = snapshot
		return err
	}
	if err := b.write(); err != nil {
		b.tasks = snapshot
		return err
	}
	return nil
}

func (b *Backend) refresh() error {
	cur, err := statFile(b.path)
	if err != nil {
		return err
	}
	if cur == b.stamp {
		return nil
	}
	return b.reload()
}

func statFile(path string) (stamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		return stamp{}, err
	}
	return stamp{st.ModTime(), st.Size()}, nil
}

func field(t *task.Task, name string) *string {
	switch name {
	case "id":
		return &t.ID
	case "title":
		return &t.Title
	case "description":
		return &t.Description
	case "status":
		return &t.Status
	case "priority":
		return &t.Priority
	case "notes":
		return &t.Notes
	case "type":
		return &t.Type
	case "assignee":
		return &t.Assignee
	case "due":
		return &t.Due
	case "start":
		return &t.Start
	case "labels":
		return &t.Labels
	}
	return nil
}

// extraKeys names every column for Task.Extra: its header, or colN when there
// is none. Repeated names get a "#2", "#3"... suffix so no two columns share a
// key (and a blank header's colN can't clash with a real header of that name).
func extraKeys(header []string, ncols int) []string {
	keys := make([]string, ncols)
	used := map[string]bool{}
	for i := range keys {
		base := "col" + strconv.Itoa(i+1)
		if i < len(header) && header[i] != "" {
			base = header[i]
		}
		key := base
		for n := 2; used[key]; n++ {
			key = base + "#" + strconv.Itoa(n)
		}
		used[key] = true
		keys[i] = key
	}
	return keys
}

// resolve builds colField from the schema and the (possibly nil) header.
func (b *Backend) resolve(ncols int) error {
	colField := make([]string, ncols)
	for _, f := range logicalFields {
		ref, configured := b.sch.fields[f]
		idx := -1
		switch {
		case configured && ref.pos > 0:
			idx = ref.pos - 1
		case b.header != nil:
			ref = b.sch.ref(f)
			for i, h := range b.header {
				if strings.EqualFold(strings.TrimSpace(h), ref.name) {
					idx = i
					break
				}
			}
		}
		if idx < 0 || idx >= ncols {
			if required[f] {
				if b.header == nil {
					return fmt.Errorf("%s: headerless csv needs options.fields.%s set to a column position", b.path, f)
				}
				return fmt.Errorf("%s: missing required column %s (field %q)", b.path, b.sch.ref(f), f)
			}
			continue
		}
		if prev := colField[idx]; prev != "" {
			return fmt.Errorf("%s: column %d is mapped to both %q and %q", b.path, idx+1, prev, f)
		}
		colField[idx] = f
	}
	b.colField = colField
	return nil
}

func (b *Backend) reload() (err error) {
	// A file that fails validation must leave the last good state untouched
	// (header and column maps are replaced before the rows are checked).
	oldHeader, oldCols, oldExtra := b.header, b.colField, b.extra
	defer func() {
		if err != nil {
			b.header, b.colField, b.extra = oldHeader, oldCols, oldExtra
		}
	}()
	st, err := os.Stat(b.path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(b.path)
	if err != nil {
		return err
	}
	if i := bytes.IndexByte(data, '\n'); i > 0 {
		b.crlf = data[i-1] == '\r'
	}
	b.bom = bytes.HasPrefix(data, []byte(bom))
	b.noFinalNL = len(data) > 0 && data[len(data)-1] != '\n'

	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	var recs [][]string
	var recLine []int // physical line each record starts on (blank lines are skipped by the reader)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", b.path, err)
		}
		line, _ := r.FieldPos(0)
		recs, recLine = append(recs, rec), append(recLine, line)
	}

	var header []string
	body := recs
	if b.sch.header {
		if len(recs) == 0 {
			// Empty file: start with a header made of the configured names.
			for _, f := range logicalFields[:6] {
				name := b.sch.ref(f).name
				if name == "" {
					name = f
				}
				header = append(header, name)
			}
			body = nil
		} else {
			header = append([]string(nil), recs[0]...)
			header[0] = strings.TrimPrefix(header[0], bom)
			body = recs[1:]
		}
	} else if len(recs) > 0 && len(recs[0]) > 0 {
		recs[0][0] = strings.TrimPrefix(recs[0][0], bom)
	}

	ncols := len(header)
	// Rows wider than the header keep their extra cells (written back as
	// columns without a header name).
	for _, row := range body {
		if len(row) > ncols {
			ncols = len(row)
		}
	}
	if b.sch.header {
		for len(header) < ncols {
			header = append(header, "")
		}
	} else {
		for _, ref := range b.sch.fields {
			if ref.pos > ncols {
				ncols = ref.pos
			}
		}
	}
	b.header = header
	if err := b.resolve(ncols); err != nil {
		return err
	}
	b.extra = extraKeys(header, ncols)

	tasks := make([]task.Task, 0, len(body))
	firstRec := 0 // index into recLine of body[0]
	if b.sch.header && len(recs) > 0 {
		firstRec = 1
	}
	lineOf := map[string]int{} // id -> line, to name both lines of a duplicate
	for n, row := range body {
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue
		}
		var t task.Task
		for i, v := range row {
			if i < ncols && b.colField[i] != "" {
				*field(&t, b.colField[i]) = v
				continue
			}
			if t.Extra == nil {
				t.Extra = map[string]string{}
			}
			t.Extra[b.extra[i]] = v
		}
		// Update and Delete find a task by ID, so an empty or repeated ID would
		// leave a row unreachable (or make a write hit the wrong one).
		line := 0
		if firstRec+n < len(recLine) {
			line = recLine[firstRec+n]
		}
		switch prev, dup := lineOf[t.ID]; {
		case strings.TrimSpace(t.ID) == "":
			return fmt.Errorf("%s: line %d has an empty id; every row needs a unique id", b.path, line)
		case dup:
			return fmt.Errorf("%s: line %d repeats id %q of line %d; ids must be unique", b.path, line, t.ID, prev)
		}
		lineOf[t.ID] = line
		tasks = append(tasks, t)
	}
	b.tasks = tasks
	b.stamp = stamp{st.ModTime(), st.Size()}
	return nil
}

// write rewrites the whole file through a temp file + rename in the same directory.
func (b *Backend) write() error {
	header := append([]string(nil), b.header...)
	colField := append([]string(nil), b.colField...)
	if b.sch.header {
		// Optional fields that were not in the file but now have values get a new column.
		mapped := map[string]bool{}
		for _, f := range colField {
			mapped[f] = true
		}
		for _, f := range logicalFields {
			if mapped[f] {
				continue
			}
			for i := range b.tasks {
				if *field(&b.tasks[i], f) != "" {
					name := b.sch.ref(f).name
					if name == "" {
						name = f
					}
					header = append(header, name)
					colField = append(colField, f)
					break
				}
			}
		}
	}
	ncols := len(colField)

	// Write through a symlink instead of replacing it with a regular file.
	target := b.path
	if real, err := filepath.EvalSymlinks(b.path); err == nil {
		target = real
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".tkan-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }

	var content bytes.Buffer
	bw := bufio.NewWriter(&content)
	var rec bytes.Buffer
	emit := func(row []string) error {
		rec.Reset()
		cw := csv.NewWriter(&rec)
		if err := cw.Write(row); err != nil {
			return err
		}
		cw.Flush()
		line := rec.Bytes() // ends with "\n"; newlines inside fields stay as-is
		if b.crlf {
			line = append(line[:len(line)-1], '\r', '\n')
		}
		_, err := bw.Write(line)
		return err
	}
	if b.sch.header {
		if err := emit(header); err != nil {
			cleanup()
			return err
		}
	}
	for i := range b.tasks {
		t := &b.tasks[i]
		row := make([]string, ncols)
		for j := 0; j < ncols; j++ {
			if f := colField[j]; f != "" {
				row[j] = *field(t, f)
			} else {
				row[j] = t.Extra[b.extra[j]]
			}
		}
		if err := emit(row); err != nil {
			cleanup()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return err
	}
	data := content.Bytes()
	if b.noFinalNL { // keep a file that had no trailing newline that way
		data = bytes.TrimSuffix(bytes.TrimSuffix(data, []byte("\n")), []byte("\r"))
	}
	if b.bom {
		data = append([]byte(bom), data...)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	// Flush to disk before the rename so a power loss can't leave an empty file.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if st, err := os.Stat(target); err == nil {
		_ = os.Chmod(tmpName, st.Mode().Perm())
	}
	// Stamp the temp file (rename keeps its mtime and size), so an external
	// write landing after the rename differs from the stamp and is noticed.
	cur, err := statFile(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	if b.sch.header {
		b.header = header
	}
	b.colField = colField
	b.stamp = cur
	return nil
}

// syncDir makes the rename durable; best effort, some filesystems don't support it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

var _ backend.Watcher = (*Backend)(nil)
