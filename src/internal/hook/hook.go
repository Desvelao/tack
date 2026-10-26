// Package hook runs user-configured commands when something happens: a task
// is created, updated, deleted or moved, tasks appear or disappear after a
// reload, a reload fails, an action ends. A hook is an action (same
// command, placeholders, env, stdin, timeout) plus the events it listens to
// and an optional `when:` filter. Only after-events exist: a hook never
// holds up or vetoes the thing it reacts to, and a failing hook never fails
// it.
package hook

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tack/internal/action"
	"tack/internal/query"
	"tack/internal/task"
)

// Event names. A name with a .success / .fail (or .succeeded / .failed)
// suffix is a variant of its parent: listening to the parent gets both.
const (
	TaskCreate        = "task.create"
	TaskCreateSuccess = "task.create.success"
	TaskCreateFail    = "task.create.fail"
	TaskUpdate        = "task.update"
	TaskUpdateSuccess = "task.update.success"
	TaskUpdateFail    = "task.update.fail"
	TaskDelete        = "task.delete"
	TaskDeleteSuccess = "task.delete.success"
	TaskDeleteFail    = "task.delete.fail"
	TaskMoved         = "task.moved"
	TaskChanged       = "task.changed"
	TaskAppeared      = "task.appeared"
	TaskDisappeared   = "task.disappeared"
	TaskDue           = "task.due"
	ColumnWIPExceeded = "column.wip_exceeded"
	ReloadOK          = "reload.ok"
	ReloadFail        = "reload.fail"
	ActionStarted     = "action.started"
	ActionFinished    = "action.finished"
	ActionSucceeded   = "action.succeeded"
	ActionFailed      = "action.failed"
)

// Prefixes of the events named after a board column (task.entered.Review).
const (
	TaskEnteredPrefix = "task.entered."
	TaskLeftPrefix    = "task.left."
)

// EventInfo documents one event; docs/config.md mirrors this table.
type EventInfo struct {
	Name    string
	Parent  string // listening to Parent also fires on this event
	HasTask bool   // the event carries a task (so `when:` makes sense)
	Desc    string
}

var events = []EventInfo{
	{TaskCreate, "", true, "a task was created (success or failure)"},
	{TaskCreateSuccess, TaskCreate, true, "a task was created; the filter sees the new task"},
	{TaskCreateFail, TaskCreate, true, "creating a task failed; {error} holds the reason"},
	{TaskUpdate, "", true, "a task was written back (success or failure)"},
	{TaskUpdateSuccess, TaskUpdate, true, "a task was updated; the filter sees the new task, {old_status} the previous status"},
	{TaskUpdateFail, TaskUpdate, true, "updating a task failed; {error} holds the reason"},
	{TaskDelete, "", true, "a task was deleted (success or failure)"},
	{TaskDeleteSuccess, TaskDelete, true, "a task was deleted; the filter sees the task as it was"},
	{TaskDeleteFail, TaskDelete, true, "deleting a task failed; {error} holds the reason"},
	{TaskMoved, "", true, "an update, or a change seen on a reload (made outside this run), changed the status; {old_status} is the previous one"},
	{TaskChanged, "", true, "a reload found a task changed outside this run (any field); {old_json} is the previous version"},
	{TaskAppeared, "", true, "a reload found a task that was not there before (made outside this run)"},
	{TaskDisappeared, "", true, "a reload no longer finds a task (removed outside this run); the filter sees it as it was"},
	{TaskDue, "", true, "a task that is not finished reached its due date or is overdue (TUI, watch; once per task per day)"},
	{ColumnWIPExceeded, "", false, "a column went over its WIP limit (TUI, watch; once per excess); {column} is its name"},
	{TaskEnteredPrefix + "COLUMN", "", true, "an update moved a task into the board column COLUMN; {column}, {old_column}, {new_status}"},
	{TaskLeftPrefix + "COLUMN", "", true, "an update moved a task out of the board column COLUMN; {column}, {old_column}, {new_status}"},
	{ReloadOK, "", false, "the board reloaded (TUI, watch; every refresh)"},
	{ReloadFail, "", false, "the board could not reload (TUI, watch; once per distinct error); {error} holds it"},
	{ActionStarted, "", true, "an action is about to start on a task; {action} is its name"},
	{ActionFinished, "", true, "an action ended (success or failure)"},
	{ActionSucceeded, ActionFinished, true, "an action ended without error"},
	{ActionFailed, ActionFinished, true, "an action ended with an error; {error} holds it"},
}

// Events lists every event with its description.
func Events() []EventInfo { return append([]EventInfo(nil), events...) }

func lookupEvent(name string) (EventInfo, bool) {
	if column(name) != "" {
		return EventInfo{Name: name, HasTask: true}, true
	}
	for _, e := range events {
		if e.Name == name {
			return e, true
		}
	}
	return EventInfo{}, false
}

