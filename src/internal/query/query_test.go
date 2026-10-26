package query

import (
	"strings"
	"testing"
	"time"

	"tack/internal/board"
	"tack/internal/task"
)

var sample = []task.Task{
	{ID: "1", Title: "Fix login bug", Description: "users cannot log in", Status: "pending", Priority: "high", Type: "fix", Assignee: "ann", Due: "2026-10-20", Extra: map[string]string{"story points": "5"}},
	{ID: "2", Title: "Bot fillers", Notes: "wip: prototype", Status: "in_progress", Priority: "medium", Type: "feature", Assignee: "bob", Due: "2026-11-05"},
	{ID: "A-3", Title: "Docs", Status: "done", Priority: "", Type: "chore", Extra: map[string]string{}},
}

func ids(t *testing.T, q string, search []string) string {
	t.Helper()
	p, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	var out []string
	for _, tk := range sample {
		if p.Match(TaskLookup(tk, "Col-"+tk.Status, board.Deps{}), search) {
			out = append(out, tk.ID)
		}
	}
	return strings.Join(out, ",")
}

func TestOperators(t *testing.T) {
	cases := map[string]string{
		"":                       "1,2,A-3",
		"bug":                    "1",
		"LOGIN":                  "1",
		"-bug":                   "2,A-3",
		"bug login":              "1",
		"bot -wip":               "",
		"title:fix":              "1",
		"title!:bot":             "1,A-3",
		"priority=high":          "1",
		"priority=HIGH":          "1",
		"priority=hi":            "",
		"priority!=high":         "2,A-3",
		"priority=":              "A-3",
		"priority!=":             "1,2",
		"priority=high,medium":   "1,2",
		"priority!=high,medium":  "A-3",
		"type^fe":                "2",
		"id~^[0-9]+$":            "1,2",
		"title~BUG|docs":         "1,A-3",
		`title:"fix login"`:      "1",
		`"fix login"`:            "1",
		`-"fix login"`:           "2,A-3",
		"col:col-done":           "A-3",
		"column=Col-pending":     "1",
		"assignee=ann":           "1",
		"assignee=":              "A-3",
		"due<2026-11-01":         "1",
		"due>=2026-11-01":        "2",
		"due<2027-01-01":         "1,2",
		"story_points=5":         "1",
		"status=in_progress":     "2",
		"notes:wip priority^med": "2",
		"priority=med":           "",
	}
	for q, want := range cases {
		if got := ids(t, q, nil); got != want {
			t.Errorf("%q: got %q want %q", q, got, want)
		}
	}
}

