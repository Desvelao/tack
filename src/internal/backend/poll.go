package backend

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PollKind says when a backend notices changes made to its store by others.
type PollKind int

const (
	PollWatch PollKind = iota // check the store on every Changed/List call (default)
	PollNone                  // read at start (and on Reload) only
	PollEvery                 // check the store at most once per interval
)

// PollMode is the parsed `poll` option shared by the backends that have one.
type PollMode struct {
	Kind     PollKind
	Interval time.Duration // PollEvery only
}

// ParsePoll reads options.poll: "watch", "none", a number of seconds or a
// duration like 5s. typ names the backend in the error message.
func ParsePoll(typ string, v any) (PollMode, error) {
	bad := func() (PollMode, error) {
		return PollMode{}, fmt.Errorf(`%s options: poll must be "watch", "none" or a number of seconds (e.g. 5 or 5s), got %v`, typ, v)
	}
	switch x := v.(type) {
	case nil:
		return PollMode{}, nil
	case int:
		if x < 1 {
			return bad()
		}
		return PollMode{PollEvery, time.Duration(x) * time.Second}, nil
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		switch s {
		case "watch", "":
			return PollMode{}, nil
		case "none":
			return PollMode{Kind: PollNone}, nil
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 {
			return PollMode{PollEvery, time.Duration(n) * time.Second}, nil
		}
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return PollMode{PollEvery, d}, nil
		}
	}
	return bad()
}
