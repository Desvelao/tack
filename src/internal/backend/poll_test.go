package backend

import (
	"strings"
	"testing"
	"time"
)

func TestParsePoll(t *testing.T) {
	ok := map[string]struct {
		in   any
		want PollMode
	}{
		"nil":         {nil, PollMode{}},
		"empty":       {"", PollMode{}},
		"watch":       {"watch", PollMode{}},
		"WATCH ":      {" WATCH ", PollMode{}},
		"none":        {"none", PollMode{Kind: PollNone}},
		"int":         {5, PollMode{PollEvery, 5 * time.Second}},
		"numeric str": {"7", PollMode{PollEvery, 7 * time.Second}},
		"seconds":     {"5s", PollMode{PollEvery, 5 * time.Second}},
		"minutes":     {"1m30s", PollMode{PollEvery, 90 * time.Second}},
		"millis":      {"250ms", PollMode{PollEvery, 250 * time.Millisecond}},
	}
	for name, c := range ok {
		got, err := ParsePoll("csv", c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, c.want)
		}
	}
	for name, in := range map[string]any{
		"zero int":     0,
		"negative int": -3,
		"zero str":     "0",
		"negative dur": "-1s",
		"zero dur":     "0s",
		"word":         "often",
		"float":        1.5,
		"bool":         true,
		"list":         []any{1},
	} {
		_, err := ParsePoll("json", in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "json options: poll must be") {
			t.Errorf("%s: message %q", name, err)
		}
	}
}
