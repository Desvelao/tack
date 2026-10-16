// Package sqlitebackend stores tasks in a table of a SQLite database. It does
// not link a SQLite library (tkan stays a small static binary): every
// operation starts a SQLite command line tool (sqlite3 by default), writes one
// SQL script to its stdin and reads JSON (`-json`) from its stdout.
//
// Values are never pasted into the SQL as text: they travel as hex blob
// literals (CAST(X'..' AS TEXT)), so no value can break out of its literal or
// be taken by the CLI for a dot command. Identifiers come from the config or
// from the database itself and are quoted. Writes run in one
// BEGIN IMMEDIATE transaction, so concurrent tkan processes are serialised by
// SQLite's own locking. See docs/backends.md.
package sqlitebackend

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"tkan/internal/backend"
	"tkan/internal/task"
)

func init() { backend.RegisterType("sqlite", Open, ".db", ".sqlite", ".sqlite3") }

const (
	defaultTable = "tasks"
	busyMillis   = 5000
	runTimeout   = 30 * time.Second
	maxStdout    = 32 << 20
	maxStderr    = 64 << 10
)

// defaultCLIs are probed in this order when options.cli is not set.
var defaultCLIs = []string{"sqlite3", "sqlite"}

type options struct {
	Source string            `yaml:"source"`
	Table  string            `yaml:"table"`
	CLI    any               `yaml:"cli"`
	Fields map[string]string `yaml:"fields"`
	Poll   any               `yaml:"poll"`
}

type column struct {
	name string
	typ  string
	pk   int // 1-based position in the primary key, 0 if not part of it
}

type stamp struct {
	mod, walMod   time.Time
	size, walSize int64
}

// Backend implements backend.Backend, backend.Watcher and backend.Reloader.
type Backend struct {
	path    string
	table   string
	cli     []string
	fields  map[string]string // logical field -> configured column name
	poll    backend.PollMode
	timeout time.Duration
	now     func() time.Time

	mu        sync.Mutex
	cols      []column // columns of the table as of the last read; nil when it does not exist
	tasks     []task.Task
	stamp     stamp
	needLoad  bool      // a write happened: the next List re-reads
	stale     bool      // poll none/N: a change was seen, the next List re-reads
	lastCheck time.Time // poll N: when the database was last examined
}

// Open resolves the CLI, validates the options and reads the table (a missing
// database or table is an empty backlog, created by the first Create).
func Open(spec backend.Spec) (backend.Backend, error) {
	var o options
	if spec.Options.Kind != 0 {
		if err := spec.Options.Decode(&o); err != nil {
			return nil, fmt.Errorf("sqlite options: %w", err)
		}
	}
	if strings.TrimSpace(o.Source) == "" {
		return nil, errors.New("sqlite options: source is required (path of the database file)")
	}
	b := &Backend{
		path:    spec.Resolve(strings.TrimSpace(o.Source)),
		table:   defaultTable,
		fields:  map[string]string{},
		timeout: runTimeout,
		now:     time.Now,
	}
	if strings.HasPrefix(b.path, "-") { // never let the path read as an option
		b.path = "." + string(os.PathSeparator) + b.path
	}
	if t := strings.TrimSpace(o.Table); t != "" {
		b.table = t
	}
	if err := checkIdent("table", b.table); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, f := range backend.LogicalFields {
		known[f] = true
	}
	for f, c := range o.Fields {
		f, c = strings.ToLower(strings.TrimSpace(f)), strings.TrimSpace(c)
		if !known[f] {
			return nil, fmt.Errorf("sqlite options: unknown field %q (valid: %s)", f, strings.Join(backend.LogicalFields, ", "))
		}
		if c == "" {
			return nil, fmt.Errorf("sqlite options: field %q: empty column name", f)
		}
		if err := checkIdent("column of field "+f, c); err != nil {
			return nil, err
		}
		b.fields[f] = c
	}
	var err error
	if b.poll, err = backend.ParsePoll("sqlite", o.Poll); err != nil {
		return nil, err
	}
	if b.cli, err = resolveCLI(o.CLI); err != nil {
		return nil, err
	}
	if _, err := os.Stat(b.path); err == nil {
		if err := b.reload(context.Background()); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *Backend) Close() error { return nil }

// ---- the CLI ----------------------------------------------------------------

// resolveCLI returns the command (program + leading arguments) to run: the one
// configured, or the first default candidate that is installed and prints JSON.
func resolveCLI(v any) ([]string, error) {
	var configured []string
	switch x := v.(type) {
	case nil:
	case string:
		if strings.TrimSpace(x) != "" {
			configured = []string{strings.TrimSpace(x)}
		}
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok || s == "" {
				return nil, errors.New("sqlite options: cli must be a program name or a list of strings (program and arguments)")
			}
			configured = append(configured, s)
		}
	default:
		return nil, errors.New("sqlite options: cli must be a program name or a list of strings (program and arguments)")
	}
	if len(configured) > 0 {
		if err := probe(configured); err != nil {
			return nil, fmt.Errorf("sqlite options: cli %v: %w", configured, err)
		}
		return configured, nil
	}
	var tried []string
	for _, name := range defaultCLIs {
		if _, err := exec.LookPath(name); err != nil {
			tried = append(tried, name+": not found")
			continue
		}
		if err := probe([]string{name}); err != nil {
			tried = append(tried, name+": "+err.Error())
			continue
		}
		return []string{name}, nil
	}
	return nil, fmt.Errorf("sqlite: no usable SQLite command line tool (%s); install sqlite3 or set options.cli", strings.Join(tried, "; "))
}

