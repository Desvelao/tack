// Package query implements tkan's filter language: plain text terms plus
// field-targeted terms with a few operators. It knows nothing about the UI.
//
// See Help for the user-facing syntax.
package query

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultSearch lists the fields a plain text term is matched against.
var DefaultSearch = []string{"id", "title", "description", "notes"}

// LogicalFields are the task fields every backend provides.
var LogicalFields = []string{"id", "title", "description", "status", "priority", "notes", "type", "assignee", "due", "start", "labels"}

type op int

const (
	opContains op = iota
	opNotContains
	opEquals
	opNotEquals
	opPrefix
	opRegex
	opLess
	opLessEq
	opGreater
	opGreaterEq
)

// operators in detection order (two-character ones first).
var operators = []struct {
	tok string
	op  op
}{
	{"!=", opNotEquals}, {"!:", opNotContains}, {"<=", opLessEq}, {">=", opGreaterEq},
	{"<", opLess}, {">", opGreater},
	{":", opContains}, {"=", opEquals}, {"^", opPrefix}, {"~", opRegex},
}

type term struct {
	src    string
	field  string // "" = plain text over the search fields
	op     op
	neg    bool // plain text only: -text
	values []string
	re     *regexp.Regexp
}

// Query is a parsed filter: alternatives (split by OR) of terms that must all
// match. The zero value matches everything.
type Query struct {
	groups [][]term
}

// each calls f for every term of every alternative.
func (q Query) each(f func(t *term) bool) {
	for gi := range q.groups {
		for ti := range q.groups[gi] {
			if !f(&q.groups[gi][ti]) {
				return
			}
		}
	}
}

// Lookup returns a field's value; ok is false for fields the record lacks
// (treated as empty).
type Lookup func(field string) (value string, ok bool)

// Normalize canonicalises a field name: lower case, spaces as underscores, col -> column.
func Normalize(name string) string {
	n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "_")
	if n == "col" {
		return "column"
	}
	return n
}

// Empty reports whether the query has no terms.
func (q Query) Empty() bool { return len(q.groups) == 0 }

// Fields returns the distinct field names referenced by field terms.
func (q Query) Fields() []string {
	seen := map[string]bool{}
	var out []string
	q.each(func(t *term) bool {
		if t.field != "" && !seen[t.field] {
			seen[t.field] = true
			out = append(out, t.field)
		}
		return true
	})
	return out
}

// Check verifies that every field term names one of the known fields.
func (q Query) Check(known []string) error {
	set := make(map[string]bool, len(known))
	for _, k := range known {
		set[Normalize(k)] = true
	}
	var err error
	q.each(func(t *term) bool {
		if t.field != "" && !set[t.field] {
			err = fmt.Errorf("unknown field %q in %q (fields: %s)%s; quote the term to search for it as text",
				t.field, t.src, strings.Join(known, ", "), DidYouMean(t.field, known))
			return false
		}
		return true
	})
	return err
}

