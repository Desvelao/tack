// Package action runs user-configured commands on a task: hand it to an AI
// agent, send a notification, call a script. The command is an argv list (never
// a shell line), so task text can fill placeholders without ever being parsed
// as shell syntax.
package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"tack/internal/pathenv"
	"tack/internal/query"
	"tack/internal/task"
)

// What `append_notes:` of on_success / on_failure puts on the task's notes.
const (
	NotesStdout  = "stdout"  // the start of the run's stdout
	NotesSummary = "summary" // the one-line summary of the run (exit status, sizes)
	NotesError   = "error"   // why the run failed: the error, else the start of stderr
)

// Outcome is what a run asks for when it ends well (on_success) or badly
// (on_failure): a column or status to move the task to, what to append to its
// notes and/or a command to run. In YAML it is a scalar (just the status) or
// {status, append_notes, command}.
type Outcome struct {
	Status      string   `yaml:"status,omitempty"`
	AppendNotes string   `yaml:"append_notes,omitempty"`
	Command     []string `yaml:"command,omitempty"` // run after the move; {result} and {exit} join the task placeholders
}

// OutcomeExtra are the placeholders an outcome command has besides the task's.
var OutcomeExtra = []string{"result", "exit"}

// IsZero reports whether the outcome asks for nothing.
func (o Outcome) IsZero() bool {
	return o.Status == "" && o.AppendNotes == "" && len(o.Command) == 0
}

// CommandSpec is the command of o as a background action named name, with the
// settings it shares with s (workdir, env), ready to expand and run.
func (o Outcome) CommandSpec(name string, s Spec) Spec {
	c := Spec{Name: name, Command: o.Command, Workdir: s.Workdir, CreateWorkdir: s.CreateWorkdir, Env: s.Env}
	c.Normalize()
	return c
}

// UnmarshalYAML accepts a scalar (the status) or a mapping.
func (o *Outcome) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&o.Status)
	}
	type plain Outcome
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*o = Outcome(p)
	return nil
}

// Run modes.
const (
	ModeBackground  = "background"  // default: run without the terminal, report OK/FAIL
	ModeInteractive = "interactive" // hand the terminal to the command (an agent, an editor)
)

// Stdin choices: what the command reads on standard input.
const (
	StdinNone     = "none"
	StdinJSON     = "json"
	StdinMarkdown = "markdown"

	// Hook-only: the event (and its task) instead of the task alone.
	StdinEvent         = "event"
	StdinEventMarkdown = "event-markdown"
)

// CaptureNotes is the only `capture:` target: the task's notes.
const CaptureNotes = "notes"

// DefaultCaptureMax is how much output `capture: notes` keeps (KiB), unless
// the action sets capture_max.
const DefaultCaptureMax = 4

// ShowOutput opens the output view when a background run ends.
const ShowOutput = "output"

// Notify values: how the board tells the user that a background run ended.
const (
	NotifyBell    = "bell"    // the terminal bell
	NotifyDesktop = "desktop" // a desktop notification (OSC 777 and OSC 9)
)

// DefaultTimeout bounds a background run unless the action sets one.
const DefaultTimeout = 10 * time.Second

// DefaultKillGrace is how long a command gets between SIGTERM and SIGKILL when
// a run is ended (timeout, cancel, quit), unless the action sets kill_grace.
const DefaultKillGrace = 5 * time.Second

// DefaultMaxOutput is how much of each output stream a background run keeps
// (KiB), unless the action sets max_output.
const DefaultMaxOutput = 256

// reservedKeys are taken by the picker itself (navigation, quit and close, force-interactive, workdir).
const reservedKeys = "ijkqwx"

// ReservedKeys lists the characters the picker keeps for itself.
func ReservedKeys() string { return reservedKeys }

