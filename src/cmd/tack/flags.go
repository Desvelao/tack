package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"tack/internal/task"
)

// takeConfigFlag removes a `-c FILE` / `-c=FILE` (or --c) from a subcommand's
// arguments, ignoring anything after `--` and the values of the command's own
// flags (so `add -title -c` keeps "-c" as the title). ok is false when there
// is none.
func takeConfigFlag(cmd command, args []string) (path string, left []string, ok bool) {
	var valued map[string]bool // flags of cmd that take a value; loaded lazily
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name := strings.TrimLeft(a, "-")
		if dashes := len(a) - len(name); dashes < 1 || dashes > 2 {
			continue
		}
		switch {
		case name == "c" && i+1 < len(args):
			path, ok = args[i+1], true
			left = append(append([]string{}, args[:i]...), args[i+2:]...)
			return
		case strings.HasPrefix(name, "c="):
			path, ok = name[2:], true
			left = append(append([]string{}, args[:i]...), args[i+1:]...)
			return
		case strings.Contains(name, "="): // -flag=value carries its value
		default:
			if valued == nil {
				valued = valueFlags(cmd)
			}
			if valued[name] {
				i++ // the next token is this flag's value, whatever it looks like
			}
		}
	}
	return "", args, false
}

// stdin is where `-description -` / `-notes -` read from (replaced in tests).
var stdin io.Reader = os.Stdin

// readText returns the text for a long field: src "-" reads stdin (once per
// command), anything else is a file path. One trailing newline is dropped.
func readText(src string, used *bool) (string, error) {
	var data []byte
	var err error
	if src == "-" {
		if *used {
			return "", errors.New("only one of -description / -notes can read from stdin (-)")
		}
		*used = true
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), nil
}

// listFlag is a repeatable, comma-separated string flag (-add-label a,b -add-label c).
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// parseNoArgs parses a subcommand that takes flags only. ok is false when the
// caller should return code: -h exits 0, bad flags or stray positionals exit 2.
func parseNoArgs(fs *flag.FlagSet, args []string, errw io.Writer) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errw, "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return exitUsage, false
	}
	return 0, true
}

// checkDate reports an error unless v is empty or YYYY-MM-DD.
func checkDate(name, v string) error {
	if _, err := time.Parse("2006-01-02", v); v != "" && err != nil {
		return fmt.Errorf("-%s must be YYYY-MM-DD", name)
	}
	return nil
}

// parseArgs parses flags that may appear before or after positional arguments
// and returns the positionals.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		// After `--` everything is positional, even if it looks like a flag.
		if i := len(args) - fs.NArg() - 1; i >= 0 && args[i] == "--" {
			return append(pos, fs.Args()...), nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// fieldSpec describes one editable task field: its flag, the placeholder shown
// in usage lines and where it lives in a Task. add, edit and their usage text
// all derive from taskFields, so a new field is added in one place.
type fieldSpec struct {
	name, meta, help string
	ptr              func(*task.Task) *string
}

var taskFields = []fieldSpec{
	{"title", "T", "task title (required for add)", func(t *task.Task) *string { return &t.Title }},
	{"description", "D", "description (- reads stdin)", func(t *task.Task) *string { return &t.Description }},
	{"priority", "P", "priority", func(t *task.Task) *string { return &t.Priority }},
	{"type", "Y", "type", func(t *task.Task) *string { return &t.Type }},
	{"notes", "N", "notes (- reads stdin)", func(t *task.Task) *string { return &t.Notes }},
	{"assignee", "A", "assignee", func(t *task.Task) *string { return &t.Assignee }},
	{"due", "DATE", "due date, YYYY-MM-DD", func(t *task.Task) *string { return &t.Due }},
	{"start", "DATE", "start date, YYYY-MM-DD", func(t *task.Task) *string { return &t.Start }},
	{"labels", "L", "comma-separated labels", func(t *task.Task) *string { return &t.Labels }},
	{"blocked-by", "IDS", "ids (same backend) of tasks to finish first, comma-separated", func(t *task.Task) *string { return &t.BlockedBy }},
}

// fieldUsage renders the field flags for a usage line, e.g.
// "[-title T] [-description D|-|-description-file F] ...".
func fieldUsage() string {
	parts := make([]string, len(taskFields))
	for i, f := range taskFields {
		p := "-" + f.name + " " + f.meta
		if f.name == "description" || f.name == "notes" {
			p += "|-|-" + f.name + "-file F"
		}
		parts[i] = "[" + p + "]"
	}
	return strings.Join(parts, " ")
}