// column is the column an entered / left event is named after ("" = not one).
func column(name string) string {
	l := strings.ToLower(name)
	for _, p := range []string{TaskEnteredPrefix, TaskLeftPrefix} {
		if strings.HasPrefix(l, p) {
			return strings.TrimSpace(name[len(p):])
		}
	}
	return ""
}

// HasTask reports whether the event name carries a task.
func HasTask(name string) bool {
	info, _ := lookupEvent(name)
	return info.HasTask
}

// EventNames lists the event names.
func EventNames() []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.Name
	}
	return names
}

// extraPlaceholders are the {names} a hook command may use besides the task ones.
var extraPlaceholders = []string{"event", "source", "error", "old_status", "old_json", "action", "column", "old_column", "new_status"}

// ExtraPlaceholders lists them, for docs and errors.
func ExtraPlaceholders() []string { return append([]string(nil), extraPlaceholders...) }

// Spec is one item of the `hooks:` list.
type Spec struct {
	action.Spec `yaml:",inline"`
	On          []string `yaml:"on"`
	When        string   `yaml:"when,omitempty"` // query on the task of the event
	// MaxDepth lowers hook_max_depth for this hook (0 = no own limit).
	MaxDepth int `yaml:"max_depth,omitempty"`
	// MaxParallel caps this hook's runs at once (0 = default: 1 when every
	// event is reload.*, else unlimited).
	MaxParallel int `yaml:"max_parallel,omitempty"`
	// MinInterval is the least time between two starts of this hook ("" = none).
	MinInterval string `yaml:"min_interval,omitempty"`
	// Queue says what happens to an event that exceeds the limits: drop
	// (default), latest (keep the newest) or all (keep up to MaxQueue).
	Queue string `yaml:"queue,omitempty"`
	// Unique "task" skips an event while a run of this hook for the same task
	// (backend and ID) is still going ("" = no check; this process only).
	Unique string `yaml:"unique,omitempty"`
	// Retries is how many times a failed run is started again (0 = none, at most MaxRetries).
	Retries int `yaml:"retries,omitempty"`
	// RetryDelay is the wait before each retry ("" = DefaultRetryDelay).
	RetryDelay string `yaml:"retry_delay,omitempty"`
	// Dedupe makes task.appeared / task.disappeared fire in only one of the
	// processes that notice the change (a marker file in the config dir).
	Dedupe bool `yaml:"dedupe,omitempty"`

	when query.Query
}

// MaxRetries bounds retries.
const MaxRetries = 10

// DefaultRetryDelay is the wait before a retry unless retry_delay is set.
const DefaultRetryDelay = time.Second

// Normalize trims and defaults the spec in place; call it before Validate.
func (s *Spec) Normalize() {
	s.Spec.Normalize()
	for i, e := range s.On {
		s.On[i] = strings.ToLower(strings.TrimSpace(e))
	}
	s.When = strings.TrimSpace(s.When)
	s.MinInterval = strings.TrimSpace(s.MinInterval)
	s.Queue = strings.ToLower(strings.TrimSpace(s.Queue))
	s.Unique = strings.ToLower(strings.TrimSpace(s.Unique))
	s.RetryDelay = strings.TrimSpace(s.RetryDelay)
}

// Queue modes.
const (
	QueueDrop   = "drop"
	QueueLatest = "latest"
	QueueAll    = "all"
)

// UniqueTask is the unique: value that allows one run per task.
const UniqueTask = "task"

// MaxGlobalParallel bounds hook_max_parallel.
const MaxGlobalParallel = 64

// CheckMaxParallel validates hook_max_parallel (0 = unlimited).
func CheckMaxParallel(n int) error {
	if n < 0 || n > MaxGlobalParallel {
		return fmt.Errorf("max parallel %d is out of range (0 = unlimited, 1..%d)", n, MaxGlobalParallel)
	}
	return nil
}

// MaxQueue bounds queue: all; events beyond it are dropped and counted.
const MaxQueue = 100

// parallel is the effective max_parallel (0 = unlimited).
func (s *Spec) parallel() int {
	if s.MaxParallel > 0 {
		return s.MaxParallel
	}
	for _, e := range s.On {
		if !strings.HasPrefix(e, "reload.") {
			return 0
		}
	}
	return 1
}

func (s *Spec) interval() time.Duration {
	d, _ := time.ParseDuration(s.MinInterval)
	return max(d, 0)
}

func (s *Spec) retryDelay() time.Duration {
	if d, err := time.ParseDuration(s.RetryDelay); err == nil && s.RetryDelay != "" {
		return max(d, 0)
	}
	return DefaultRetryDelay
}

