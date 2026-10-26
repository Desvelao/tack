package query

import (
	"sort"
	"strings"
	"testing"

	"tack/internal/board"
	"tack/internal/task"
)

func order(tasks []task.Task, keys ...board.SortKey) string {
	ts := append([]task.Task(nil), tasks...)
	sort.SliceStable(ts, func(i, j int) bool {
		return CompareBy(TaskLookup(ts[i], "", board.Deps{}), TaskLookup(ts[j], "", board.Deps{}), keys) < 0
	})
	var ids []string
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return strings.Join(ids, ",")
}

func TestCompareBy(t *testing.T) {
	tasks := []task.Task{
		{ID: "10", Title: "b", Priority: "low"},
		{ID: "2", Title: "A", Priority: "high"},
		{ID: "A-10", Title: "c"},
		{ID: "A-9", Title: "d", Priority: "urgent"},
		{ID: "1", Title: "e", Priority: "HIGH", Extra: map[string]string{"story points": "5"}},
	}
	prio := board.SortKey{Field: "priority", Values: []string{"high", "medium", "low"}}
	for name, c := range map[string]struct {
		keys []board.SortKey
		want string
	}{
		"natural id":                    {[]board.SortKey{{Field: "id"}}, "1,2,10,A-9,A-10"},
		"descending id":                 {[]board.SortKey{{Field: "id", Desc: true}}, "A-10,A-9,10,2,1"},
		"title ignores case":            {[]board.SortKey{{Field: "title"}}, "2,10,A-10,A-9,1"},
		"explicit order, unlisted next": {[]board.SortKey{prio}, "2,1,10,A-9,A-10"},
		"explicit then tiebreak":        {[]board.SortKey{prio, {Field: "id", Desc: true}}, "2,1,10,A-9,A-10"},
		"empty last when descending":    {[]board.SortKey{{Field: "priority", Desc: true, Values: prio.Values}}, "A-9,10,2,1,A-10"},
		"extra column":                  {[]board.SortKey{{Field: "story_points"}}, "1,10,2,A-10,A-9"},
	} {
		if got := order(tasks, c.keys...); got != c.want {
			t.Errorf("%s: got %s want %s", name, got, c.want)
		}
	}
	// No keys: nothing is reordered.
	if got := order(tasks); got != "10,2,A-10,A-9,1" {
		t.Errorf("no keys: %s", got)
	}
}

func TestCheckSort(t *testing.T) {
	known := KnownFields(nil)
	if err := CheckSort([]board.SortKey{{Field: "priority"}, {Field: "backend"}}, known); err != nil {
		t.Fatal(err)
	}
	err := CheckSort([]board.SortKey{{Field: "nope"}}, known)
	if err == nil || !strings.HasPrefix(err.Error(), "board.sort: unknown field") {
		t.Fatalf("got %v", err)
	}
}

func TestMultiKeySortWithValueListThenDescending(t *testing.T) {
	tasks := []task.Task{
		{ID: "1", Priority: "low", Title: "a"},
		{ID: "2", Priority: "high", Title: "b"},
		{ID: "3", Priority: "high", Title: "c"},
		{ID: "4", Priority: "low", Title: ""}, // empty second key
		{ID: "5", Priority: "high", Title: ""},
	}
	keys := []board.SortKey{{Field: "priority", Values: []string{"high", "low"}}, {Field: "title", Desc: true}}
	// high first; within a priority titles descending (c before b); empty titles last despite Desc
	if got := order(tasks, keys...); got != "3,2,5,1,4" {
		t.Fatalf("got %s", got)
	}
}

func TestPrioritySortsByUrgencyUnlessAnOrderIsGiven(t *testing.T) {
	tasks := []task.Task{
		{ID: "1", Priority: "low"}, {ID: "2", Priority: "High"}, {ID: "3", Priority: "medium"},
		{ID: "4", Priority: "critical"}, {ID: "5"}, {ID: "6", Priority: "weird"}, {ID: "7", Priority: "P2"},
	}
	// urgency order, unknown values after the known ones, empty last
	if got := order(tasks, board.SortKey{Field: "priority"}); got != "4,2,3,7,1,6,5" {
		t.Fatalf("ascending: %s", got)
	}
	if got := order(tasks, board.SortKey{Field: "priority", Desc: true}); got != "6,1,7,3,2,4,5" {
		t.Fatalf("descending: %s", got)
	}
	// an explicit list still wins
	if got := order(tasks, board.SortKey{Field: "priority", Values: []string{"low", "high"}}); !strings.HasPrefix(got, "1,2,") {
		t.Fatalf("explicit order ignored: %s", got)
	}
}