// Spec is one item of the `actions:` list.
type Spec struct {
	Name   string   `yaml:"id"`               // identifier, used as `tack run ID`
	Label  string   `yaml:"title,omitempty"`  // shown in the picker; default: id
	Labels []string `yaml:"labels,omitempty"` // free tags (code, review, research, ...) for consumers that classify actions
	Key    string   `yaml:"key,omitempty"`    // one-character shortcut in the picker
	// LegacyName and LegacyLabel are the deprecated keys `name` and `label`; Normalize folds them into Name and Label.
	LegacyName  string   `yaml:"name,omitempty"`
	LegacyLabel string   `yaml:"label,omitempty"`
	Command     []string `yaml:"command"`
	Mode        string   `yaml:"mode,omitempty"`    // background (default) | interactive
	Workdir     string   `yaml:"workdir,omitempty"` // relative to the config file; default: its directory
	// CreateWorkdir makes workdir (with its parents) when it does not exist, so a templated workdir such as wt/{slug} works.
	CreateWorkdir bool   `yaml:"create_workdir,omitempty"`
	Timeout       string `yaml:"timeout,omitempty"` // background only; default 10s
	// InteractiveTimeout bounds an interactive run (mode: interactive or `tack run -i`); default: none.
	InteractiveTimeout string            `yaml:"interactive_timeout,omitempty"`
	KillGrace          string            `yaml:"kill_grace,omitempty"` // SIGTERM, then SIGKILL after this; default 5s, 0s = SIGKILL at once
	MaxOutput          int               `yaml:"max_output,omitempty"` // KiB kept of each of stdout and stderr (background); default 256
	Exclusive          bool              `yaml:"exclusive,omitempty"`  // one background run per card at a time (in this process)
	Claim              bool              `yaml:"claim,omitempty"`      // claim the task (assignee + lease) for the run, release it after; see internal/claim
	Stdin              string            `yaml:"stdin,omitempty"`      // none (default) | json | markdown (hooks: | event | event-markdown)
	Show               string            `yaml:"show,omitempty"`       // "" | output: open the output when a background run ends
	Notify             string            `yaml:"notify,omitempty"`     // "" | bell | desktop: tell the user when a background run ends
	Env                map[string]string `yaml:"env,omitempty"`
	// Capture keeps the start of a background run's stdout on the task: "notes"
	// appends the first CaptureMax KiB (default 4) to its notes.
	Capture    string `yaml:"capture,omitempty"`
	CaptureMax int    `yaml:"capture_max,omitempty"`
	// OnSuccess and OnFailure move the task to this column or status when a run
	// ends well or badly (not when it is canceled).
	// Each may also append to the task's notes (append_notes: stdout | summary on
	// success, stdout | summary | error on failure). OnFail is an alias of OnFailure.
	OnSuccess Outcome `yaml:"on_success,omitempty"`
	OnFailure Outcome `yaml:"on_failure,omitempty"`
	OnFail    Outcome `yaml:"on_fail,omitempty"`
	// Backend limits the spec to the tasks of one backend: config sets it for
	// the `actions:` (and `hooks:`) of a backend entry. "" = every backend.
	Backend  string `yaml:"-"`
	conflict string // set by Normalize when the deprecated and the current key disagree
}

// cleanLabels trims the labels, drops empty and repeated ones and keeps the order.
func cleanLabels(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// placeholders maps each {name} usable in command to the task value it stands for.
var placeholders = map[string]func(task.Task) string{
	"id":          func(t task.Task) string { return t.ID },
	"title":       func(t task.Task) string { return t.Title },
	"description": func(t task.Task) string { return t.Description },
	"status":      func(t task.Task) string { return t.Status },
	"priority":    func(t task.Task) string { return t.Priority },
	"notes":       func(t task.Task) string { return t.Notes },
	"type":        func(t task.Task) string { return t.Type },
	"assignee":    func(t task.Task) string { return t.Assignee },
	"due":         func(t task.Task) string { return t.Due },
	"start":       func(t task.Task) string { return t.Start },
	"labels":      func(t task.Task) string { return t.Labels },
	"blocked_by":  func(t task.Task) string { return t.BlockedBy },
	"lease":       func(t task.Task) string { return t.Lease },
	"claimed_at":  func(t task.Task) string { return t.ClaimedAt },
	"backend":     func(t task.Task) string { return t.Backend },
	"json":        func(t task.Task) string { return string(taskJSON(t)) },
	"slug":        func(t task.Task) string { return slug(t.Title) },
}

// workdirPlaceholders are the names allowed in `workdir`: short values that become a
// single, safe path element (see pathElem).
var workdirPlaceholders = []string{"id", "backend", "slug"}

// slug turns a title into a short name safe for a directory or branch:
// lower-case letters and digits joined by single dashes, at most 40 characters.
func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}

// pathElem makes a task value one harmless path element: separators and a
// leading dot are replaced, so a task cannot steer workdir out of its directory.
func pathElem(v string) string {
	v = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r < 32 {
			return '_'
		}
		return r
	}, v)
	if strings.HasPrefix(v, ".") {
		v = "_" + v[1:]
	}
	if v == "" {
		v = "_"
	}
	return v
}

// Placeholders lists the names usable as {name} in command, for docs and errors.
func Placeholders() []string {
	return []string{"id", "title", "description", "status", "priority", "notes", "type", "assignee", "due", "start", "labels", "blocked_by", "lease", "claimed_at", "backend", "json", "slug"}
}