// ShellWarnings is action.Spec.ShellWarnings knowing the hook placeholders too.
func (s Spec) ShellWarnings() []string { return s.Spec.ShellWarnings(extraPlaceholders) }

// WhenWarnings flags a when: that may never match: task.delete.fail often
// carries only the task's id (the task could not be read before the delete).
func (s Spec) WhenWarnings() []string {
	if s.When == "" || !s.listens(TaskDeleteFail) {
		return nil
	}
	return []string{"when: on task.delete.fail: the event often carries only the task id (the task could not be read), so a when: on other fields may never match"}
}

// TestEvent builds the event `tack hooks test` fires for s on t: name ("" =
// the first of on:, a parent event as its .success variant), the task as its
// subject, oldStatus as the previous status of an update. A fail event gets
// a made-up error.
func TestEvent(s *Spec, name string, t task.Task, oldStatus string) (Event, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = s.On[0]
		for _, e := range events {
			if e.Parent == name {
				name = e.Name // the first variant is .success / .succeeded
				break
			}
		}
	}
	info, ok := lookupEvent(name)
	if !ok {
		return Event{}, fmt.Errorf("unknown event %q%s (events: %s)", name, query.DidYouMean(name, EventNames()), strings.Join(EventNames(), ", "))
	}
	for _, e := range events {
		if e.Parent == name {
			return Event{}, fmt.Errorf("%s is never fired itself; test %s.success or %s.fail", name, name, name)
		}
	}
	if !s.listens(name) {
		return Event{}, fmt.Errorf("hook %s does not listen to %s (on: %s)", s.Name, name, strings.Join(s.On, ", "))
	}
	ev := Event{Name: name, Source: "test"}
	if !info.HasTask {
		if name == ReloadFail {
			ev.Err = "test error"
		}
		if name == ColumnWIPExceeded {
			ev.Column = "test"
		}
		return ev, nil
	}
	switch name {
	case TaskDeleteSuccess, TaskDeleteFail, TaskDisappeared:
		ev.Old, ev.HasOld = t, true
	case TaskUpdateSuccess, TaskMoved, TaskChanged:
		ev.Old, ev.HasOld, ev.New = t, true, t
		if oldStatus != "" {
			ev.Old.Status = oldStatus
		}
	default:
		ev.New = t
		if column(name) != "" {
			ev.Old, ev.HasOld = t, true
			if oldStatus != "" {
				ev.Old.Status = oldStatus
			}
		}
	}
	if strings.HasSuffix(name, ".fail") || name == ActionFailed {
		ev.Err = "test error"
	}
	if strings.HasPrefix(name, "action.") {
		ev.Action = "test"
	}
	return ev, nil
}

func (s *Spec) queueMode() string {
	if s.Queue == "" {
		return QueueDrop
	}
	return s.Queue
}

// Validate checks one normalized spec.
func (s *Spec) Validate() error {
	as := s.Spec
	if len(s.Command) == 0 {
		return errors.New("command is required (a list: program and arguments)")
	}
	if err := as.ValidateWith(extraPlaceholders, []string{action.StdinEvent, action.StdinEventMarkdown}); err != nil {
		return err
	}
	if s.Mode != action.ModeBackground {
		return errors.New("a hook runs in the background (mode: interactive is for actions)")
	}
	if s.InteractiveTimeout != "" {
		return errors.New("interactive_timeout is for interactive actions; use timeout for a hook")
	}
	if s.Key != "" || s.Label != "" {
		return errors.New("key and label are for the actions picker; a hook has neither")
	}
	if s.Notify != "" {
		return errors.New("notify is for actions started from the board; a hook has no board to tell")
	}
	if s.Claim {
		return errors.New("claim is for actions; a hook does not claim the task")
	}
	if s.Capture != "" || !s.OnSuccess.IsZero() || !s.OnFailure.IsZero() || !s.OnFail.IsZero() {
		return errors.New("capture, on_success and on_failure are for actions; a hook does not change the task")
	}
	if err := CheckMaxDepth(s.MaxDepth); err != nil {
		return err
	}
	if s.MaxParallel < 0 {
		return fmt.Errorf("max_parallel %d is out of range (1 or more; 0 = default)", s.MaxParallel)
	}
	if s.MinInterval != "" {
		if d, err := time.ParseDuration(s.MinInterval); err != nil || d < 0 {
			return fmt.Errorf("invalid min_interval %q (e.g. 30s)", s.MinInterval)
		}
	}
	if s.Retries < 0 || s.Retries > MaxRetries {
		return fmt.Errorf("retries %d is out of range (0..%d)", s.Retries, MaxRetries)
	}
	if s.RetryDelay != "" {
		if d, err := time.ParseDuration(s.RetryDelay); err != nil || d < 0 {
			return fmt.Errorf("invalid retry_delay %q (e.g. 5s)", s.RetryDelay)
		}
		if s.Retries == 0 {
			return errors.New("retry_delay needs retries (how many times to start a failed run again)")
		}
	}
	if !slices.Contains([]string{"", QueueDrop, QueueLatest, QueueAll}, s.Queue) {
		return fmt.Errorf("invalid queue %q (drop, latest or all)", s.Queue)
	}
	if s.Unique != "" && s.Unique != UniqueTask {
		return fmt.Errorf("invalid unique %q (task)", s.Unique)
	}
	if len(s.On) == 0 {
		return errors.New("on is required (a list of events; see docs/config.md)")
	}
	withTask := true
	for _, e := range s.On {
		info, ok := lookupEvent(e)
		if !ok {
			return fmt.Errorf("unknown event %q%s (events: %s)", e, query.DidYouMean(e, EventNames()), strings.Join(EventNames(), ", "))
		}
		withTask = withTask && info.HasTask
	}
	if s.Dedupe && !slices.Contains(s.On, TaskAppeared) && !slices.Contains(s.On, TaskDisappeared) {
		return errors.New("dedupe applies only to task.appeared / task.disappeared (other events fire only in the process that wrote)")
	}
	s.when = query.Query{}
	if s.When != "" {
		if !withTask {
			return errors.New("when needs events that carry a task; reload.* events do not")
		}
		q, err := query.Parse(s.When)
		if err != nil {
			return fmt.Errorf("when: %w", err)
		}
		if err := checkWhenFields(q); err != nil {
			return fmt.Errorf("when: %w", err)
		}
		s.when = q
	}
	return nil
}

