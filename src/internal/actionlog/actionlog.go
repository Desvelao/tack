// Package actionlog is the optional, detailed record of action runs: what was
// run (command, stdin), when, for how long and what it printed. The audit
// trail only keeps a one-line summary of each run (Record.Summary); this log
// keeps the rest. It is disabled unless the config sets `action_log:`, and
// never stores environment variable values (they may hold secrets).
//
// Two layouts: `file` appends one JSON object per run to a .jsonl file;
// `dir` writes one JSON file per run, in a sub-directory per action.
// Files are private (0600/0700): prompts and command output can be sensitive.
package actionlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"tack/internal/action"
	"tack/internal/backend/filestore"
	"tack/internal/query"
	"tack/internal/task"
)

// Spec is the `action_log:` section; set at most one of file and dir.
type Spec struct {
	File string `yaml:"file,omitempty"` // .jsonl, relative to the config file
	Dir  string `yaml:"dir,omitempty"`  // one JSON file per run, relative to the config file
	// MaxSize (KiB, file only): past it the file is moved to FILE.1 (replacing
	// the one before), so the log on disk stays under about twice this. 0 = grows
	// for ever.
	MaxSize int `yaml:"max_size,omitempty"`
	// Keep (dir only): how many run files to keep, the newest; older ones are
	// deleted as new ones are written. 0 = keep all.
	Keep int `yaml:"keep,omitempty"`
}

// Enabled reports whether any destination is set.
func (s Spec) Enabled() bool {
	return strings.TrimSpace(s.File) != "" || strings.TrimSpace(s.Dir) != ""
}

// Record is one run of an action.
type Record struct {
	Time       time.Time `json:"time"`  // when it started
	Ended      time.Time `json:"ended"` // when it ended
	DurationMS int64     `json:"duration_ms"`
	Session    string    `json:"session"`
	RunID      string    `json:"run_id,omitempty"` // the child's $TACK_RUN_ID; `tack report` records carry it too
	Source     string    `json:"source"`           // tui | cli | hook
	Event      string    `json:"event,omitempty"`  // hook runs: the event that fired it (task.moved, ...); Action is then the hook's name
	Action     string    `json:"action"`
	Mode       string    `json:"mode"`
	Backend    string    `json:"backend,omitempty"`
	ID         string    `json:"id"`
	Title      string    `json:"title,omitempty"`
	Result     string    `json:"result"` // ok | error
	Error      string    `json:"error,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Command    []string  `json:"command"`
	Workdir    string    `json:"workdir,omitempty"`
	Stdin      string    `json:"stdin,omitempty"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"` // output was cut at the capture limit
	// Report records (source "report", written by `tack report`): what the agent said.
	ReportStatus string `json:"report_status,omitempty"`
	ReportNote   string `json:"report_note,omitempty"`
}

// Results of a run.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// Duration is the run time.
func (r Record) Duration() time.Duration { return time.Duration(r.DurationMS) * time.Millisecond }

// Summary is the one line the audit trail keeps: action, time, exit status and
// output sizes. It never holds what the command printed: the audit file is
// kept for good (and may be shipped to a log collector, unlike the opt-in action
// log), and a token or a prompt echoed by an agent must not end up there. An error is only included
// when tack made it up (timed out, canceled, could not start); a command that
// exited by itself is described by its exit status.
func (r Record) Summary() string {
	var b strings.Builder
	b.WriteString("action " + r.Action + " " + FormatDuration(r.Duration()))
	if r.Mode == action.ModeInteractive {
		b.WriteString(" (terminal)")
	} else {
		if r.ExitCode != nil {
			fmt.Fprintf(&b, " exit %d", *r.ExitCode)
		}
		fmt.Fprintf(&b, " out %s err %s", size(len(r.Stdout)), size(len(r.Stderr)))
	}
	if r.Error != "" && r.ExitCode == nil {
		b.WriteString(": " + oneLine(r.Error))
	}
	return b.String()
}

// FormatDuration is "<1s", "7s", "1m12s".
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Round(time.Second).String()
}