// placeholderRe matches the escapes {{ and }} (a literal brace) and {name}.
var placeholderRe = regexp.MustCompile(`\{\{|\}\}|\{(\w+)\}`)

// lowerName is what a placeholder name looks like; {HOME} or {print} in a
// shell, awk or jq text is not mistaken for one unless it resembles a name.
var lowerName = regexp.MustCompile(`^[a-z_]+$`)

// Normalize trims and defaults the spec in place. Call it before Validate.
func (s *Spec) Normalize() {
	s.Name = strings.TrimSpace(s.Name)
	s.Label = strings.TrimSpace(s.Label)
	if legacy := strings.TrimSpace(s.LegacyName); legacy != "" {
		if s.Name != "" && s.Name != legacy {
			s.conflict = "id and name are both set (name is the deprecated alias of id)"
		}
		if s.Name == "" {
			s.Name = legacy
		}
	}
	if legacy := strings.TrimSpace(s.LegacyLabel); legacy != "" && s.Label == "" {
		s.Label = legacy
	}
	s.LegacyName, s.LegacyLabel = "", ""
	s.Labels = cleanLabels(s.Labels)
	s.Key = strings.TrimSpace(s.Key)
	s.Mode = strings.ToLower(strings.TrimSpace(s.Mode))
	if s.Mode == "" {
		s.Mode = ModeBackground
	}
	s.Show = strings.ToLower(strings.TrimSpace(s.Show))
	s.Notify = strings.ToLower(strings.TrimSpace(s.Notify))
	s.Capture = strings.ToLower(strings.TrimSpace(s.Capture))
	if s.OnFailure.IsZero() {
		s.OnFailure = s.OnFail
	}
	s.OnFail = Outcome{}
	for _, o := range []*Outcome{&s.OnSuccess, &s.OnFailure} {
		o.Status = strings.TrimSpace(o.Status)
		o.AppendNotes = strings.ToLower(strings.TrimSpace(o.AppendNotes))
	}
	s.Stdin = strings.ToLower(strings.TrimSpace(s.Stdin))
	if s.Stdin == "" {
		s.Stdin = StdinNone
	}
}

// Title is the picker label.
func (s Spec) Title() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Name
}

// Validate checks one normalized spec. It does not look at other actions; see
// ValidateAll for duplicates.
func (s Spec) Validate() error { return s.ValidateExtra(nil) }

// ValidateExtra is Validate for callers (hooks) that add their own
// {placeholders} to the task ones; extra lists those names.
func (s Spec) ValidateExtra(extra []string) error { return s.ValidateWith(extra, nil) }

