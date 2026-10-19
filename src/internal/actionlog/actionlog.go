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

	"tkan/internal/action"
	"tkan/internal/query"
	"tkan/internal/task"
)

// Spec is the `action_log:` section; set at most one of file and dir.
type Spec struct {
	File string `yaml:"file,omitempty"` // .jsonl, relative to the config file
	Dir  string `yaml:"dir,omitempty"`  // one JSON file per run, relative to the config file
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
	RunID      string    `json:"run_id,omitempty"` // the child's $TKAN_RUN_ID; `tkan report` records carry it too
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
	Cwd        string    `json:"cwd,omitempty"`
	Stdin      string    `json:"stdin,omitempty"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"` // output was cut at the capture limit
	// Report records (source "report", written by `tkan report`): what the agent said.
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
// less protected than the action log (0644 against 0600), and a token or a
// prompt echoed by an agent must not end up there. An error is only included
// when tkan made it up (timed out, canceled, could not start); a command that
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
		Command: p.Argv, Cwd: p.Dir, Stdin: p.Stdin, RunID: s.Env["TKAN_RUN_ID"]} // p.Env is never stored
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
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}
	l := &Log{session: session}
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
		return f.Close()
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
		data, err := os.ReadFile(l.file)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		if err != nil {
			return nil, 0, err
		}
		// Line by line without a size limit: a huge record must not hide the later ones.
		// Newest lines first, so the older ones need not be parsed.
		for len(data) > 0 && (n <= 0 || len(out) < n) {
			var line []byte
			if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
				data, line = data[:i], data[i+1:]
			} else {
				data, line = nil, data
			}
			var r Record
			if line = bytes.TrimSpace(line); len(line) > 0 && json.Unmarshal(line, &r) == nil {
				keep(r)
			}
		}
		for len(data) > 0 {
			var line []byte
			line, data, _ = bytes.Cut(data, []byte{'\n'})
			if len(bytes.TrimSpace(line)) > 0 {
				older++
			}
		}
	} else {
		var paths []string
		err := filepath.WalkDir(l.dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
				paths = append(paths, p)
			}
			return nil // skip what cannot be read
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, 0, err
		}
		// File names start with the UTC start time: newest first without reading them.
		sort.Slice(paths, func(i, j int) bool { return filepath.Base(paths[i]) > filepath.Base(paths[j]) })
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

// Fields are the names usable in a filter over records.
var Fields = []string{"action", "mode", "result", "date", "time", "source", "event", "backend", "id", "title", "exit"}

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
