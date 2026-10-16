package board

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSortKeysParse(t *testing.T) {
	var s Spec
	y := `
sort:
  - priority: [high, medium, low]
  - -id
  - "Story Points:3, 2,1"
  - -col
  - {-title: [b, a]}
`
	if err := yaml.Unmarshal([]byte(y), &s); err != nil {
		t.Fatal(err)
	}
	want := []SortKey{
		{Field: "priority", Values: []string{"high", "medium", "low"}},
		{Field: "id", Desc: true},
		{Field: "story_points", Values: []string{"3", "2", "1"}},
		{Field: "column", Desc: true},
		{Field: "title", Desc: true, Values: []string{"b", "a"}},
	}
	if !reflect.DeepEqual(s.Sort, want) {
		t.Fatalf("got %+v", s.Sort)
	}
	if got := s.Sort[0].String(); got != "priority:high,medium,low" {
		t.Fatalf("String: %s", got)
	}
}

func TestSortKeysRejectBadInput(t *testing.T) {
	for y, want := range map[string]string{
		"sort: [{a: [x], b: [y]}]":   "exactly one entry",
		"sort: [[a]]":                "sort key is",
		"sort: [priority, priority]": "twice",
		"sort: ['-']":                "empty",
		"sort: ['p:a,']":             "empty value",
	} {
		var s Spec
		err := yaml.Unmarshal([]byte(y), &s)
		if err == nil {
			err = s.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", y, err, want)
		}
	}
}

func TestColumnSortParsesAndValidates(t *testing.T) {
	var s Spec
	y := "columns:\n  - {name: Done, sort: [-id]}\n  - {name: Todo}\nsort: [priority]\n"
	if err := yaml.Unmarshal([]byte(y), &s); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := New(s, nil)
	if got := b.Columns[0].Sort; len(got) != 1 || got[0].Field != "id" || !got[0].Desc {
		t.Fatalf("column sort not carried: %+v", got)
	}
	if len(b.Columns[1].Sort) != 0 {
		t.Fatal("column without sort must inherit board.sort")
	}
	s.Columns[0].Sort = []SortKey{{Field: "id"}, {Field: "id"}}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "board.columns[0].sort") {
		t.Fatalf("duplicate field in column sort: %v", err)
	}
}