// ValidateWith is ValidateExtra plus extra stdin choices the caller (hooks)
// accepts besides none, json and markdown.
func (s Spec) ValidateWith(extra, extraStdin []string) error {
	if s.conflict != "" {
		return errors.New(s.conflict)
	}
	if s.Name == "" {
		return errors.New("id is required")
	}
	if bad := strings.IndexFunc(s.Name, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '-')
	}); bad >= 0 {
		return fmt.Errorf("id %q may only contain letters, digits, '_', '.' and '-' (it is used as `tack run ID`)", s.Name)
	}
	if len(s.Command) == 0 || strings.TrimSpace(s.Command[0]) == "" {
		return errors.New("command is required (a list: program and arguments)")
	}
	if strings.TrimSpace(s.Command[0]) != s.Command[0] || strings.ContainsAny(s.Command[0], "{}") {
		return errors.New("command[0] must be a plain program name or path (no surrounding spaces, no {placeholders})")
	}
	known := append(Placeholders(), extra...)
	for i, arg := range s.Command {
		for _, m := range placeholderRe.FindAllStringSubmatch(arg, -1) {
			name := m[1]
			if name == "" || slices.Contains(known, name) || !lowerName.MatchString(name) {
				continue // an escape, a placeholder, or text such as ${HOME}
			}
			// Another lower-case word stays literal (awk '{print}'), unless it looks like a typo of a name.
			if sug := query.Suggest(name, known); sug != "" {
				return fmt.Errorf("command[%d]: unknown placeholder {%s}; did you mean {%s}? (known: %s; write {{ and }} for literal braces)", i, name, sug, strings.Join(known, ", "))
			}
		}
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(s.Workdir, -1) {
		if name := m[1]; name != "" && slices.Contains(known, name) && !slices.Contains(workdirPlaceholders, name) {
			return fmt.Errorf("workdir: {%s} is not allowed (only %s: they become one safe path element)", name, strings.Join(workdirPlaceholders, ", "))
		}
	}
	switch s.Mode {
	case ModeBackground, ModeInteractive:
	default:
		return fmt.Errorf("mode %q must be %s or %s", s.Mode, ModeBackground, ModeInteractive)
	}
	if choices := append([]string{StdinNone, StdinJSON, StdinMarkdown}, extraStdin...); !slices.Contains(choices, s.Stdin) {
		return fmt.Errorf("stdin %q must be %s or %s", s.Stdin, strings.Join(choices[:len(choices)-1], ", "), choices[len(choices)-1])
	}
	switch s.Show {
	case "", ShowOutput:
	default:
		return fmt.Errorf("show %q must be %s (or left out)", s.Show, ShowOutput)
	}
	if s.Mode == ModeInteractive && s.Show != "" {
		return errors.New("show applies to background actions; an interactive one prints to the terminal")
	}
	switch s.Notify {
	case "", NotifyBell, NotifyDesktop:
	default:
		return fmt.Errorf("notify %q must be %s or %s (or left out)", s.Notify, NotifyBell, NotifyDesktop)
	}
	if s.Mode == ModeInteractive && s.Notify != "" {
		return errors.New("notify applies to background actions; an interactive one ends in front of you")
	}
	if s.Mode == ModeInteractive && s.Stdin != StdinNone {
		return errors.New("an interactive action owns the terminal, so stdin must be none")
	}
	if s.Timeout != "" {
		if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("invalid timeout %q (e.g. 30s)", s.Timeout)
		}
	}
	if s.CreateWorkdir && s.Workdir == "" {
		return errors.New("create_workdir needs workdir (the config's directory always exists)")
	}
	if s.InteractiveTimeout != "" {
		if d, err := time.ParseDuration(s.InteractiveTimeout); err != nil || d <= 0 {
			return fmt.Errorf("invalid interactive_timeout %q (e.g. 2h)", s.InteractiveTimeout)
		}
	}
	if s.KillGrace != "" {
		if d, err := time.ParseDuration(s.KillGrace); err != nil || d < 0 {
			return fmt.Errorf("invalid kill_grace %q (e.g. 10s; 0s kills at once)", s.KillGrace)
		}
	}
	if s.MaxOutput < 0 {
		return fmt.Errorf("max_output %d must be a number of KiB (0 = the default, %d)", s.MaxOutput, DefaultMaxOutput)
	}
	switch s.Capture {
	case "":
	case CaptureNotes:
		if s.Mode == ModeInteractive {
			return errors.New("capture applies to background actions; an interactive one prints to the terminal")
		}
	default:
		return fmt.Errorf("capture %q must be %s (or left out)", s.Capture, CaptureNotes)
	}
	for _, c := range []struct {
		key string
		o   Outcome
		ok  []string
	}{
		{"on_success", s.OnSuccess, []string{NotesStdout, NotesSummary}},
		{"on_failure", s.OnFailure, []string{NotesStdout, NotesSummary, NotesError}},
	} {
		if c.o.AppendNotes == "" {
			continue
		}
		if !slices.Contains(c.ok, c.o.AppendNotes) {
			return fmt.Errorf("%s: append_notes %q must be %s", c.key, c.o.AppendNotes, strings.Join(c.ok, ", "))
		}
		if s.Mode == ModeInteractive {
			return fmt.Errorf("%s: append_notes applies to background actions; an interactive one prints to the terminal", c.key)
		}
	}
	for _, c := range []struct {
		key string
		o   Outcome
	}{{"on_success", s.OnSuccess}, {"on_failure", s.OnFailure}} {
		if len(c.o.Command) == 0 {
			continue
		}
		if err := c.o.CommandSpec(s.Name, s).ValidateExtra(OutcomeExtra); err != nil {
			return fmt.Errorf("%s: command: %w", c.key, err)
		}
	}
	if s.CaptureMax < 0 {
		return fmt.Errorf("capture_max %d must be a number of KiB (0 = the default, %d)", s.CaptureMax, DefaultCaptureMax)
	}
	if s.Key != "" {
		if utf8.RuneCountInString(s.Key) != 1 {
			return fmt.Errorf("key %q must be a single character", s.Key)
		}
		if strings.Contains(reservedKeys, strings.ToLower(s.Key)) {
			return fmt.Errorf("key %q is reserved by the picker (reserved: %s)", s.Key, strings.Join(strings.Split(reservedKeys, ""), ", "))
		}
	}
	for k := range s.Env {
		if k == "" || strings.Contains(k, "=") {
			return fmt.Errorf("env name %q is invalid", k)
		}
	}
	return nil
}

// ValidateAll normalizes and validates the top-level list, rejecting
// duplicate names and keys.
func ValidateAll(specs []Spec) error { return ValidateFor(specs, "") }

