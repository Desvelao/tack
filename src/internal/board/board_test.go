package board

import (
	"reflect"
	"strings"
	"testing"

	"tkan/internal/task"
)

func spec() Spec {
	return Spec{Columns: []ColumnSpec{
		{Name: "Backlog", Values: []string{"pending", "todo", ""}},
		{Name: "Doing", Values: []string{"in_progress", "WIP"}, Write: "in_progress"},
		{Name: "Done", Values: []string{"done"}},
	}}
}

func TestOrderMappingAndWrite(t *testing.T) {
	b, err := New(spec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.Names(), []string{"Backlog", "Doing", "Done"}) {
		t.Fatalf("names: %v", b.Names())
	}
	for raw, want := range map[string]int{"pending": 0, "TODO": 0, "": 0, "wip": 1, "In_Progress": 1, "done": 2, "x": -1} {
		if got := b.ColumnOf(raw); got != want {
			t.Errorf("ColumnOf(%q)=%d want %d", raw, got, want)
		}
	}
	if b.WriteValue(0) != "pending" || b.WriteValue(1) != "in_progress" || b.WriteValue(2) != "done" {
		t.Fatalf("write values: %q %q %q", b.WriteValue(0), b.WriteValue(1), b.WriteValue(2))
	}
}

func TestValuesDefaultToName(t *testing.T) {
	b, _ := New(Spec{Columns: []ColumnSpec{{Name: "Review"}}}, nil)
	if b.ColumnOf("review") != 0 || b.WriteValue(0) != "Review" {
		t.Fatalf("%+v", b.Columns)
	}
}

func TestUnmappedModes(t *testing.T) {
	present := []string{"pending", "blocked", "", "qa"}

	s := spec()
	b, _ := New(s, present)
	if got := b.Names(); !reflect.DeepEqual(got[3:], []string{"blocked", "qa"}) {
		// "" is mapped by Backlog, so only blocked and qa are extras
		t.Fatalf("append: %v", got)
	}
	if !b.Columns[3].Extra {
		t.Fatal("extra flag")
	}

	s.Unmapped = Hide
	b, _ = New(s, present)
	if len(b.Columns) != 3 || b.ColumnOf("blocked") != -1 {
		t.Fatalf("hide: %v", b.Names())
	}

	s.Unmapped = Other
	b, _ = New(s, present)
	if len(b.Columns) != 4 || b.Columns[3].Name != "other" || b.ColumnOf("qa") != 3 || b.ColumnOf("blocked") != 3 {
		t.Fatalf("other: %+v", b.Columns)
	}
}

func TestEmptyValueNamedNone(t *testing.T) {
	b, _ := New(Spec{Columns: []ColumnSpec{{Name: "a"}}}, []string{""})
	if b.Columns[1].Name != NoneName || b.ColumnOf("") != 1 {
		t.Fatalf("%+v", b.Columns)
	}
}

