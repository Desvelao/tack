package query

import "strings"

// Suggest returns the candidate closest to name (edit distance of at most 2,
// and less than the name's length), or "" when nothing is close. Comparison
// ignores case.
func Suggest(name string, candidates []string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	best, bestD := "", 3
	for _, c := range candidates {
		d := editDistance(n, strings.ToLower(c))
		if d < bestD && d < len([]rune(n)) {
			best, bestD = c, d
		}
	}
	return best
}

// DidYouMean formats the suggestion for an error message ("" when none).
func DidYouMean(name string, candidates []string) string {
	if s := Suggest(name, candidates); s != "" {
		return `; did you mean "` + s + `"?`
	}
	return ""
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