// ValidateFor is ValidateAll for the `actions:` of one backend entry (""
// = the top-level list): it also scopes every spec to that backend.
func ValidateFor(specs []Spec, backend string) error {
	names, keys := map[string]bool{}, map[string]bool{}
	for i := range specs {
		s := &specs[i]
		s.Normalize()
		s.Backend = backend
		if err := s.Validate(); err != nil {
			return fmt.Errorf("actions[%d]: %w", i+1, err)
		}
		n := strings.ToLower(s.Name)
		if names[n] {
			return fmt.Errorf("actions[%d]: duplicate name %q", i+1, s.Name)
		}
		names[n] = true
		if s.Key != "" {
			k := strings.ToLower(s.Key)
			if keys[k] {
				return fmt.Errorf("actions[%d]: key %q is already used by another action", i+1, s.Key)
			}
			keys[k] = true
		}
	}
	return nil
}

// For returns the actions that apply to a task of backend, out of a list
// holding the shared ones (Backend "") and those of any backend: the shared
// ones in order, where an action of the backend with the same name takes the
// place of a shared one, then the backend's other actions.
func For(specs []Spec, backend string) []Spec {
	var out []Spec
	own := func(name string) (Spec, bool) {
		for _, s := range specs {
			if s.Backend != "" && strings.EqualFold(s.Backend, backend) && strings.EqualFold(s.Name, name) {
				return s, true
			}
		}
		return Spec{}, false
	}
	for _, s := range specs {
		if s.Backend != "" {
			continue
		}
		if o, ok := own(s.Name); ok {
			s = o
		}
		out = append(out, s)
	}
	for _, s := range specs {
		if s.Backend != "" && strings.EqualFold(s.Backend, backend) {
			if _, shared := Find(out, s.Name); !shared {
				out = append(out, s)
			}
		}
	}
	return out
}

// CheckKeys reports two actions that apply to the tasks of backend (see For)
// with the same picker key.
func CheckKeys(specs []Spec, backend string) error {
	by := map[string]string{}
	for _, s := range For(specs, backend) {
		if s.Key == "" {
			continue
		}
		k := strings.ToLower(s.Key)
		if other, ok := by[k]; ok {
			return fmt.Errorf("actions %q and %q both use key %q on the tasks of backend %s", other, s.Name, s.Key, backend)
		}
		by[k] = s.Name
	}
	return nil
}

// Names lists the action names of every backend once, in order.
func Names(specs []Spec) []string {
	var out []string
	for _, s := range specs {
		if !slices.ContainsFunc(out, func(n string) bool { return strings.EqualFold(n, s.Name) }) {
			out = append(out, s.Name)
		}
	}
	return out
}

// DefaultMaxParallel is how many runs of an action started on the board's
// marked cards go at once when the config sets no action_max_parallel;
// HardMaxParallel is the largest value accepted.
const (
	DefaultMaxParallel = 2
	HardMaxParallel    = 16
)

// CheckMaxParallel validates action_max_parallel (0 = the default).
func CheckMaxParallel(n int) error {
	if n < 0 || n > HardMaxParallel {
		return fmt.Errorf("%d is out of range (0 = default %d, 1..%d)", n, DefaultMaxParallel, HardMaxParallel)
	}
	return nil
}

// Find returns the action called name (case-insensitive).
func Find(specs []Spec, name string) (Spec, bool) {
	for _, s := range specs {
		if strings.EqualFold(s.Name, strings.TrimSpace(name)) {
			return s, true
		}
	}
	return Spec{}, false
}

// freeText are the placeholders whose value is free text, which may start with "-".
var freeText = []string{"title", "description", "notes", "labels", "assignee"}

// shells are base names of programs whose -c argument is a shell script.
var shells = []string{"sh", "bash", "zsh", "dash", "ash", "ksh"}

// Warnings lists risky but valid things in the command:
//   - an argument that starts with a free-text placeholder, follows a plain
//     argument (not an option, whose value it may be) and comes before any
//     "--", so a title such as "--add-dir /" is parsed by the program as an
//     option. Put "--" before the text, or pass it on stdin.
//   - the ShellWarnings.
func (s Spec) Warnings() []string {
	if len(s.Command) > 0 && slices.Contains(shells, filepath.Base(s.Command[0])) {
		// sh -c 'script' sh "{title}": the text is a positional parameter
		return s.ShellWarnings(nil)
	}
	var out []string
	for i, arg := range s.Command {
		if arg == "--" {
			break
		}
		if i < 1 || strings.HasPrefix(s.Command[i-1], "-") {
			continue
		}
		m := placeholderRe.FindStringSubmatch(arg)
		if m == nil || !strings.HasPrefix(arg, m[0]) || !slices.Contains(freeText, m[1]) {
			continue
		}
		out = append(out, fmt.Sprintf("command[%d]: %s starts with {%s}, so task text beginning with \"-\" is read as an option; put \"--\" before it or use stdin: markdown", i, strconv.Quote(arg), m[1]))
	}
	return out
}

