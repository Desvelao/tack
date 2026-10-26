package query

import (
	"strings"
	"unicode"

	"tack/internal/board"
	"tack/internal/task"
)

// CompareBy orders two records by keys: negative when a sorts before b.
// Empty values sort last whatever the direction; ties return 0 so callers can
// keep the original order (use a stable sort).
func CompareBy(a, b Lookup, keys []board.SortKey) int {
	for _, k := range keys {
		av, _ := a(k.Field)
		bv, _ := b(k.Field)
		aEmpty, bEmpty := strings.TrimSpace(av) == "", strings.TrimSpace(bv) == ""
		switch {
		case aEmpty && bEmpty:
			continue
		case aEmpty:
			return 1
		case bEmpty:
			return -1
		}
		c := compareValues(av, bv, k.Values)
		if k.Field == "priority" && len(k.Values) == 0 { // urgency, not alphabet: critical < high < medium < low
			if ra, rb := task.PriorityRank(av), task.PriorityRank(bv); ra != rb {
				c = 1
				if ra < rb {
					c = -1
				}
			}
		}
		if k.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func compareValues(a, b string, order []string) int {
	if len(order) > 0 {
		ra, rb := rank(a, order), rank(b, order)
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		if ra < len(order) { // same listed value
			return 0
		}
	}
	return natural(a, b)
}

// rank is the position of v in order, or len(order) when unlisted.
func rank(v string, order []string) int {
	v = strings.ToLower(strings.TrimSpace(v))
	for i, o := range order {
		if strings.ToLower(strings.TrimSpace(o)) == v {
			return i
		}
	}
	return len(order)
}

// natural compares case-insensitively with digit runs compared as numbers
// ("2" < "10", "A-2" < "A-10").
func natural(a, b string) int {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if unicode.IsDigit(ra[i]) && unicode.IsDigit(rb[j]) {
			si, sj := i, j
			for i < len(ra) && unicode.IsDigit(ra[i]) {
				i++
			}
			for j < len(rb) && unicode.IsDigit(rb[j]) {
				j++
			}
			na, nb := strings.TrimLeft(string(ra[si:i]), "0"), strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) { // more digits = bigger, without overflow
				if len(na) < len(nb) {
					return -1
				}
				return 1
			}
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
			continue
		}
		if ra[i] != rb[j] {
			if ra[i] < rb[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case len(ra)-i < len(rb)-j:
		return -1
	case len(ra)-i > len(rb)-j:
		return 1
	}
	return 0
}

// CheckSort validates the sort fields against the known query fields.
func CheckSort(keys []board.SortKey, known []string) error {
	set := map[string]bool{}
	for _, k := range known {
		set[Normalize(k)] = true
	}
	for _, k := range keys {
		if !set[Normalize(k.Field)] {
			return &UnknownFieldError{Name: k.Field, Known: known, Prefix: "board.sort"}
		}
	}
	return nil
}