func TestSearchFieldsAreConfigurable(t *testing.T) {
	if got := ids(t, "bob", nil); got != "" {
		t.Fatalf("default search must not look at extras: %q", got)
	}
	if got := ids(t, "bob", []string{"assignee"}); got != "2" {
		t.Fatalf("custom search: %q", got)
	}
	if got := ids(t, "bug", []string{"Assignee", "priority"}); got != "" {
		t.Fatalf("title is not searched: %q", got)
	}
	if got := ids(t, "-bob", []string{"assignee"}); got != "1,A-3" {
		t.Fatalf("negated custom search: %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"title:":         "missing value",
		"title!:":        "missing value",
		"title^":         "missing value",
		"title~":         "missing regular expression",
		"title~(":        "invalid regular expression",
		`"open`:          "unterminated quote",
		"title:a,":       "missing value",
		`-""`:            "empty search term",
		`title:""`:       "missing value",
		"title:a,,b":     "missing value",
		"-priority=high": "cannot be negated",
		"-title:wip":     "title!=value",
		"title!~wip":     "unknown operator '!~'",
		"id!^A-":         "unknown operator '!^'",
	}
	for q, want := range cases {
		if _, err := Parse(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v want %q", q, err, want)
		}
	}
}

func TestCheckUnknownField(t *testing.T) {
	known := KnownFields(sample)
	q, _ := Parse("colour:red")
	err := q.Check(known)
	if err == nil || !strings.Contains(err.Error(), "unknown field \"colour\"") || !strings.Contains(err.Error(), "assignee") {
		t.Fatalf("%v", err)
	}
	for _, ok := range []string{"title:a", "col:x", "story_points=5", "Assignee=ann"} {
		q, _ := Parse(ok)
		if err := q.Check(known); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	// URLs and similar must be quotable.
	q, _ = Parse(`"http://x"`)
	if err := q.Check(known); err != nil || len(q.Fields()) != 0 {
		t.Fatalf("quoted term should be plain text: %v %v", err, q.Fields())
	}
	q, _ = Parse("http://x")
	if q.Check(known) == nil {
		t.Fatal("unquoted URL is read as field http")
	}
}

func TestKnownFieldsAndSearchCheck(t *testing.T) {
	k := KnownFields(sample)
	want := "id,title,description,status,priority,notes,type,assignee,due,start,labels,blocked_by,lease,claimed_at,column,backend,blocked,ready,running,story_points"
	if got := strings.Join(k, ","); got != want {
		t.Fatalf("got %s", got)
	}
	if err := CheckSearch([]string{"title", "Assignee"}, k); err != nil {
		t.Fatal(err)
	}
	if err := CheckSearch([]string{"nope"}, k); err == nil || !strings.Contains(err.Error(), "board.search") {
		t.Fatalf("%v", err)
	}
}

func TestHelpMentionsEveryOperatorAndFields(t *testing.T) {
	h := Help([]string{"title", "Assignee"})
	for _, want := range []string{"field:value", "field!:value", "field=value", "field!=value", "field^value", "field~regex",
		"field=", "a,b", "-text", `"two words"`, "title, assignee", "what they mean"} {
		if !strings.Contains(h, want) {
			t.Errorf("help missing %q:\n%s", want, h)
		}
	}
	if n := strings.Count(h, "\n"); n > 20 {
		t.Errorf("help is %d lines; it must fit a 24-row terminal page", n)
	}
	if !strings.Contains(h, "none") {
		t.Error("help must say that a,b with !: / != means none of them")
	}
	for _, line := range strings.Split(h, "\n") {
		if strings.HasPrefix(line, "  ") && len([]rune(line)) > 80 {
			t.Errorf("row wider than 80 columns (%d): %q", len([]rune(line)), line)
		}
	}
}

func TestOrderingComparesNumbersNumerically(t *testing.T) {
	tasks := []task.Task{
		{ID: "a", Extra: map[string]string{"points": "9"}},
		{ID: "b", Extra: map[string]string{"points": "10"}},
		{ID: "c", Extra: map[string]string{"points": "2.5"}},
		{ID: "d", Extra: map[string]string{"points": "n/a"}}, // text vs number falls back to string order
	}
	cases := map[string]string{
		"points>5":    "a,b,d", // "n/a" > "5" as text
		"points<10":   "a,c",
		"points>=9.0": "a,b,d",
		"points<=2.5": "c",
	}
	for q, want := range cases {
		p, err := Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tk := range tasks {
			if p.Match(TaskLookup(tk, "", board.Deps{}), nil) {
				got = append(got, tk.ID)
			}
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%q: got %v want %s", q, got, want)
		}
	}
}

func TestOrAlternatives(t *testing.T) {
	cases := map[string]string{
		"priority=high OR type=feature":            "1,2",
		"priority=high type=feature OR title:docs": "A-3", // AND binds tighter than OR
		"priority=high | priority=medium":          "1,2",
		`"OR"`:                                     "",
		"title:fix OR title:bot OR title:docs":     "1,2,A-3",
	}
	for q, want := range cases {
		if got := ids(t, q, nil); got != want {
			t.Errorf("%q: got %q want %q", q, got, want)
		}
	}
	for _, bad := range []string{"OR", "OR title:x", "title:x OR", "title:x OR OR title:y"} {
		if _, err := Parse(bad); err == nil || !strings.Contains(err.Error(), "OR needs a term") && !strings.Contains(err.Error(), "needs a term before") {
			t.Errorf("%q: got %v", bad, err)
		}
	}
	if p, _ := Parse(""); !p.Empty() {
		t.Error("empty query must be Empty")
	}
}

func TestRelativeDates(t *testing.T) {
	old := Now
	Now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }
	defer func() { Now = old }()
	// sample dues: 1 = 2026-10-20, 2 = 2026-11-05, 3 = none
	cases := map[string]string{
		"due=today":      "1",
		"due<tomorrow":   "1",
		"due>yesterday":  "1,2",
		"due<today+7d":   "1",
		"due<=today+16d": "1,2",
		"due<today+3w":   "1,2",
		"due>=today-1w":  "1,2",
		"due<today+1m":   "1,2", // 2026-11-20
		"due<today+1w":   "1",
		"due>today-1y":   "1,2",
		"due<today+1y":   "1,2",
		"due:today":      "1",
		"due^today":      "1",
		"due!:today":     "2,A-3",
		"due:today+16d":  "2",
	}
	for q, want := range cases {
		if got := ids(t, q, nil); got != want {
			t.Errorf("%q: got %q want %q", q, got, want)
		}
	}
}

func TestSuggestAndUnknownFieldHints(t *testing.T) {
	known := []string{"id", "title", "assignee", "priority", "story_points"}
	for in, want := range map[string]string{"titel": "title", "asignee": "assignee", "Priorty": "priority", "story_point": "story_points", "zzz": "", "x": ""} {
		if got := Suggest(in, known); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", in, got, want)
		}
	}
	p, _ := Parse("asignee=ann")
	if err := p.Check(known); err == nil || !strings.Contains(err.Error(), `did you mean "assignee"`) {
		t.Fatalf("query hint: %v", err)
	}
	if err := CheckSearch([]string{"titel"}, known); err == nil || !strings.Contains(err.Error(), `did you mean "title"`) {
		t.Fatalf("search hint: %v", err)
	}
}