// Match reports whether the record satisfies every term. search lists the
// fields plain text terms look at (DefaultSearch when empty).
func (q Query) Match(get Lookup, search []string) bool {
	if len(search) == 0 {
		search = DefaultSearch
	}
	if len(q.groups) == 0 {
		return true // the empty query matches everything
	}
	for _, g := range q.groups {
		all := true
		for i := range g {
			if !g[i].match(get, search) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (t *term) match(get Lookup, search []string) bool {
	if t.field == "" {
		found := false
		for _, f := range search {
			v, _ := get(Normalize(f))
			if containsAny(strings.ToLower(v), t.values) {
				found = true
				break
			}
		}
		return found != t.neg
	}
	v, _ := get(t.field)
	switch t.op {
	case opContains:
		return containsAny(strings.ToLower(v), t.values)
	case opNotContains:
		return !containsAny(strings.ToLower(v), t.values)
	case opEquals:
		return equalsAny(v, expandDates(t.values))
	case opNotEquals:
		return !equalsAny(v, expandDates(t.values))
	case opPrefix:
		lv := strings.ToLower(strings.TrimSpace(v))
		for _, p := range t.values {
			if strings.HasPrefix(lv, p) {
				return true
			}
		}
		return false
	case opRegex:
		return t.re.MatchString(v)
	case opLess, opLessEq, opGreater, opGreaterEq:
		return compareAny(strings.ToLower(strings.TrimSpace(v)), t.op, expandDates(t.values))
	}
	return false
}

// Now is the clock for relative dates; tests replace it.
var Now = time.Now

var relDate = regexp.MustCompile(`^(today|tomorrow|yesterday)(?:([+-])(\d+)([dw]))?$`)

// expandDates turns today, tomorrow, yesterday and today+7d / today-2w (d =
// days, w = weeks) into ISO dates; other values pass through unchanged.
func expandDates(values []string) []string {
	var out []string
	for i, v := range values {
		m := relDate.FindStringSubmatch(v)
		if m == nil {
			continue
		}
		if out == nil {
			out = append([]string(nil), values...)
		}
		d := Now()
		switch m[1] {
		case "tomorrow":
			d = d.AddDate(0, 0, 1)
		case "yesterday":
			d = d.AddDate(0, 0, -1)
		}
		if m[3] != "" {
			n, _ := strconv.Atoi(m[3])
			if m[4] == "w" {
				n *= 7
			}
			if m[2] == "-" {
				n = -n
			}
			d = d.AddDate(0, 0, n)
		}
		out[i] = d.Format("2006-01-02")
	}
	if out == nil {
		return values
	}
	return out
}

// compareAny orders numerically when both sides are numbers ("9" < "10") and
// by string otherwise, which sorts ISO dates correctly. An empty field never
// matches an ordering term.
func compareAny(lv string, o op, values []string) bool {
	if lv == "" {
		return false
	}
	for _, s := range values {
		c := compareNumOrText(lv, s)
		if o == opLess && c < 0 || o == opLessEq && c <= 0 || o == opGreater && c > 0 || o == opGreaterEq && c >= 0 {
			return true
		}
	}
	return false
}

func compareNumOrText(a, b string) int {
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(b, 64)
	if errA == nil && errB == nil && !math.IsNaN(fa) && !math.IsNaN(fb) {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

func containsAny(lv string, values []string) bool {
	for _, s := range values {
		if strings.Contains(lv, s) {
			return true
		}
	}
	return false
}

func equalsAny(v string, values []string) bool {
	lv := strings.ToLower(strings.TrimSpace(v))
	for _, s := range values {
		if lv == s {
			return true
		}
	}
	return false
}

// Parse compiles a filter string. Terms are separated by whitespace outside quotes.
func Parse(s string) (Query, error) {
	toks, err := tokenize(s)
	if err != nil {
		return Query{}, err
	}
	var q Query
	var cur []term
	for _, raw := range toks {
		if raw == "OR" || raw == "|" { // alternatives; a quoted "OR" is plain text
			if len(cur) == 0 {
				return Query{}, fmt.Errorf("%s needs a term before it", raw)
			}
			q.groups = append(q.groups, cur)
			cur = nil
			continue
		}
		t, err := parseTerm(raw)
		if err != nil {
			return Query{}, err
		}
		cur = append(cur, t)
	}
	switch {
	case len(cur) > 0:
		q.groups = append(q.groups, cur)
	case len(q.groups) > 0:
		return Query{}, fmt.Errorf("OR needs a term after it")
	}
	return q, nil
}

// tokenize splits on whitespace outside double quotes, keeping the quotes.
func tokenize(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(s) && s[i+1] == '"':
			cur.WriteString(`\"`)
			i++
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case (c == ' ' || c == '\t') && !inQuote:
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in %q", s)
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks, nil
}

// unquote removes unescaped double quotes and resolves \".
func unquote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == '"':
			b.WriteByte('"')
			i++
		case s[i] == '"':
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// splitValues splits raw on commas outside quotes and unquotes each part.
func splitValues(raw string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\\' && i+1 < len(raw) && raw[i+1] == '"':
			cur.WriteString(`\"`)
			i++
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ',' && !inQuote:
			parts = append(parts, unquote(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(parts, unquote(cur.String()))
}

func isIdentStart(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' }
func isIdent(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9' || c == '.' || c == '-'
}

func parseTerm(raw string) (term, error) {
	t := term{src: raw}

	// field<op>value ?
	if isIdentStart(raw[0]) {
		i := 1
		for i < len(raw) && isIdent(raw[i]) {
			i++
		}
		for _, o := range operators {
			if strings.HasPrefix(raw[i:], o.tok) {
				t.field = Normalize(raw[:i])
				t.op = o.op
				return finishField(t, raw[i+len(o.tok):])
			}
		}
	}

	// "-field<op>value" is not a negated field term; it would silently turn
	// into a text exclusion that matches everything, so point at the real syntax.
	if len(raw) > 2 && raw[0] == '-' && isIdentStart(raw[1]) {
		i := 2
		for i < len(raw) && isIdent(raw[i]) {
			i++
		}
		for _, o := range operators {
			if strings.HasPrefix(raw[i:], o.tok) {
				return t, fmt.Errorf("%q: fields cannot be negated with '-'; use %s!=value or %s!:value", raw, raw[1:i], raw[1:i])
			}
		}
	}

	// plain text, optionally negated
	body := raw
	if strings.HasPrefix(body, "-") && len(body) > 1 {
		t.neg = true
		body = body[1:]
	}
	v := strings.ToLower(unquote(body))
	if v == "" {
		return t, fmt.Errorf("empty search term in %q", raw)
	}
	t.values = []string{v}
	return t, nil
}

func finishField(t term, rawValue string) (term, error) {
	if t.op == opRegex {
		pat := unquote(rawValue)
		if pat == "" {
			return t, fmt.Errorf("missing regular expression in %q", t.src)
		}
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return t, fmt.Errorf("invalid regular expression in %q: %v", t.src, err)
		}
		t.re = re
		return t, nil
	}
	for _, v := range splitValues(rawValue) {
		t.values = append(t.values, strings.ToLower(strings.TrimSpace(v)))
	}
	if t.op != opEquals && t.op != opNotEquals {
		for _, v := range t.values {
			if v == "" {
				return t, fmt.Errorf("missing value in %q (use field= to test for an empty field)", t.src)
			}
		}
	}
	return t, nil
}

// sortedUnique returns the distinct, sorted names.
func sortedUnique(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n = Normalize(n); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