// ValidateAll normalizes and validates the top-level list, rejecting duplicate names.
func ValidateAll(specs []Spec) error { return ValidateFor(specs, "") }

// ValidateFor is ValidateAll for the `hooks:` of one backend entry ("" = the
// top-level list): it also scopes every spec to that backend, which takes
// events that carry a task only (reload.* concern the whole board).
func ValidateFor(specs []Spec, backend string) error {
	names := map[string]bool{}
	for i := range specs {
		s := &specs[i]
		s.Normalize()
		s.Backend = backend
		if err := s.Validate(); err != nil {
			return fmt.Errorf("hooks[%d]: %w", i+1, err)
		}
		if backend != "" {
			for _, e := range s.On {
				if info, _ := lookupEvent(e); !info.HasTask {
					return fmt.Errorf("hooks[%d]: %s concerns the whole board, not one backend; put this hook in the top-level hooks:", i+1, e)
				}
			}
		}
		n := strings.ToLower(s.Name)
		if names[n] {
			return fmt.Errorf("hooks[%d]: duplicate name %q", i+1, s.Name)
		}
		names[n] = true
	}
	return nil
}

// appliesTo reports whether s runs for a task of backend: a backend's hook
// only for its own tasks, a shared one unless that backend has a hook of the
// same name (which takes its place there).
func appliesTo(specs []Spec, s *Spec, backend string) bool {
	if s.Backend != "" {
		return strings.EqualFold(s.Backend, backend)
	}
	if backend == "" {
		return true
	}
	for i := range specs {
		o := &specs[i]
		if o.Backend != "" && strings.EqualFold(o.Backend, backend) && strings.EqualFold(o.Name, s.Name) {
			return false
		}
	}
	return true
}

// listens reports whether the spec wants event name.
// listensColumns reports whether s listens to an entered / left event.
func (s *Spec) listensColumns() bool {
	for _, e := range s.On {
		if column(e) != "" {
			return true
		}
	}
	return false
}

func (s *Spec) listens(name string) bool {
	info, _ := lookupEvent(name)
	for _, e := range s.On {
		if strings.EqualFold(e, name) || (info.Parent != "" && e == info.Parent) {
			return true
		}
	}
	return false
}

// checkWhenFields catches typos of the built-in fields (`priorty=high`). Any
// other field name is allowed: the config is read before any task is, and an
// extra column of a csv, a frontmatter key or a custom backend's field is only
// known once tasks are listed. At run time such a field matches the tasks that
// have it.
func checkWhenFields(q query.Query) error {
	known := query.KnownFields(nil)
	for _, f := range q.Fields() {
		if f == query.RunningField {
			return fmt.Errorf("field %q is not available in when: (only the board knows its running actions)", f)
		}
		if slices.Contains(query.DerivedFields, f) {
			return fmt.Errorf("field %q is not available in when: (an event carries one task, not the list blocked_by refers to); test blocked_by instead", f)
		}
		if slices.Contains(known, f) {
			continue
		}
		if s := query.Suggest(f, known); s != "" {
			return fmt.Errorf("unknown field %q; did you mean %q? (fields: %s; other names match extra columns of the tasks)", f, s, strings.Join(known, ", "))
		}
	}
	return nil
}
