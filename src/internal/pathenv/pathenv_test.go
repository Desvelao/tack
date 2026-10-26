package pathenv

import (
	"os"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	t.Setenv("PE_ROOT", "/srv/w")
	t.Setenv("PE_EMPTY", "")
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"$PE_ROOT/x":    "/srv/w/x",
		"${PE_ROOT}x":   "/srv/wx",
		"a/$PE_EMPTY/b": "a//b",
		"cost$$":        "cost$",
		"~/wt":          home + "/wt",
		"~":             home,
		"~user/x":       "~user/x",
		"plain/{id}":    "plain/{id}",
	} {
		if got, err := Expand(in); err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Expand("$PE_NOPE_UNSET/x"); err == nil || !strings.Contains(err.Error(), "$PE_NOPE_UNSET is not set") {
		t.Errorf("unset: %v", err)
	}
}
