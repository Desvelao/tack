// Package csvbackend stores tasks in a CSV file.
//
// Which source column holds each logical field (id, title, status, ...) comes
// from the `options:` of the `type: csv` entry in tack.yaml; see docs/config.md. Without
// it the header must contain columns named like the fields. Required fields:
// id, title, status. Columns that are not mapped are kept in Task.Extra and
// written back unchanged.
package csvbackend

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"tack/internal/backend"
	"tack/internal/backend/filestore"
	"tack/internal/task"
)

func init() { backend.RegisterType("csv", Open, ".csv") }

const bom = "\xef\xbb\xbf"

var logicalFields = backend.LogicalFields

var required = backend.RequiredFields

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
	poll   pollMode
}

// The poll option is shared with the other backends (see backend.ParsePoll).
type pollMode = backend.PollMode

const (
	pollWatch = backend.PollWatch
	pollNone  = backend.PollNone
	pollEvery = backend.PollEvery
)

func parsePoll(v any) (pollMode, error) { return backend.ParsePoll("csv", v) }

type yamlSchema struct {
	Source string         `yaml:"source"`
	Header *bool          `yaml:"header"`
	Fields map[string]any `yaml:"fields"`
	Poll   any            `yaml:"poll"`
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
	if s.poll, err = parsePoll(ys.Poll); err != nil {
		return s, "", err
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

// Backend is the CSV file as a filestore.Store: locking, merging outside edits,
// atomic writes and polling are the Store's; the codec below is what is CSV.
type Backend struct {
	*filestore.Store
	c *codec
}

// codec reads and writes the CSV text. What it learns from the file (the
// header, which column holds which field, line endings, BOM) is kept so a write
// gives the file back in the shape it had.
type codec struct {
	sch schema

	mu        sync.Mutex // Stores asks while the Store may be decoding or encoding
	header    []string   // nil when the file has no header row
	colField  []string   // source column index -> logical field ("" = extra)
	extra     []string   // source column index -> unique Extra key (for unmapped columns)
	crlf      bool       // file uses \r\n record terminators; preserved on write
	cellCRLF  bool       // line breaks inside quoted cells are \r\n (encoding/csv reads them as \n); written back so
	bom       bool       // file starts with a UTF-8 BOM; preserved on write
	noFinalNL bool       // file did not end with a newline; preserved on write

	// An encode that adds columns for fields that gained a value: kept apart
	// until the file was written (Written), so a failed write leaves the layout.
	nextHeader   []string
	nextColField []string
}

// Open loads path. The file must already exist.
func Open(spec backend.Spec) (backend.Backend, error) {
	sch, path, err := parseSchema(spec)
	if err != nil {
		return nil, err
	}
	c := &codec{sch: sch}
	st, err := filestore.NewStore("csv", path, sch.poll, c)
	if err != nil {
		return nil, err
	}
	return &Backend{Store: st, c: c}, nil
}

// Stores reports whether the file has a column for the logical field.
func (b *Backend) Stores(field string) bool {
	b.c.mu.Lock()
	defer b.c.mu.Unlock()
	return slices.Contains(b.c.colField, field)
}

func field(t *task.Task, name string) *string { return t.FieldPtr(name) }

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

// resolve maps each source column to a logical field, from the schema and the (possibly nil) header.
func (c *codec) resolve(header []string, ncols int) ([]string, error) {
	colField := make([]string, ncols)
	for _, f := range logicalFields {
		ref, configured := c.sch.fields[f]
		idx := -1
		switch {
		case configured && ref.pos > 0:
			idx = ref.pos - 1
		case header != nil:
			ref = c.sch.ref(f)
			for i, h := range header {
				if strings.EqualFold(strings.TrimSpace(h), ref.name) {
					idx = i
					break
				}
			}
		}
		if idx < 0 || idx >= ncols {
			if required[f] {
				if header == nil {
					return nil, fmt.Errorf("headerless csv needs options.fields.%s set to a column position", f)
				}
				return nil, fmt.Errorf("missing required column %s (field %q)", c.sch.ref(f), f)
			}
			continue
		}
		if prev := colField[idx]; prev != "" {
			return nil, fmt.Errorf("column %d is mapped to both %q and %q", idx+1, prev, f)
		}
		colField[idx] = f
	}
	return colField, nil
}

// Decode implements filestore.Codec. A file that fails leaves what the codec
// knew of the last good one alone.
func (c *codec) Decode(data []byte) ([]task.Task, error) {
	crlf := c.crlf
	if i := bytes.IndexByte(data, '\n'); i > 0 {
		crlf = data[i-1] == '\r'
	}

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
			return nil, err
		}
		line, _ := r.FieldPos(0)
		recs, recLine = append(recs, rec), append(recLine, line)
	}

	var header []string
	body := recs
	if c.sch.header {
		if len(recs) == 0 {
			// Empty file: start with a header made of the configured names.
			for _, f := range logicalFields[:6] {
				name := c.sch.ref(f).name
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
	if c.sch.header {
		for len(header) < ncols {
			header = append(header, "")
		}
	} else {
		for _, ref := range c.sch.fields {
			if ref.pos > ncols {
				ncols = ref.pos
			}
		}
	}
	colField, err := c.resolve(header, ncols)
	if err != nil {
		return nil, err
	}
	extra := extraKeys(header, ncols)

	tasks := make([]task.Task, 0, len(body))
	firstRec := 0 // index into recLine of body[0]
	if c.sch.header && len(recs) > 0 {
		firstRec = 1
	}
	lineOf := map[string]int{} // id -> line, to name both lines of a duplicate
	for n, row := range body {
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue
		}
		var t task.Task
		for i, v := range row {
			if i < ncols && colField[i] != "" {
				*field(&t, colField[i]) = v
				continue
			}
			if t.Extra == nil {
				t.Extra = map[string]string{}
			}
			t.Extra[extra[i]] = v
		}
		// Update and Delete find a task by ID, so an empty or repeated ID would
		// leave a row unreachable (or make a write hit the wrong one).
		line := 0
		if firstRec+n < len(recLine) {
			line = recLine[firstRec+n]
		}
		switch prev, dup := lineOf[t.ID]; {
		case strings.TrimSpace(t.ID) == "":
			return nil, fmt.Errorf("line %d has an empty id; every row needs a unique id", line)
		case dup:
			return nil, fmt.Errorf("line %d repeats id %q of line %d; ids must be unique", line, t.ID, prev)
		}
		lineOf[t.ID] = line
		tasks = append(tasks, t)
	}

	c.mu.Lock()
	c.header, c.colField, c.extra = header, colField, extra
	c.crlf, c.cellCRLF = crlf, quotedCRLF(data)
	c.bom = bytes.HasPrefix(data, []byte(bom))
	c.noFinalNL = len(data) > 0 && data[len(data)-1] != '\n'
	c.mu.Unlock()
	return tasks, nil
}

// Encode implements filestore.Codec: the whole file, in the layout Decode saw.
func (c *codec) Encode(tasks []task.Task, _ []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	header := append([]string(nil), c.header...)
	colField := append([]string(nil), c.colField...)
	if c.sch.header {
		// Optional fields that were not in the file but now have values get a new column.
		mapped := map[string]bool{}
		for _, f := range colField {
			mapped[f] = true
		}
		for _, f := range logicalFields {
			if mapped[f] {
				continue
			}
			for i := range tasks {
				if *field(&tasks[i], f) != "" {
					name := c.sch.ref(f).name
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

	var content bytes.Buffer
	bw := bufio.NewWriter(&content)
	var rec bytes.Buffer
	emit := func(row []string) error {
		rec.Reset()
		cw := csv.NewWriter(&rec)
		if c.cellCRLF {
			row = slices.Clone(row)
			for i, v := range row {
				row[i] = strings.ReplaceAll(v, "\n", "\r\n")
			}
		}
		if err := cw.Write(row); err != nil {
			return err
		}
		cw.Flush()
		line := rec.Bytes() // ends with "\n"; newlines inside fields stay as-is
		if c.crlf {
			line = append(line[:len(line)-1], '\r', '\n')
		}
		_, err := bw.Write(line)
		return err
	}
	if c.sch.header {
		if err := emit(header); err != nil {
			return nil, err
		}
	}
	for i := range tasks {
		t := &tasks[i]
		row := make([]string, ncols)
		for j := 0; j < ncols; j++ {
			if f := colField[j]; f != "" {
				row[j] = *field(t, f)
			} else {
				row[j] = t.Extra[c.extra[j]]
			}
		}
		if err := emit(row); err != nil {
			return nil, err
		}
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	data := content.Bytes()
	if c.noFinalNL { // keep a file that had no trailing newline that way
		data = bytes.TrimSuffix(bytes.TrimSuffix(data, []byte("\n")), []byte("\r"))
	}
	if c.bom {
		data = append([]byte(bom), data...)
	}
	c.nextHeader, c.nextColField = header, colField
	return data, nil
}

// Written implements filestore.Written: the file now has the columns Encode added.
func (c *codec) Written() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sch.header {
		c.header = c.nextHeader
	}
	c.colField = c.nextColField
}

// quotedCRLF reports whether a line break inside a quoted cell of data is \r\n.
func quotedCRLF(data []byte) bool {
	in := false
	for i, c := range data {
		switch {
		case c == '"':
			in = !in
		case in && c == '\r' && i+1 < len(data) && data[i+1] == '\n':
			return true
		}
	}
	return false
}