func TestLookupPrefersNameThenValue(t *testing.T) {
	b, _ := New(spec(), nil)
	m, ok := b.Lookup("doing")
	if !ok || !m.Name || m.Col != 1 || m.Raw != "in_progress" {
		t.Fatalf("%+v", m)
	}
	m, ok = b.Lookup("wip")
	if !ok || m.Name || m.Col != 1 || m.Raw != "WIP" {
		t.Fatalf("%+v", m)
	}
	if _, ok := b.Lookup("nope"); ok {
		t.Fatal("unknown should not resolve")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]Spec{
		"duplicate column":               {Columns: []ColumnSpec{{Name: "a"}, {Name: "A"}}},
		"mapped to both":                 {Columns: []ColumnSpec{{Name: "a", Values: []string{"x"}}, {Name: "b", Values: []string{"X"}}}},
		"name is required":               {Columns: []ColumnSpec{{Name: " "}}},
		"unmapped":                       {Unmapped: "nope"},
		"board.agents[1]: name is empty": {Agents: []string{"claude", " "}},
		`write "inprogress" is not one of its values`: {Columns: []ColumnSpec{{Name: "Doing", Values: []string{"in_progress", "wip"}, Write: "inprogress"}}},
	}
	for want, s := range cases {
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestEmptySpecUsesDefaults(t *testing.T) {
	b, err := New(Spec{}, nil)
	if err != nil || len(b.Columns) != 4 || b.Columns[0].Name != "pending" {
		t.Fatalf("%v %+v", err, b.Columns)
	}
}

func TestSearchValidation(t *testing.T) {
	if err := (Spec{Search: []string{"title", " "}}).Validate(); err == nil || !strings.Contains(err.Error(), "board.search") {
		t.Fatalf("got %v", err)
	}
	if err := (Spec{Search: []string{"title", "assignee"}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedColumnNamesNeverShadowConfigured(t *testing.T) {
	// "backlog" is a raw value no column claims, but Backlog is a column name.
	b, _ := New(spec(), []string{"pending", "backlog"})
	if got := b.Names()[3]; got != "backlog*" {
		t.Fatalf("extra column named %q, want backlog*", got)
	}
	m, ok := b.Lookup("backlog*")
	if !ok || m.Raw != "backlog" {
		t.Fatalf("extra not reachable by name: %+v %v", m, ok)
	}

	s := spec()
	s.Columns = append(s.Columns, ColumnSpec{Name: "other", Values: []string{"x"}})
	s.Unmapped = Other
	b, _ = New(s, []string{"qa"})
	if got := b.Names()[len(b.Columns)-1]; got != "other*" {
		t.Fatalf("catch-all named %q, want other*", got)
	}
}

func TestWriteMustBeOneOfTheValues(t *testing.T) {
	ok := Spec{Columns: []ColumnSpec{
		{Name: "Doing", Values: []string{"in_progress", "WIP"}, Write: "wip"}, // case-insensitive
		{Name: "Done", Write: "done"},                                         // values default to the name
	}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestColumnLimit(t *testing.T) {
	s := Spec{Columns: []ColumnSpec{{Name: "Doing", Limit: 2}, {Name: "Done"}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := New(s, nil)
	if lim, over := b.Over(0, 2); lim != 2 || over {
		t.Fatalf("2 cards in limit 2: %d %v", lim, over)
	}
	if _, over := b.Over(0, 3); !over {
		t.Fatal("3 cards must exceed limit 2")
	}
	if _, over := b.Over(1, 99); over {
		t.Fatal("a column without a limit is never over")
	}
	bad := Spec{Columns: []ColumnSpec{{Name: "x", Limit: -1}}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("negative limit accepted: %v", err)
	}
}

func TestColumnColorValidation(t *testing.T) {
	for _, ok := range []string{"", "0", "75", "255", "#ff8800", "#ABCDEF"} {
		s := Spec{Columns: []ColumnSpec{{Name: "a", Color: ok}}}
		if err := s.Validate(); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"red", "256", "-1", "#fff", "#gggggg", "12x"} {
		s := Spec{Columns: []ColumnSpec{{Name: "a", Color: bad}}}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "color") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	b, _ := New(Spec{Columns: []ColumnSpec{{Name: "a", Color: " 75 "}}}, nil)
	if b.Columns[0].Color != "75" {
		t.Fatalf("color not carried/trimmed: %q", b.Columns[0].Color)
	}
}

func TestDuplicateValueMessagesAndWriteSpelling(t *testing.T) {
	dup := Spec{Columns: []ColumnSpec{{Name: "A", Values: []string{"x"}}, {Name: "Backlog", Values: []string{"Todo", " todo "}}}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "board.columns[1]") || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("same-column duplicate: %v", err)
	}
	two := Spec{Columns: []ColumnSpec{{Name: "A", Values: []string{"x"}}, {Name: "B", Values: []string{"X"}}}}
	if err := two.Validate(); err == nil || !strings.Contains(err.Error(), "board.columns[1]") || !strings.Contains(err.Error(), "mapped to both") {
		t.Fatalf("cross-column duplicate: %v", err)
	}
	b, err := New(Spec{Columns: []ColumnSpec{{Name: "Doing", Values: []string{"in_progress", "WIP"}, Write: "wip"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.WriteValue(0); got != "WIP" {
		t.Fatalf("write spelled %q, want the value's own spelling WIP", got)
	}
}

func TestDeps(t *testing.T) {
	b, err := New(DefaultSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := []task.Task{
		{ID: "1", Status: "done", Backend: "a"},
		{ID: "2", Status: "failed", Backend: "a"},
		{ID: "3", Status: "pending", Backend: "a", BlockedBy: "1"},
		{ID: "4", Status: "pending", Backend: "a", BlockedBy: " 2 , 3,3, 9,4"},
		{ID: "5", Status: "pending", Backend: "b", BlockedBy: "3"}, // ids refer to the own backend
	}
	d := b.Deps(tasks)
	if !d.Known() || (Deps{}).Known() {
		t.Fatal("Known")
	}
	for i, want := range []string{"", "", "", "2,3", ""} {
		if got := strings.Join(d.Blockers(tasks[i]), ","); got != want {
			t.Errorf("#%s: blockers %q, want %q (failed is not finished; unknown and own ids do not block)", tasks[i].ID, got, want)
		}
	}
	if !d.Blocked(tasks[3]) || d.Blocked(tasks[2]) || (Deps{}).Blocked(tasks[3]) {
		t.Error("Blocked")
	}
}