// probe checks that the command runs and understands -json (SQLite >= 3.33).
func probe(cli []string) error {
	out, err := run(context.Background(), cli, ":memory:", "SELECT 1 AS x;", 10*time.Second)
	if err != nil {
		return err
	}
	rows, err := parseResults(out)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		return errors.New("does not support -json (SQLite 3.33 or newer is required)")
	}
	return nil
}

// run starts the CLI on db, feeds it script and returns its stdout.
func run(ctx context.Context, cli []string, db, script string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(append([]string(nil), cli[1:]...), "-bail", "-json", db)
	cmd := exec.CommandContext(ctx, cli[0], args...)
	cmd.Stdin = strings.NewReader(script)
	cmd.WaitDelay = time.Second
	stdout, stderr := &capWriter{max: maxStdout}, &capWriter{max: maxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.truncated {
		return nil, fmt.Errorf("output exceeds %d MiB", maxStdout>>20)
	}
	if err != nil {
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			return nil, fmt.Errorf("timed out after %s", timeout)
		case errors.Is(ctx.Err(), context.Canceled):
			return nil, context.Canceled
		}
		msg := strings.TrimSpace(stderr.buf.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return stdout.buf.Bytes(), nil
}

// capWriter keeps at most max bytes and drops the rest (writes never fail).
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.max - w.buf.Len(); len(p) > room {
		w.truncated = true
		p = p[:max(room, 0)]
	}
	w.buf.Write(p)
	return n, nil
}

type row map[string]any

// parseResults decodes the stream of JSON arrays the CLI prints, one per
// statement that returned rows.
func parseResults(out []byte) ([][]row, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var res [][]row
	for {
		var rows []row
		if err := dec.Decode(&rows); err == io.EOF {
			return res, nil
		} else if err != nil {
			return nil, fmt.Errorf("unexpected output from the SQLite CLI: %v", err)
		}
		res = append(res, rows)
	}
}

func (b *Backend) exec(ctx context.Context, script string) ([][]row, error) {
	out, err := run(ctx, b.cli, b.path, ".timeout "+fmt.Sprint(busyMillis)+"\n"+script, b.timeout)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	return parseResults(out)
}

// ---- SQL building -----------------------------------------------------------

func checkIdent(what, s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("sqlite options: %s %q contains a control character", what, s)
		}
	}
	return nil
}

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// lit returns a SQL text literal for s that no content can escape.
func lit(s string) (string, error) {
	if strings.ContainsRune(s, 0) {
		return "", errors.New("sqlite: values cannot contain NUL characters")
	}
	if s == "" {
		return "''", nil
	}
	return "CAST(X'" + hex.EncodeToString([]byte(s)) + "' AS TEXT)", nil
}

func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return fmt.Sprint(v)
}

// ---- reading ----------------------------------------------------------------

const noTable = "no such table"

