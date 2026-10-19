package query

import (
	"strings"
	"testing"
)

// Every field a task filter understands has a meaning in the fields page.
func TestTaskFieldDocsCoverEveryField(t *testing.T) {
	docs := TaskFieldDocs([]string{"customer", "story_points"})
	have := map[string]FieldDoc{}
	for _, d := range docs {
		for _, n := range strings.Split(d.Name, ", ") {
			have[n] = d
		}
		if strings.TrimSpace(d.Desc) == "" {
			t.Errorf("%s has no description", d.Name)
		}
	}
	for _, f := range KnownFields(nil) {
		if _, ok := have[f]; !ok {
			t.Errorf("field %q has no entry in TaskFieldDocs", f)
		}
	}
	if have["column"].Alias != "col" {
		t.Errorf("column should list its alias col: %+v", have["column"])
	}
	var own string
	for _, d := range docs {
		own += d.Name + " " + d.Desc + "\n"
	}
	if !strings.Contains(own, "<your columns>") || !strings.Contains(own, "customer") || !strings.Contains(own, "story_points") {
		t.Errorf("own columns not described:\n%s", own)
	}
	for _, d := range TaskFieldDocs(KnownFields(nil)) {
		if d.Name == "<your columns>" {
			t.Errorf("built-in fields must not count as the file's own: %+v", d)
		}
	}
}

func TestFieldsHelpFitsAScreen(t *testing.T) {
	var many []string
	for i := 0; i < 12; i++ {
		many = append(many, "column_number_"+string(rune('a'+i)))
	}
	h := FieldsHelp("Filter fields", TaskFieldDocs(many), nil)
	if n := strings.Count(h, "\n"); n > 20 {
		t.Errorf("%d lines; with the page title and footer it must fit 24 rows", n)
	}
	for _, line := range strings.Split(h, "\n") {
		if len([]rune(line)) > 80 {
			t.Errorf("wider than 80 columns (%d): %q", len([]rune(line)), line)
		}
	}
	for _, want := range []string{"column (col)", "status", "raw stored value", "<your columns>", "(12 in all)", "A plain word"} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q:\n%s", want, h)
		}
	}
}