func size(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func oneLine(s string) string { return firstLine(s) }

// FromJob builds the record of a background run that has ended.
func FromJob(j *action.Job, source string) Record {
	snap := j.Snapshot()
	r := base(j.Spec, j.Task, j.Plan, snap.Started, snap.Ended, snap.Err, source)
	r.Stdout, r.Stderr, r.Truncated = snap.Stdout, snap.Stderr, snap.Truncated
	if snap.ExitCode >= 0 {
		code := snap.ExitCode
		r.ExitCode = &code
	}
	return r
}

// Interactive builds the record of a run that had the terminal; its output
// was never captured.
func Interactive(s action.Spec, t task.Task, p action.Plan, started, ended time.Time, err error, source string) Record {
	return base(s, t, p, started, ended, err, source)
}

func base(s action.Spec, t task.Task, p action.Plan, started, ended time.Time, err error, source string) Record {
	r := Record{Time: started, Ended: ended, DurationMS: ended.Sub(started).Milliseconds(), Source: source,
		Action: s.Name, Mode: s.Mode, Backend: t.Backend, ID: t.ID, Title: t.Title, Result: ResultOK,
		Command: p.Argv, Workdir: p.Dir, Stdin: p.Stdin, RunID: s.Env["TACK_RUN_ID"]} // p.Env is never stored
	if len(r.Stdin) > maxStdin {
		r.Stdin, r.Truncated = cutUTF8(r.Stdin, maxStdin), true
	}
	if err != nil {
		r.Result, r.Error = ResultError, strings.TrimPrefix(err.Error(), s.Name+": ")
	}
	return r
}

var runSeq atomic.Int64

// NewRunID returns an id for one run, unique across processes and within one:
// UTC time, process id and a counter.
func NewRunID() string {
	return fmt.Sprintf("%s-%d-%d", time.Now().UTC().Format("20060102T150405"), os.Getpid(), runSeq.Add(1))
}

// Log writes and reads records. A nil *Log is a disabled log: every method is safe.
type Log struct {
	file, dir string
	session   string
	maxSize   int64 // bytes; 0 = no rotation
	keep      int
	appends   int // dir: appends so far, to prune now and then
	mu        sync.Mutex
}

// New returns the log for spec, or nil (disabled) when nothing is configured.
// Relative paths resolve against baseDir. Nothing is created until the first
// Append. session tags the records of this run.
func New(s Spec, baseDir, session string) (*Log, error) {
	file, dir := strings.TrimSpace(s.File), strings.TrimSpace(s.Dir)
	switch {
	case file == "" && dir == "":
		return nil, nil
	case file != "" && dir != "":
		return nil, errors.New("action_log: set either file or dir, not both")
	case file != "" && !strings.EqualFold(filepath.Ext(file), ".jsonl"):
		return nil, fmt.Errorf("action_log.file %q: use a .jsonl file (or set dir for one JSON file per run)", file)
	}
	switch {
	case s.MaxSize < 0 || s.Keep < 0:
		return nil, errors.New("action_log: max_size and keep cannot be negative")
	case s.MaxSize > 0 && file == "":
		return nil, errors.New("action_log.max_size is for file; use keep with dir")
	case s.Keep > 0 && dir == "":
		return nil, errors.New("action_log.keep is for dir; use max_size with file")
	}
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}
	l := &Log{session: session, maxSize: int64(s.MaxSize) << 10, keep: s.Keep}
	if file != "" {
		l.file = abs(file)
	} else {
		l.dir = abs(dir)
	}
	return l, nil
}

// Enabled reports whether runs are being stored.
func (l *Log) Enabled() bool { return l != nil }

// Path is the file or directory records go to ("" when disabled).
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.file + l.dir
}

// Session is the id records of this run carry.
func (l *Log) Session() string {
	if l == nil {
		return ""
	}
	return l.session
}