// load reads the table: its columns and every row.
func (b *Backend) load(ctx context.Context) ([]column, []task.Task, error) {
	res, err := b.exec(ctx, "PRAGMA table_info("+ident(b.table)+");\nSELECT * FROM "+ident(b.table)+";\n")
	if err != nil {
		if strings.Contains(err.Error(), noTable) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if len(res) == 0 {
		return nil, nil, nil // the table does not exist
	}
	var cols []column
	for _, r := range res[0] {
		pk := 0
		if n, ok := r["pk"].(json.Number); ok {
			v, _ := n.Int64()
			pk = int(v)
		}
		cols = append(cols, column{name: text(r["name"]), typ: text(r["type"]), pk: pk})
	}
	m, err := b.mapColumns(cols)
	if err != nil {
		return nil, nil, err
	}
	claimed := map[string]bool{}
	for _, c := range m {
		claimed[c] = true
	}
	var rows []row
	if len(res) > 1 {
		rows = res[1]
	}
	tasks := make([]task.Task, 0, len(rows))
	ids := map[string]bool{}
	for i, r := range rows {
		var t task.Task
		for f, c := range m {
			*t.FieldPtr(f) = text(r[c])
		}
		switch {
		case t.ID == "":
			return nil, nil, fmt.Errorf("sqlite: row %d of table %q has an empty id", i+1, b.table)
		case ids[t.ID]:
			return nil, nil, fmt.Errorf("sqlite: duplicate id %q in table %q", t.ID, b.table)
		}
		ids[t.ID] = true
		for _, c := range cols {
			if !claimed[c.name] {
				if t.Extra == nil {
					t.Extra = map[string]string{}
				}
				t.Extra[c.name] = text(r[c.name])
			}
		}
		tasks = append(tasks, t)
	}
	return cols, tasks, nil
}

// mapColumns resolves each logical field to a real column (names compare
// case-insensitively). Required fields must exist; an optional field that is
// configured but missing is an error, one left at its default name is skipped.
func (b *Backend) mapColumns(cols []column) (map[string]string, error) {
	m := map[string]string{}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
	}
	used := map[string]bool{}
	for _, f := range backend.LogicalFields {
		want, configured := b.fields[f]
		if !configured {
			want = f
		}
		found := ""
		for _, c := range cols {
			if strings.EqualFold(c.name, want) && !used[c.name] {
				found = c.name
				break
			}
		}
		switch {
		case found != "":
			m[f] = found
			used[found] = true
		case backend.RequiredFields[f] || configured:
			return nil, fmt.Errorf("sqlite: field %q: no column %q in table %q (columns: %s); set options.fields.%s",
				f, want, b.table, strings.Join(names, ", "), f)
		}
	}
	return m, nil
}

func statPath(p string) (time.Time, int64) {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}, 0
	}
	return st.ModTime(), st.Size()
}

func (b *Backend) statNow() stamp {
	var s stamp
	s.mod, s.size = statPath(b.path)
	s.walMod, s.walSize = statPath(b.path + "-wal")
	return s
}

// reload re-reads the table. The stamp is taken before the read, so a write
// landing meanwhile is noticed (at worst once too often) rather than missed.
func (b *Backend) reload(ctx context.Context) error {
	st := b.statNow()
	cols, tasks, err := b.load(ctx)
	if err != nil {
		return err
	}
	b.cols, b.tasks, b.stamp = cols, tasks, st
	b.needLoad, b.stale = false, false
	return nil
}

func (b *Backend) List(ctx context.Context) ([]task.Task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.needLoad || b.stale || (b.poll.Kind == backend.PollWatch && b.statNow() != b.stamp) {
		if err := b.reload(ctx); err != nil {
			return nil, err
		}
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

// Changed implements backend.Watcher: the database file (or its WAL) differs
// from what was last read or written.
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
	changed := b.statNow() != b.stamp
	if changed {
		b.stale = true
	}
	return changed, nil
}

// Reload implements backend.Reloader: re-read the table now, whatever poll says.
func (b *Backend) Reload() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reload(context.Background())
}

// ---- writing ----------------------------------------------------------------

// layout is the set of columns a write may touch.
type layout struct {
	cols    []column
	mapped  map[string]string // logical field -> column
	extra   []string          // unmapped columns
	idCol   string
	idAlias bool // the id column is the rowid: SQLite assigns it
	create  string
}

// plan describes the table to write to; when it does not exist yet it is the
// default layout (one column per field, named as configured) plus the CREATE.
func (b *Backend) plan() (layout, error) {
	cols := b.cols
	var create string
	if cols == nil {
		var defs []string
		for _, f := range backend.LogicalFields {
			name := f
			if c, ok := b.fields[f]; ok {
				name = c
			}
			switch f {
			case "id":
				cols = append(cols, column{name: name, typ: "INTEGER", pk: 1})
				defs = append(defs, ident(name)+" INTEGER PRIMARY KEY AUTOINCREMENT")
			default:
				cols = append(cols, column{name: name, typ: "TEXT"})
				defs = append(defs, ident(name)+" TEXT NOT NULL DEFAULT ''")
			}
		}
		create = "CREATE TABLE IF NOT EXISTS " + ident(b.table) + " (" + strings.Join(defs, ", ") + ");\n"
	}
	m, err := b.mapColumns(cols)
	if err != nil {
		return layout{}, err
	}
	l := layout{cols: cols, mapped: m, idCol: m["id"], create: create}
	claimed := map[string]bool{}
	for _, c := range m {
		claimed[c] = true
	}
	pks := 0
	for _, c := range cols {
		if c.pk > 0 {
			pks++
		}
		if !claimed[c.name] {
			l.extra = append(l.extra, c.name)
		}
	}
	for _, c := range cols {
		if c.name == l.idCol && c.pk == 1 && pks == 1 && strings.EqualFold(c.typ, "INTEGER") {
			l.idAlias = true
		}
	}
	return l, nil
}