// ShellWarnings lists the placeholders inside the script of `sh -c SCRIPT`:
// the value is pasted into the script text unquoted, so the shell runs a title
// such as "$(rm -rf ~)". Passed as a positional parameter it is safe. extra
// names the caller's own placeholders (hooks).
func (s Spec) ShellWarnings(extra []string) []string {
	if len(s.Command) == 0 || !slices.Contains(shells, filepath.Base(s.Command[0])) {
		return nil
	}
	i := shellScript(s.Command)
	if i == 0 {
		return nil
	}
	var out []string
	known := append(Placeholders(), extra...)
	for _, m := range placeholderRe.FindAllStringSubmatch(s.Command[i], -1) {
		if m[1] != "" && slices.Contains(known, m[1]) {
			out = append(out, fmt.Sprintf("command[%d]: {%s} is inside the shell script, so the shell interprets its value ($(...), quotes, ;); pass it as an argument instead: [%s, -c, '... \"$1\" ...', %s, \"{%s}\"]",
				i, m[1], s.Command[0], filepath.Base(s.Command[0]), m[1]))
		}
	}
	return out
}

// shellScript is the index of the script of `sh [options] -c SCRIPT ...` (the
// first operand once an option containing c was seen), 0 when there is none.
func shellScript(cmd []string) int {
	hasC := false
	for i := 1; i < len(cmd); i++ {
		a := cmd[i]
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+") {
			hasC = hasC || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c"))
			continue
		}
		if hasC {
			return i
		}
		return 0
	}
	return 0
}

// agentPrograms are base names of AI coding agents, which run for minutes.
var agentPrograms = []string{"claude", "codex", "aider", "gemini", "opencode", "goose", "amp", "cursor-agent", "copilot"}

// TimeoutWarning says so when the command looks like an AI agent (command[0]'s
// base name, after a leading `env [VAR=x]`) and a background run has no explicit
// `timeout:`, so it is ended at DefaultTimeout. "" otherwise; interactive runs
// have no timeout and are exempt.
func (s Spec) TimeoutWarning() string {
	if s.Mode == ModeInteractive || strings.TrimSpace(s.Timeout) != "" || len(s.Command) == 0 {
		return ""
	}
	cmd := s.Command
	if filepath.Base(cmd[0]) == "env" {
		i := 1
		for i < len(cmd) && (strings.HasPrefix(cmd[i], "-") || strings.Contains(cmd[i], "=")) {
			i++
		}
		if i >= len(cmd) {
			return ""
		}
		cmd = cmd[i:]
	}
	prog := filepath.Base(cmd[0])
	if !slices.Contains(agentPrograms, prog) {
		return ""
	}
	return fmt.Sprintf("%s looks like an AI agent but sets no timeout:, so it is ended after %s; set timeout: (e.g. 30m)", prog, DefaultTimeout)
}

// CaptureBytes is how many bytes of stdout `capture: notes` keeps.
func (s Spec) CaptureBytes() int {
	if s.CaptureMax > 0 {
		return s.CaptureMax << 10
	}
	return DefaultCaptureMax << 10
}

// WithWorkdir returns s running in dir instead of its configured workdir (a
// per-run override; "" keeps the configured one). Like the configured value it
// takes placeholders and is relative to the config directory.
func (s Spec) WithWorkdir(dir string) Spec {
	if dir != "" {
		s.Workdir = dir
	}
	return s
}

// WithRun returns s with the variables that tie a run to its record:
// TACK_RUN_ID and, when the action log is on, TACK_ACTION_LOG (its file or
// directory). They go in the command's environment only; records never hold it.
func (s Spec) WithRun(runID, logPath string) Spec {
	env := make(map[string]string, len(s.Env)+2)
	for k, v := range s.Env {
		env[k] = v
	}
	esc := func(v string) string { return strings.NewReplacer("{", "{{", "}", "}}").Replace(v) } // Expand fills {name}
	env["TACK_RUN_ID"] = esc(runID)
	if logPath != "" {
		env["TACK_ACTION_LOG"] = esc(logPath)
	}
	s.Env = env
	return s
}

// Plan is a fully expanded invocation, ready to run.
type Plan struct {
	Argv  []string
	Env   []string // KEY=VALUE, appended to the process environment
	Stdin string   // "" = none
	Dir   string
	Err   error // the command cannot be started as expanded (an argument too large for exec)
}

