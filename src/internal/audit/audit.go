// Package audit records what tkan did (reloads and writes) in memory and,
// when configured, in a .jsonl or .csv file shared by every run. Entries are
// filtered with the query language of internal/query.
package audit

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tkan/internal/backend/filestore"
	"tkan/internal/query"
)

// Operations and results recorded in Entry.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpDelete = "delete"
	OpReload = "reload"
	OpRun    = "run"    // a configured action was started on a task
	OpReport = "report" // an agent reported on a task (`tkan report`)

	// OpHookPrefix starts the op of a hook run; the event follows it
	// (see HookOp), so `op:hook` finds every hook and
	// `op=hook:task.moved` the ones fired by that event.
	OpHookPrefix = "hook:"

	ResultOK      = "ok"
	ResultError   = "error"
	ResultSkipped = "skipped" // a hook event dropped by a limit (max_parallel, unique, ...)
)

// HookOp is the op of a hook run fired by event, e.g. "hook:task.moved".
func HookOp(event string) string { return OpHookPrefix + event }

// Fields are the names usable in an audit filter.
var Fields = []string{"op", "date", "time", "details", "result", "source", "backend", "id", "session"}

// SearchFields are what a plain word of a filter is matched against.
var SearchFields = []string{"details", "op", "date"}

// FieldDocs say what each of Fields is, for the filter help.
var FieldDocs = []query.FieldDoc{
	{Name: "op", Desc: "create, update, delete, reload, run, or hook:EVENT"},
	{Name: "date", Desc: "local day of the entry, YYYY-MM-DD: date=today"},
	{Name: "time", Desc: "local date and time, YYYY-MM-DD HH:MM:SS"},
	{Name: "details", Desc: "what happened: title, status, note, error text"},
	{Name: "result", Desc: "ok, error or skipped (a hook event dropped by a limit)"},
	{Name: "source", Desc: "who did it: tui, cli, mcp, or hook (started by a hook)"},
	{Name: "backend", Desc: "name of the backend the entry is about"},
	{Name: "id", Desc: "id of the task the entry is about"},
	{Name: "session", Desc: "current, previous, or the id of an older session"},
}

var seq atomic.Uint32

// lockWait is how long a CSV append waits for another process writing the same file.
const lockWait = 5 * time.Second

// maxMemory bounds the entries kept in memory for this session.
const maxMemory = 5000

// Entry is one audited action.
type Entry struct {
	Time    time.Time `json:"time"`
	Session string    `json:"session"`
	Source  string    `json:"source"` // tui | cli | mcp | hook
	Op      string    `json:"op"`
	Backend string    `json:"backend,omitempty"`
	ID      string    `json:"id,omitempty"`
	Result  string    `json:"result"`
	Details string    `json:"details,omitempty"`
}

// Log collects the entries of this run (a "session") and, with a file, reads
// the ones earlier runs left there.
type Log struct {
	mu      sync.Mutex
	path    string // "" = memory only
	maxSize int64  // bytes; the file is rotated to path+".1" past it (0 = never)
	csv     bool
	session string
	source  string
	own     []Entry // this session, oldest first
	others  []Entry // other sessions, from the file
	stamp   [2]int64
	loaded  bool
	werr    error // first write failure, reported once through Err
}

// New returns a log writing to path ("" keeps everything in memory). The
// format follows the extension: .jsonl or .csv.
func New(path string) (*Log, error) {
	l := &Log{path: path, source: "cli"}
	// The counter keeps sessions of one process apart (tests, embedding).
	l.session = fmt.Sprintf("%s-%d-%04d", time.Now().UTC().Format("20060102T150405"), os.Getpid(), seq.Add(1))
	if path == "" {
		return l, nil
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl":
	case ".csv":
		l.csv = true
	default:
		return nil, fmt.Errorf("audit.file %q: unsupported extension (use .jsonl or .csv)", path)
	}
	return l, nil
}

// SetSource names the front end that records ("tui" or "cli").
func (l *Log) SetSource(s string) {
	l.mu.Lock()
	l.source = s
	l.mu.Unlock()
}

// SetMaxSize makes the file rotate: an append that finds it larger than kib
// KiB first renames it to path+".1" (replacing the previous one), so the trail
// takes at most about twice that. 0 or less never rotates.
func (l *Log) SetMaxSize(kib int) {
	l.mu.Lock()
	l.maxSize = int64(kib) << 10
	if kib <= 0 {
		l.maxSize = 0
	}
	l.mu.Unlock()
}

// Session is the id of this run.
func (l *Log) Session() string { return l.session }

// Path is the file entries are persisted to ("" = memory only).
func (l *Log) Path() string { return l.path }

// Record stamps and stores e. A failure to write the file is kept for Err; the
// entry stays in memory.
func (l *Log) Record(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Session, e.Source = l.session, l.source
	if e.Result == "" {
		e.Result = ResultOK
	}
	l.own = append(l.own, e)
	if len(l.own) > maxMemory {
		l.own = l.own[len(l.own)-maxMemory:]
	}
	if l.path == "" {
		return
	}
	if err := l.appendFile(e); err != nil && l.werr == nil {
		l.werr = err
	}
}

// Audit implements backend.Auditor.
func (l *Log) Audit(op, backend, id string, err error, details string) {
	e := Entry{Op: op, Backend: backend, ID: id, Details: details}
	if err != nil {
		e.Result = ResultError
		if e.Details != "" {
			e.Details += ": "
		}
		e.Details += err.Error()
	}
	l.Record(e)
}