// assignments lists "column", value-literal for every writable column of t
// (everything mapped except the id, plus the extras t carries).
func (l layout) assignments(t task.Task) (names, vals []string, err error) {
	add := func(col, v string) error {
		s, err := lit(v)
		if err != nil {
			return err
		}
		names, vals = append(names, ident(col)), append(vals, s)
		return nil
	}
	for _, f := range backend.LogicalFields {
		if c, ok := l.mapped[f]; ok && f != "id" {
			if err := add(c, *t.FieldPtr(f)); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, c := range l.extra {
		if v, ok := t.Extra[c]; ok {
			if err := add(c, v); err != nil {
				return nil, nil, err
			}
		}
	}
	return names, vals, nil
}

// write runs a script inside one immediate transaction and, on success,
// remembers the new file stamp so the change is not reported back as external.
func (b *Backend) write(ctx context.Context, body string) ([][]row, error) {
	res, err := b.exec(ctx, "BEGIN IMMEDIATE;\n"+body+"COMMIT;\n")
	if err != nil {
		return nil, err
	}
	b.stamp = b.statNow()
	b.needLoad = true
	return res, nil
}

func (b *Backend) Create(ctx context.Context, t task.Task) (task.Task, error) {
	if err := t.Validate(); err != nil {
		return task.Task{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	l, err := b.plan()
	if err != nil {
		return task.Task{}, err
	}
	names, vals, err := l.assignments(t)
	if err != nil {
		return task.Task{}, err
	}
	tbl := ident(b.table)
	if !l.idAlias { // max(numeric id)+1, like the csv backend
		names = append([]string{ident(l.idCol)}, names...)
		vals = append([]string{"(SELECT COALESCE(MAX(CAST(" + ident(l.idCol) + " AS INTEGER)), 0) + 1 FROM " + tbl + ")"}, vals...)
	}
	body := l.create +
		"INSERT INTO " + tbl + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ");\n" +
		"SELECT " + ident(l.idCol) + " AS id FROM " + tbl + " WHERE rowid = last_insert_rowid();\n"
	res, err := b.write(ctx, body)
	if err != nil {
		return task.Task{}, err
	}
	if len(res) == 0 || len(res[0]) != 1 || text(res[0][0]["id"]) == "" {
		return task.Task{}, errors.New("sqlite: the new row has no id")
	}
	if l.create != "" {
		b.cols = l.cols
	}
	t = t.Clone()
	t.ID = text(res[0][0]["id"])
	return t, nil
}

func (b *Backend) Update(ctx context.Context, t task.Task) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, err := b.tableOrNotFound(t.ID)
	if err != nil {
		return err
	}
	names, vals, err := l.assignments(t)
	if err != nil {
		return err
	}
	sets := make([]string, len(names))
	for i := range names {
		sets[i] = names[i] + " = " + vals[i]
	}
	if len(sets) == 0 {
		sets = []string{ident(l.idCol) + " = " + ident(l.idCol)}
	}
	id, err := lit(t.ID)
	if err != nil {
		return err
	}
	return b.changeOne(ctx, "UPDATE "+ident(b.table)+" SET "+strings.Join(sets, ", ")+
		" WHERE CAST("+ident(l.idCol)+" AS TEXT) = "+id+";\n", t.ID)
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, err := b.tableOrNotFound(id)
	if err != nil {
		return err
	}
	q, err := lit(id)
	if err != nil {
		return err
	}
	return b.changeOne(ctx, "DELETE FROM "+ident(b.table)+" WHERE CAST("+ident(l.idCol)+" AS TEXT) = "+q+";\n", id)
}

func (b *Backend) tableOrNotFound(id string) (layout, error) {
	if b.cols == nil {
		return layout{}, fmt.Errorf("%w: %s", backend.ErrNotFound, id)
	}
	return b.plan()
}

// changeOne runs a statement that must touch exactly one row.
func (b *Backend) changeOne(ctx context.Context, stmt, id string) error {
	res, err := b.write(ctx, stmt+"SELECT changes() AS n;\n")
	if err != nil {
		return err
	}
	if len(res) == 0 || len(res[0]) != 1 {
		return errors.New("sqlite: unexpected output from the SQLite CLI")
	}
	if text(res[0][0]["n"]) == "0" {
		return fmt.Errorf("%w: %s", backend.ErrNotFound, id)
	}
	return nil
}