// Limits that keep exec from failing with E2BIG ("argument list too long"):
// Linux refuses one argument or environment string of 128 KiB and the lot of
// them above a few MiB.
const (
	maxArgLen  = 120 << 10 // one argv element
	maxEnvLen  = 64 << 10  // one variable made from the task (TACK_TASK_JSON, ...)
	maxArgsLen = 1 << 20   // argv and the plan's variables together
)

// stripNUL drops NUL bytes, which exec refuses in arguments and variables.
func stripNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// EnvVar returns the KEY=VALUE entry for a variable made from task data. A value
// too large for the environment is left out and KEY_OMITTED=... says why, so one
// card with long notes does not make every action fail to start.
func EnvVar(key, value string) string {
	value = stripNUL(value)
	if len(key)+1+len(value) <= maxEnvLen {
		return key + "=" + value
	}
	return fmt.Sprintf("%s_OMITTED=%d bytes, too large for the environment; read the task from stdin (stdin: json or markdown)", key, len(value))
}

// Expand fills the placeholders for t. baseDir is the config directory that
// Workdir resolves against. Substitution is a single pass: a task whose title
// contains "{id}" is not expanded a second time.
func (s Spec) Expand(t task.Task, baseDir string) Plan { return s.ExpandWith(t, baseDir, nil) }

// ExpandWith is Expand with extra {name} values (and no other change); the
// extra names win over none, they never shadow a task placeholder.
func (s Spec) ExpandWith(t task.Task, baseDir string, extra map[string]string) Plan {
	// fill replaces the {name}s of text; with only set, just those names (their values made path-safe).
	fill := func(text string, only []string) string {
		return placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
			switch m {
			case "{{":
				return "{"
			case "}}":
				return "}"
			}
			name := m[1 : len(m)-1]
			if only != nil && !slices.Contains(only, name) {
				return m
			}
			v, ok := "", false
			if f, found := placeholders[name]; found {
				v, ok = f(t), true
			} else if e, found := extra[name]; found {
				v, ok = e, true
			}
			if !ok {
				return m
			}
			if only != nil {
				return pathElem(stripNUL(v))
			}
			return stripNUL(v)
		})
	}
	p := Plan{Dir: baseDir}
	if s.Workdir != "" {
		tmpl, err := pathenv.Expand(s.Workdir) // before the placeholders, so task text is never expanded
		if err != nil {
			p.Err = fmt.Errorf("%s: workdir: %w", s.Name, err)
		}
		p.Dir = fill(tmpl, workdirPlaceholders)
		if !filepath.IsAbs(p.Dir) {
			p.Dir = filepath.Join(baseDir, p.Dir)
		}
	}
	for _, arg := range s.Command {
		p.Argv = append(p.Argv, fill(arg, nil))
	}
	for k, v := range s.Env {
		p.Env = append(p.Env, k+"="+stripNUL(fill(v, nil)))
	}
	if p.Err == nil {
		p.Err = s.checkSize(p.Argv, p.Env, t, extra)
	}
	p.Env = append(p.Env,
		EnvVar("TACK_TASK_ID", t.ID),
		EnvVar("TACK_TASK_TITLE", t.Title),
		EnvVar("TACK_TASK_STATUS", t.Status),
		EnvVar("TACK_BACKEND", t.Backend),
		EnvVar("TACK_WORKDIR", p.Dir),
		EnvVar("TACK_TASK_JSON", string(taskJSON(t))),
	)
	switch s.Stdin {
	case StdinJSON:
		p.Stdin = string(taskJSON(t)) + "\n"
	case StdinMarkdown:
		p.Stdin = markdown(t)
	}
	return p
}

