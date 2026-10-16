package task

import (
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	ok := []Task{{Title: "x"}, {Title: "x", Due: "2026-02-28", Start: "2026-01-01"}}
	for _, tk := range ok {
		if err := tk.Validate(); err != nil {
			t.Errorf("%+v: %v", tk, err)
		}
	}
	ok = append(ok, Task{Title: "x", Due: " 2026-01-01 "}, Task{Title: "x", Start: "2026-05-01", Due: "2026-05-01"})
	bad := []Task{{}, {Title: "  "}, {Title: "x", Due: "tomorrow"}, {Title: "x", Start: "2026-13-01"}, {Title: "x", Due: "2026-02-30"}, {Title: "x", Start: "2026-06-01", Due: "2026-05-01"}}
	for _, tk := range bad {
		if tk.Validate() == nil {
			t.Errorf("%+v accepted", tk)
		}
	}
}

func TestLabelList(t *testing.T) {
	got := Task{Labels: " a, b ,,c,"}.LabelList()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("%q", got)
	}
	if got := (Task{}).LabelList(); len(got) != 0 {
		t.Fatalf("empty labels: %q", got)
	}
}

func TestDueTimeAndOverdue(t *testing.T) {
	now := time.Date(2026, 10, 16, 15, 30, 0, 0, time.Local)
	cases := []struct {
		due     string
		ok      bool
		overdue bool
	}{
		{"2026-10-15", true, true},
		{"2026-10-16", true, false}, // due today is not overdue
		{"2026-10-17", true, false},
		{"", false, false},
		{"soon", false, false},
	}
	for _, c := range cases {
		tk := Task{Due: c.due}
		if _, ok := tk.DueTime(); ok != c.ok {
			t.Errorf("%q: ok=%v", c.due, ok)
		}
		if got := tk.Overdue(now); got != c.overdue {
			t.Errorf("%q: overdue=%v, want %v", c.due, got, c.overdue)
		}
	}
	if d, ok := (Task{Start: "2026-01-02"}).StartTime(); !ok || d.Day() != 2 {
		t.Errorf("StartTime: %v %v", d, ok)
	}
}

func TestValidateIsDeterministicAndTrims(t *testing.T) {
	// both dates are wrong: the message always names due first
	for i := 0; i < 50; i++ {
		err := Task{Title: "x", Due: "soon", Start: "later"}.Validate()
		if err == nil || !strings.HasPrefix(err.Error(), `due "soon"`) {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	// what Validate accepts, DueTime parses (and the reverse)
	for _, d := range []string{" 2026-01-01", "2026-01-01 ", "2026-1-1", "01/02/2026"} {
		_, parsed := Task{Due: d}.DueTime()
		valid := Task{Title: "x", Due: d}.Validate() == nil
		if parsed != valid {
			t.Errorf("%q: DueTime ok=%v but Validate ok=%v", d, parsed, valid)
		}
	}
	if err := (Task{Title: "x", Start: "2026-06-01", Due: "2026-05-01"}).Validate(); err == nil || !strings.Contains(err.Error(), "after due") {
		t.Errorf("start after due: %v", err)
	}
}

func TestLabelListDedupesAndHasLabel(t *testing.T) {
	tk := Task{Labels: "UI, api ,ui,,Api, ops"}
	got := tk.LabelList()
	if strings.Join(got, "|") != "UI|api|ops" {
		t.Fatalf("%q", got)
	}
	for _, l := range []string{"ui", " API ", "Ops"} {
		if !tk.HasLabel(l) {
			t.Errorf("HasLabel(%q) = false", l)
		}
	}
	for _, l := range []string{"u", "op", "", "web"} {
		if tk.HasLabel(l) {
			t.Errorf("HasLabel(%q) = true (whole labels only)", l)
		}
	}
}

func TestNormalizeAndPriorityRank(t *testing.T) {
	if Normalize(" Fix ") != "fix" || Normalize("FIX") != Normalize("fix") {
		t.Error("Normalize")
	}
	order := []string{"critical", "URGENT", "P0", "High", "p1", "Medium", "normal", "p2", "low", "Minor", "p3", "trivial", "lowest", "p4"}
	prev := -1
	for _, p := range order {
		r := PriorityRank(p)
		if r == UnknownPriority || r < prev {
			t.Errorf("%q ranks %d after %d", p, r, prev)
		}
		prev = r
	}
	for p, want := range map[string]int{"1": 0, "2": 1, "3": 2, "4": 3, "5": 4, "6": UnknownPriority, "": UnknownPriority, "whatever": UnknownPriority} {
		if got := PriorityRank(p); got != want {
			t.Errorf("PriorityRank(%q) = %d, want %d", p, got, want)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	orig := Task{ID: "1", Title: "a", Extra: map[string]string{"k": "v"}}
	c := orig.Clone()
	c.Extra["k"] = "changed"
	c.Extra["new"] = "x"
	c.Title = "b"
	if orig.Extra["k"] != "v" || len(orig.Extra) != 1 || orig.Title != "a" {
		t.Fatalf("clone shares state with the original: %+v", orig)
	}
	if (Task{}).Clone().Extra != nil {
		t.Error("cloning a task without Extra must not invent a map")
	}
}