// Append stores r (stamping the session). It is a no-op on a disabled log.
func (l *Log) Append(r Record) error {
	if l == nil {
		return nil
	}
	r.Session = l.session
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != "" {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(l.file), 0o700); err != nil {
			return fmt.Errorf("action log: %w", err)
		}
		if l.maxSize > 0 {
			// Under a lock, so two tack processes do not rotate over each other.
			unlock, err := filestore.LockFile(l.file+".lock", rotateLockWait)
			if err != nil {
				return fmt.Errorf("action log: %w", err)
			}
			defer unlock()
			if st, err := os.Stat(l.file); err == nil && st.Size() > l.maxSize {
				if err := os.Rename(l.file, l.file+".1"); err != nil {
					return fmt.Errorf("action log: rotate: %w", err)
				}
			}
		}
		f, err := os.OpenFile(l.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("action log: %w", err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil { // one write: lines of concurrent runs do not interleave
			f.Close()
			return fmt.Errorf("action log: %w", err)
		}
		return f.Close()
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	sub := filepath.Join(l.dir, safe(r.Action))
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return fmt.Errorf("action log: %w", err)
	}
	stem := fmt.Sprintf("%s-%s-%s", r.Time.UTC().Format("20060102T150405Z"), safe(r.Backend), safe(r.ID))
	for n := 0; ; n++ { // never overwrite: two runs of the same card in one second get -1, -2, ...
		name := stem + ".json"
		if n > 0 {
			name = fmt.Sprintf("%s-%d.json", stem, n)
		}
		f, err := os.OpenFile(filepath.Join(sub, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("action log: %w", err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			f.Close()
			return fmt.Errorf("action log: %w", err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		if l.appends++; l.keep > 0 && (l.appends == 1 || l.appends%pruneEvery == 0) {
			l.prune()
		}
		return nil
	}
}

// rotateLockWait is how long a rotating append waits for another tack.
const rotateLockWait = 5 * time.Second

// pruneEvery is how many appends pass between two prunes of a dir log (and the
// first append prunes), so keeping the directory small costs a walk now and then.
const pruneEvery = 20

// runFiles lists the run files of a dir log, newest first (their names start
// with the UTC start time).
func (l *Log) runFiles() ([]string, error) {
	var paths []string
	err := filepath.WalkDir(l.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			paths = append(paths, p)
		}
		return nil // skip what cannot be read
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sort.Slice(paths, func(i, j int) bool { return filepath.Base(paths[i]) > filepath.Base(paths[j]) })
	return paths, nil
}

// prune deletes the run files beyond the newest l.keep. Best effort. l.mu held.
func (l *Log) prune() {
	paths, err := l.runFiles()
	if err != nil || len(paths) <= l.keep {
		return
	}
	for _, p := range paths[l.keep:] {
		_ = os.Remove(p)
	}
}

// safe makes s usable in a file name: letters and digits of any script stay
// (two actions called "é" and "è" must not share a directory), the rest becomes _.
func safe(s string) string {
	out := strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r) { // action names may use any letter
			return r
		}
		return '_'
	}, s)
	if out == "" || strings.Trim(out, ".") == "" {
		return "_"
	}
	return out
}

// Records returns the stored records, oldest first. Unreadable or damaged
// files and lines are skipped. includeOwn false leaves out the runs of this
// session (the board shows those live).
func (l *Log) Records(includeOwn bool) ([]Record, error) {
	recs, _, err := l.Recent(includeOwn, 0)
	return recs, err
}

// Recent is Records limited to the newest n records (n <= 0 means all), still
// oldest first. older counts the entries left unread before them: a file's
// lines or a directory's files, so an estimate (damaged ones count too). The
// older ones are not parsed, which keeps a big, never-trimmed log cheap.
func (l *Log) Recent(includeOwn bool, n int) (recs []Record, older int, err error) {
	if l == nil {
		return nil, 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Record
	keep := func(r Record) {
		if r.Time.IsZero() || r.Action == "" || (!includeOwn && r.Session == l.session) {
			return
		}
		out = append(out, r)
	}
	if l.file != "" {
		// The current file, then FILE.1 (what a rotation moved aside), newest line
		// first, each read from its end in chunks: the older lines are never parsed.
		stopped := false
		for _, p := range []string{l.file, l.file + ".1"} {
			f, err := os.Open(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, 0, err
			}
			if stopped {
				older += countLines(f, -1)
				f.Close()
				continue
			}
			unread, early, err := reverseLines(f, func(line []byte) bool {
				var r Record
				if line = bytes.TrimSpace(line); len(line) > 0 && json.Unmarshal(line, &r) == nil {
					keep(r)
				}
				return n <= 0 || len(out) < n
			})
			if err == nil && early {
				older += countLines(f, unread)
				stopped = true
			}
			f.Close()
			if err != nil {
				return nil, 0, err
			}
		}
	} else {
		paths, err := l.runFiles()
		if err != nil {
			return nil, 0, err
		}
		for i, p := range paths {
			if n > 0 && len(out) >= n {
				older = len(paths) - i
				break
			}
			data, err := os.ReadFile(p)
			var r Record
			if err == nil && json.Unmarshal(data, &r) == nil {
				keep(r)
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // collected newest first: equal times keep their stored order
		out[i], out[j] = out[j], out[i]
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, older, nil
}

// chunk is how much of a log file is read at a time from its end.
const chunk = 64 << 10

// reverseLines calls visit with the lines of f, last first, reading backwards in
// chunks. It stops when visit returns false; early then says that lines remain
// and unread is the length of the part of the file before them (the lines in
// [0, unread) were not visited).
func reverseLines(f *os.File, visit func(line []byte) (more bool)) (unread int64, early bool, err error) {
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, false, err
	}
	var carry []byte // the start of the first line seen so far: its beginning is in an earlier chunk
	buf := make([]byte, chunk)
	for off > 0 {
		n := int64(len(buf))
		if off < n {
			n = off
		}
		off -= n
		if _, err := f.ReadAt(buf[:n], off); err != nil {
			return 0, false, err
		}
		data := append(append([]byte(nil), buf[:n]...), carry...)
		for {
			i := bytes.LastIndexByte(data, '\n')
			if i < 0 {
				break
			}
			line := data[i+1:]
			data = data[:i]
			if !visit(line) {
				return off + int64(len(data)), true, nil
			}
		}
		carry = data
	}
	if len(carry) > 0 {
		if !visit(carry) {
			return 0, false, nil
		}
	}
	return 0, false, nil
}

// countLines is the number of lines in the first upto bytes of f (all of it when
// upto < 0), counted without parsing: an estimate, as blank or damaged lines count.
func countLines(f *os.File, upto int64) int {
	if upto == 0 {
		return 0
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	r := io.Reader(f)
	if upto > 0 {
		r = io.LimitReader(f, upto)
	}
	buf := make([]byte, chunk)
	lines, last := 0, byte('\n')
	for {
		n, err := r.Read(buf)
		lines += bytes.Count(buf[:n], []byte{'\n'})
		if n > 0 {
			last = buf[n-1]
		}
		if err != nil {
			break
		}
	}
	if last != '\n' {
		lines++ // the last line has no newline
	}
	return lines
}

// Fields are the names usable in a filter over records.
var Fields = []string{"action", "mode", "result", "date", "time", "source", "event", "backend", "id", "title", "exit", "run", "report_status"}

// SearchFields are what a plain word of a filter is matched against.
var SearchFields = []string{"action", "title", "id", "error"}

// FieldDocs say what each of Fields is, for the filter help.
var FieldDocs = []query.FieldDoc{
	{Name: "action", Desc: "name of the action that ran"},
	{Name: "mode", Desc: "background or interactive (had the terminal)"},
	{Name: "result", Desc: "ok or error"},
	{Name: "date", Desc: "local day the run started, YYYY-MM-DD: date=today"},
	{Name: "time", Desc: "local date and time the run started"},
	{Name: "source", Desc: "tui, cli or hook"},
	{Name: "event", Desc: "event that fired a hook run: event:task.moved"},
	{Name: "backend", Desc: "backend of the task it ran on"},
	{Name: "id", Desc: "id of the task it ran on"},
	{Name: "title", Desc: "title of that task when it ran"},
	{Name: "exit", Desc: "exit status of the command (not set when killed)"},
	{Name: "run", Desc: "run id: run=ID lists a run and the tack report records made inside it"},
	{Name: "report_status", Desc: "status a tack report record moved the task to"},
}

// Lookup exposes a record's fields to the query matcher.
func Lookup(r Record) query.Lookup {
	return func(field string) (string, bool) {
		switch field {
		case "action":
			return r.Action, true
		case "mode":
			return r.Mode, true
		case "result":
			return r.Result, true
		case "date":
			return r.Time.Local().Format("2006-01-02"), true
		case "time":
			return r.Time.Local().Format("2006-01-02 15:04:05"), true
		case "source":
			return r.Source, true
		case "event":
			return r.Event, true
		case "backend":
			return r.Backend, true
		case "id":
			return r.ID, true
		case "title":
			return r.Title, true
		case "error":
			return r.Error, true
		case "exit":
			if r.ExitCode != nil {
				return fmt.Sprint(*r.ExitCode), true
			}
			return "", true
		case "run":
			return r.RunID, true
		case "report_status":
			return r.ReportStatus, true
		}
		return "", false
	}
}

// Parse parses and validates a filter over records.
func Parse(src string) (query.Query, error) {
	q, err := query.Parse(src)
	if err != nil {
		return q, err
	}
	return q, q.Check(append(append([]string{}, Fields...), "error"))
}

// Filter keeps the records matching q (all of them when q is empty).
func Filter(recs []Record, q query.Query) []Record {
	if q.Empty() {
		return recs
	}
	var out []Record
	for _, r := range recs {
		if q.Match(Lookup(r), SearchFields) {
			out = append(out, r)
		}
	}
	return out
}

// maxStdin caps the stdin kept in a record, like the captured output.
const maxStdin = 256 << 10

// cutUTF8 shortens s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