// checkSize reports an argument or a user `env:` value the system would refuse
// to start, naming the placeholders that made it large. env holds the KEY=VALUE
// entries of s.Env once filled in.
func (s Spec) checkSize(argv, env []string, t task.Task, extra map[string]string) error {
	total := 0
	for _, e := range env {
		total += len(e) + 1
		if len(e) < maxArgLen {
			continue
		}
		key, _, _ := strings.Cut(e, "=")
		var big []string
		for _, m := range placeholderRe.FindAllStringSubmatch(s.Env[key], -1) {
			name := m[1]
			v, ok := extra[name]
			if f, known := placeholders[name]; known {
				v, ok = f(t), true
			}
			if ok && len(v) > 4<<10 {
				big = append(big, fmt.Sprintf("{%s} is %d KiB", name, len(v)>>10))
			}
		}
		why := "the text of the variable"
		if len(big) > 0 {
			why = strings.Join(big, ", ")
		}
		return fmt.Errorf("%s: env %s is %d KiB once filled in (the limit is %d KiB): %s; pass large text on stdin (stdin: json or markdown) instead", s.Name, key, len(e)>>10, maxArgLen>>10, why)
	}
	for i, a := range argv {
		total += len(a) + 1
		if len(a) < maxArgLen {
			continue
		}
		var big []string
		for _, m := range placeholderRe.FindAllStringSubmatch(s.Command[i], -1) {
			name := m[1]
			v, ok := extra[name]
			if f, known := placeholders[name]; known {
				v, ok = f(t), true
			}
			if ok && len(v) > 4<<10 {
				big = append(big, fmt.Sprintf("{%s} is %d KiB", name, len(v)>>10))
			}
		}
		why := "the text of the command"
		if len(big) > 0 {
			why = strings.Join(big, ", ")
		}
		return fmt.Errorf("%s: command[%d] is %d KiB once filled in (the limit is %d KiB): %s; pass large text on stdin (stdin: json or markdown) instead", s.Name, i, len(a)>>10, maxArgLen>>10, why)
	}
	if total > maxArgsLen {
		return fmt.Errorf("%s: the arguments are %d KiB once filled in (the limit is %d KiB); pass large text on stdin (stdin: json or markdown) instead", s.Name, total>>10, maxArgsLen>>10)
	}
	return nil
}

// timeout is the background time limit.
func (s Spec) timeout() time.Duration {
	if d, err := time.ParseDuration(s.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultTimeout
}

// ClaimLease is how long the claim of a `claim: true` run lasts: as long as
// the run may take (timeout plus kill grace), 0 (until released) for an
// interactive run without interactive_timeout.
func (s Spec) ClaimLease(interactive bool) time.Duration {
	if interactive {
		if d, err := time.ParseDuration(s.InteractiveTimeout); err == nil && d > 0 {
			return d + s.Grace()
		}
		return 0
	}
	return s.timeout() + s.Grace()
}

// Grace is the time between SIGTERM and SIGKILL when a run is ended.
func (s Spec) Grace() time.Duration {
	if d, err := time.ParseDuration(s.KillGrace); err == nil && d >= 0 && s.KillGrace != "" {
		return d
	}
	return DefaultKillGrace
}

// maxOutput is the number of bytes kept of each stream.
func (s Spec) maxOutput() int {
	if s.MaxOutput > 0 {
		return s.MaxOutput << 10
	}
	return DefaultMaxOutput << 10
}

func taskJSON(t task.Task) []byte {
	b, err := json.Marshal(t)
	if err != nil { // a struct of strings cannot fail; keep the signature simple
		return []byte("{}")
	}
	return b
}

// TaskMarkdown renders the task as a prompt-friendly document (stdin: markdown).
func TaskMarkdown(t task.Task) string { return markdown(t) }

// markdown renders the task as a prompt-friendly document.
func markdown(t task.Task) string {
	var b strings.Builder
	b.WriteString("# " + t.Title + "\n\n")
	for _, f := range []struct{ k, v string }{
		{"id", t.ID}, {"backend", t.Backend}, {"status", t.Status}, {"priority", t.Priority},
		{"type", t.Type}, {"assignee", t.Assignee}, {"due", t.Due}, {"start", t.Start}, {"labels", t.Labels},
		{"blocked_by", t.BlockedBy}, {"lease", t.Lease}, {"claimed_at", t.ClaimedAt},
	} {
		if f.v != "" {
			b.WriteString("- " + f.k + ": " + f.v + "\n")
		}
	}
	if t.Description != "" {
		b.WriteString("\n## Description\n\n" + t.Description + "\n")
	}
	if t.Notes != "" {
		b.WriteString("\n## Notes\n\n" + t.Notes + "\n")
	}
	return b.String()
}

// environ is the process environment plus the plan's variables.
func (p Plan) environ() []string { return append(os.Environ(), p.Env...) }

// ShellWord quotes s for display only when a shell would split or expand it.
func ShellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || r == ',' || r == '@' || r == '%' || r == '+' ||
			('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z'))
	}) < 0 {
		return s
	}
	if !strings.ContainsAny(s, "\n'") {
		return "'" + s + "'"
	}
	return strconv.Quote(s)
}

// Line is the command as one display line (program and arguments, quoted
// where a shell would need it). The environment is not part of it.
func (p Plan) Line() string {
	words := make([]string, len(p.Argv))
	for i, a := range p.Argv {
		words[i] = ShellWord(a)
	}
	return strings.Join(words, " ")
}