func TestMalformedRelativeDatesAreErrors(t *testing.T) {
	for _, q := range []string{"due<today+7", "due<today+1x", "due=tomorrow-", "due<tomorow", "start>=yesterdy", "due<todya+1d"} {
		if _, err := Parse(q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
	// Plain text that merely looks similar stays text.
	for _, q := range []string{"status=todo", "title=today's", "title:tomorow", "status<todo", "due<2026-12-01", "due=today", "due!=today+2d"} {
		if _, err := Parse(q); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	_, err := Parse("due<tomorow")
	if err == nil || !strings.Contains(err.Error(), `did you mean "tomorrow"`) {
		t.Errorf("no suggestion: %v", err)
	}
}

func TestLabelsMatchSingleLabels(t *testing.T) {
	tasks := []task.Task{
		{ID: "1", Labels: "ops,wip"},
		{ID: "2", Labels: "devops"},
		{ID: "3", Labels: " Ops , x "},
		{ID: "4"},
	}
	cases := map[string]string{
		"labels=ops":                  "1,3",
		"labels=OPS":                  "1,3",
		"labels!=ops":                 "2,4",
		"labels:ops":                  "1,2,3",
		"labels^op":                   "1,3",
		"labels^dev":                  "2",
		"labels=wip,x":                "1,3",
		"labels!=wip,x":               "2,4",
		"labels=":                     "4",
		"labels!=":                    "1,2,3",
		"labels=ops wip":              "",
		"labels=ops OR labels=devops": "1,2,3",
	}
	for q, want := range cases {
		p, err := Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		var got []string
		for _, tk := range tasks {
			if p.Match(TaskLookup(tk, "", board.Deps{}), nil) {
				got = append(got, tk.ID)
			}
		}
		if g := strings.Join(got, ","); g != want {
			t.Errorf("%q = %q, want %q", q, g, want)
		}
	}
}

func TestBlockedAndReady(t *testing.T) {
	b, err := board.New(board.DefaultSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := []task.Task{
		{ID: "1", Status: "done"},
		{ID: "2", Status: "pending", BlockedBy: "1"},
		{ID: "3", Status: "pending", BlockedBy: "2"},
		{ID: "4", Status: "pending", BlockedBy: "7"},
	}
	deps := b.Deps(tasks)
	cases := map[string]string{
		"blocked=yes":  "3",
		"ready=yes":    "1,2,4",
		"ready=no":     "3",
		"blocked_by=":  "1",
		"blocked_by:2": "3",
		"-x ready!=no": "1,2,4",
	}
	for q, want := range cases {
		p, err := Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tk := range tasks {
			if p.Match(TaskLookup(tk, "", deps), nil) {
				got = append(got, tk.ID)
			}
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%q: got %v want %s", q, got, want)
		}
	}
	// without the task list blocked and ready are unknown: empty
	if v, ok := TaskLookup(tasks[2], "", board.Deps{})("blocked"); ok || v != "" {
		t.Errorf("blocked without Deps: %q %v", v, ok)
	}
	// running is supplied by the board; TaskLookup leaves it unknown
	if v, ok := TaskLookup(tasks[2], "", board.Deps{})(RunningField); ok || v != "" {
		t.Errorf("running from TaskLookup: %q %v", v, ok)
	}
}

// today, tomorrow and yesterday are dates only in a field that holds dates: a
// label, a type or a title of that name is text.
func TestRelativeWordsAreTextOutsideDateFields(t *testing.T) {
	old := Now
	Now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }
	defer func() { Now = old }()
	tk := task.Task{ID: "9", Title: "today", Type: "tomorrow", Labels: "yesterday,x", Due: "2026-10-20"}
	match := func(q string) bool {
		p, err := Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		return p.Match(TaskLookup(tk, "Col", board.Deps{}), nil)
	}
	for q, want := range map[string]bool{
		"title=today":       true,
		"type=tomorrow":     true,
		"labels=yesterday":  true,
		"title!=today":      false,
		"labels!=yesterday": false,
		"due=today":         true,
		"due!=today":        false,
		"due=2026-10-20":    true,
		"title=2026-10-20":  false,
		"labels=2026-10-20": false,
		"title>today":       false, // text order, not 2026-10-20
		"title>=today":      true,
		"type<=tomorrow":    true,
	} {
		if got := match(q); got != want {
			t.Errorf("%q: got %v want %v", q, got, want)
		}
	}
}

func TestRelativeMonthsAndYearsStopAtTheEndOfTheMonth(t *testing.T) {
	old := Now
	defer func() { Now = old }()
	for _, c := range []struct{ now, expr, want string }{
		{"2026-01-31", "today+1m", "2026-02-28"},
		{"2026-03-31", "today-1m", "2026-02-28"},
		{"2024-02-29", "today+1y", "2025-02-28"},
		{"2026-10-20", "today+1m", "2026-11-20"},
	} {
		d, _ := time.Parse("2006-01-02", c.now)
		Now = func() time.Time { return d }
		if got := expandDates([]string{c.expr})[0]; got != c.want {
			t.Errorf("%s %s = %s, want %s", c.now, c.expr, got, c.want)
		}
	}
}

// A greater priority is a more urgent one, whatever the letters of the words.
func TestPriorityOrdersByUrgency(t *testing.T) {
	cases := map[string]string{
		"priority>=high":     "1",
		"priority>medium":    "1",
		"priority<=medium":   "2",
		"priority<high":      "2",
		"priority<=high":     "1,2",
		"priority>low":       "1,2",
		"priority>=critical": "",
		"priority>=p1":       "1",
		"priority<=2":        "1,2", // a bare number ranks like its word: 2 is high
		"priority>=URGENT":   "",
		"priority<trivial":   "",
		"priority>trivial":   "1,2",
		"priority>=high,low": "1,2", // any of the values
	}
	for q, want := range cases {
		if got := ids(t, q, nil); got != want {
			t.Errorf("%q: got %q want %q", q, got, want)
		}
	}
	for _, q := range []string{"priority>hgh", "priority<=soon"} {
		if _, err := Parse(q); err == nil || !strings.Contains(err.Error(), "unknown priority") {
			t.Errorf("%q: %v", q, err)
		}
	}
	// = and ! keep comparing the words.
	if got := ids(t, "priority=high", nil); got != "1" {
		t.Errorf("priority=high: %q", got)
	}
}

func TestTokenizeQuoting(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{`a b`, []string{"a", "b"}},
		{"a\tb", []string{"a", "b"}},
		{`"a b" c`, []string{`"a b"`, "c"}},
		{`title:"a b"`, []string{`title:"a b"`}},
		{`"say \"hi\" now"`, []string{`"say \"hi\" now"`}},
		{`title~"a b|c d"`, []string{`title~"a b|c d"`}},
		{`""`, []string{`""`}},
		{`a "" b`, []string{"a", `""`, "b"}},
		{`\"a b"`, []string{`\"a b"`}}, // outside quotes \" is no escape: the " opens a quote
	} {
		got, err := tokenize(c.in)
		if err != nil || strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("tokenize(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := tokenize(`"open`); err == nil {
		t.Error("an unterminated quote must be an error")
	}
	if _, err := tokenize(`"open \"`); err == nil {
		t.Error("an escaped closing quote does not close the quote")
	}
}

func TestUnquote(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:        "plain",
		`"a b"`:        "a b",
		`a"b c"d`:      "ab cd",
		`say \"hi\"`:   `say "hi"`,
		`"say \"hi\""`: `say "hi"`,
		`""`:           "",
		`back\slash`:   `back\slash`,
		`"a,b"`:        "a,b",
	} {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuotingInTermsAndRegexes(t *testing.T) {
	tasks := []task.Task{
		{ID: "1", Title: `say "hi" now`},
		{ID: "2", Title: "a b|c d"},
		{ID: "3", Title: "a,b and c"},
		{ID: "4", Title: "plain", Priority: "high"},
	}
	match := func(q string) (string, error) {
		p, err := Parse(q)
		if err != nil {
			return "", err
		}
		var out []string
		for _, tk := range tasks {
			if p.Match(TaskLookup(tk, "", board.Deps{}), nil) {
				out = append(out, tk.ID)
			}
		}
		return strings.Join(out, ","), nil
	}
	for q, want := range map[string]string{
		`title:"say \"hi\""`:    "1", // escaped quote inside quotes is a literal quote
		`"\"hi\""`:              "1", // plain text, quoted
		`title~"\"hi\""`:        "1", // regex: \" becomes a literal quote
		`title~"a b|c d"`:       "2", // a regex with spaces and an alternative needs quotes
		`title~a|plain`:         "1,2,3,4",
		`title~"a,b|plain"`:     "3,4",     // commas are not split in a regex: this one is literal
		`title~plain,high`:      "",        // "plain,high" is one pattern
		`title:"a,b"`:           "3",       // a quoted comma is part of the value
		`title:a,plain`:         "1,2,3,4", // unquoted: any of "a", "plain"
		`title=plain,"a b|c d"`: "2,4",
		`-"a b"`:                "1,3,4",
	} {
		got, err := match(q)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", q, got, err, want)
		}
	}
	for _, q := range []string{`""`, `-""`, `title~""`, `a OR ""`} {
		if _, err := match(q); err == nil {
			t.Errorf("%s: want an error", q)
		}
	}
}