// Err returns the first file write failure since the last call, then clears it.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.werr
	l.werr = nil
	return err
}

func (l *Log) appendFile(e Entry) error {
	var buf bytes.Buffer
	// The lock decides the CSV header from the file size (two processes starting on
	// an empty file must not both look before either has written) and keeps a
	// rotation from racing with another process's append.
	if l.csv || l.maxSize > 0 {
		unlock, err := filestore.LockFile(l.path+".lock", lockWait)
		if err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		defer unlock()
		if st, err := os.Stat(l.path); err == nil && l.maxSize > 0 && st.Size() > l.maxSize {
			if err := os.Rename(l.path, l.path+".1"); err != nil {
				return fmt.Errorf("audit log: rotate: %w", err)
			}
		}
	}
	if l.csv {
		if st, err := os.Stat(l.path); err != nil || st.Size() == 0 {
			buf.WriteString("time,session,source,op,backend,id,result,details\n")
		}
		w := csv.NewWriter(&buf)
		_ = w.Write([]string{e.Time.Format(time.RFC3339), e.Session, e.Source, e.Op, e.Backend, e.ID, e.Result, e.Details})
		w.Flush()
	} else {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	// One write call, so concurrent runs do not interleave inside a line.
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return fmt.Errorf("audit log: %w", err)
	}
	return f.Close()
}

// Entries returns every known entry, oldest first: the other sessions found
// in the file (re-read when the file changed, so runs started meanwhile show
// up) and this session's own.
func (l *Log) Entries() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if l.path != "" {
		err = l.refresh()
	}
	out := make([]Entry, 0, len(l.others)+len(l.own))
	out = append(out, l.others...)
	out = append(out, l.own...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, err
}

func (l *Log) refresh() error {
	st, err := os.Stat(l.path)
	if os.IsNotExist(err) {
		l.others, l.loaded = nil, true
		return nil
	}
	if err != nil {
		return err
	}
	stamp := [2]int64{st.ModTime().UnixNano(), st.Size()}
	if l.loaded && stamp == l.stamp {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return err
	}
	l.others = l.others[:0:0]
	for _, e := range parse(data, l.csv) {
		if e.Session != l.session {
			l.others = append(l.others, e)
		}
	}
	l.stamp, l.loaded = stamp, true
	return nil
}

// parse reads a log file, skipping lines it does not understand.
func parse(data []byte, isCSV bool) []Entry {
	var out []Entry
	if isCSV {
		r := csv.NewReader(bytes.NewReader(data))
		r.FieldsPerRecord = -1
		for {
			rec, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil || len(rec) < 8 {
				continue
			}
			t, err := time.Parse(time.RFC3339, rec[0])
			if err != nil { // the header, or a damaged row
				continue
			}
			out = append(out, Entry{Time: t, Session: rec[1], Source: rec[2], Op: rec[3], Backend: rec[4], ID: rec[5], Result: rec[6], Details: rec[7]})
		}
		return out
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil || e.Time.IsZero() {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Sessions describes how the sessions in entries relate to this run.
type Sessions struct {
	current, previous string
}

// SessionsOf finds this session and the previous one: the latest other
// session that started before this one.
func (l *Log) SessionsOf(entries []Entry) Sessions {
	s := Sessions{current: l.session}
	for _, e := range entries {
		if e.Session < l.session && e.Session > s.previous { // ids start with their UTC start time
			s.previous = e.Session
		}
	}
	return s
}

// Kind is "current", "previous" or the session id itself.
func (s Sessions) Kind(session string) string {
	switch session {
	case s.current:
		return "current"
	case s.previous:
		return "previous"
	}
	return session
}

// Label is the heading shown for a session.
func (s Sessions) Label(session string) string {
	switch s.Kind(session) {
	case "current":
		return "this session"
	case "previous":
		return "previous session"
	}
	if t, err := time.Parse("20060102T150405", strings.SplitN(session, "-", 2)[0]); err == nil {
		return "session " + t.Local().Format("2006-01-02 15:04")
	}
	return "session " + session
}

// Lookup exposes an entry's fields to the query matcher. date is the local
// calendar day (so date>=2026-10-01 and date=today work), time the full stamp.
func (s Sessions) Lookup(e Entry) query.Lookup {
	return func(field string) (string, bool) {
		switch field {
		case "op":
			return e.Op, true
		case "date":
			return e.Time.Local().Format("2006-01-02"), true
		case "time":
			return e.Time.Local().Format("2006-01-02 15:04:05"), true
		case "details":
			return e.Details, true
		case "result":
			return e.Result, true
		case "source":
			return e.Source, true
		case "backend":
			return e.Backend, true
		case "id":
			return e.ID, true
		case "session":
			return s.Kind(e.Session), true
		}
		return "", false
	}
}

// Parse parses and validates an audit filter.
func Parse(src string) (query.Query, error) {
	q, err := query.Parse(src)
	if err != nil {
		return q, err
	}
	return q, q.Check(Fields)
}

// Filter keeps the entries matching q (all of them when q is empty).
func (s Sessions) Filter(entries []Entry, q query.Query) []Entry {
	if q.Empty() {
		return entries
	}
	var out []Entry
	for _, e := range entries {
		if q.Match(s.Lookup(e), SearchFields) {
			out = append(out, e)
		}
	}
	return out
}
