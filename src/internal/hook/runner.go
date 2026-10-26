package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/query"
	"tack/internal/task"
)

// DepthEnv carries the nesting level to the commands a hook starts: a
// `tack` they run sees it and fires hooks only while the level is below the
// limit (DefaultMaxDepth unless configured), so a hook that moves a task
// cannot loop forever.
const DepthEnv = "TACK_HOOK_DEPTH"

// ChainEnv carries the names of the hooks that led to this process, joined
// with ChainSep ("a>b"); a hook already in the chain never fires again.
const ChainEnv = "TACK_HOOK_CHAIN"

// ChainSep joins the names in ChainEnv.
const ChainSep = ">"

// DefaultMaxDepth is the limit when none is configured: a tack started by a
// hook fires nothing.
const DefaultMaxDepth = 1

// HardMaxDepth is the highest limit that can be configured.
const HardMaxDepth = 3

// CheckMaxDepth validates a configured limit (0 = not set).
func CheckMaxDepth(n int) error {
	if n < 0 || n > HardMaxDepth {
		return fmt.Errorf("max depth %d is out of range (0 = default, 1..%d)", n, HardMaxDepth)
	}
	return nil
}

// Event is what happened.
type Event struct {
	Name   string // one of the Events() names
	Source string // tui | cli | poll
	Old    task.Task
	HasOld bool
	New    task.Task
	Action string // action events: its name
	Err    string
	Column string // column.wip_exceeded: the column; otherwise derived from the task's status
}

// subject is the task the `when:` filter and the {placeholders} see.
func (e Event) subject() task.Task {
	switch e.Name {
	case TaskDeleteSuccess, TaskDeleteFail, TaskDisappeared:
		return e.Old
	}
	return e.New
}

// Recorder takes the audit entries of hook runs (*audit.Log).
type Recorder interface{ Record(audit.Entry) }

// Runner starts the hooks that listen to an event. A nil *Runner does nothing.
type Runner struct {
	specs   []Spec
	baseDir string
	source  string
	depth   int
	max     int      // global limit
	gmax    int      // hook_max_parallel: runs at once over all hooks (0 = unlimited)
	chain   []string // hooks that led to this process
	alog    *actionlog.Log
	rec     Recorder
	wg      sync.WaitGroup

	ctx     context.Context // the runs' parent: Cancel ends them all
	cancel  context.CancelFunc
	running atomic.Int32

	mu       sync.Mutex // guards st, gactive, inflight and pruned
	st       map[*Spec]*state
	gactive  int                 // runs in flight, all hooks
	inflight map[inflightKey]int // runs in flight of a unique: task hook
	pruned   time.Time           // last prune of the dedupe markers

	bspec   board.Spec // maps statuses to columns; nil Columns = no board known
	hasSpec bool
	dueDay  string          // the day dueSeen belongs to
	dueSeen map[string]bool // tasks task.due fired for on dueDay
	wipOver map[string]bool // columns that were over their limit at the last Tick

	observe func(Event) // sees every event fired, hooks or not (see Observing)
}

// inflightKey identifies the task a unique hook is running on.
type inflightKey struct {
	s    *Spec
	task string
}

// state is what the Runner remembers of one hook (guarded by Runner.mu).
type state struct {
	active  int // runs in flight
	last    time.Time
	q       []queued // events waiting for a slot or the interval; each holds a wg token
	dropped int      // events dropped by the limits, ever
	timer   *time.Timer
}

type queued struct {
	ev     Event
	t      task.Task
	marker string // the dedupe marker this event created, removed if the event is dropped
}

// New returns a Runner, or nil when there is nothing to run. source is the
// default Event.Source ("tui" or "cli"). Specs must be validated.
func New(specs []Spec, baseDir, source string, alog *actionlog.Log, rec Recorder) *Runner {
	if len(specs) == 0 {
		return nil
	}
	depth, _ := strconv.Atoi(os.Getenv(DepthEnv))
	ctx, cancel := context.WithCancel(context.Background())
	var chain []string
	if c := os.Getenv(ChainEnv); c != "" {
		chain = strings.Split(c, ChainSep)
	}
	st := map[*Spec]*state{}
	for i := range specs {
		st[&specs[i]] = &state{}
	}
	return &Runner{specs: specs, baseDir: baseDir, source: source, depth: depth, max: DefaultMaxDepth, chain: chain, alog: alog, rec: rec, ctx: ctx, cancel: cancel, st: st, inflight: map[inflightKey]int{}}
}

// SetSource sets the Event.Source of the events that do not name one.
func (r *Runner) SetSource(s string) {
	if r != nil {
		r.source = s
	}
}

// SetObserver makes fn see every event the Runner fires, before the hooks run
// (`tack watch -json`); nil turns it off.
func (r *Runner) SetObserver(fn func(Event)) {
	if r != nil {
		r.observe = fn
	}
}

// Observing returns a Runner with no hooks that only reports its events to fn.
// It lets a process without hooks use the same code paths as one with them.
func Observing(source string, fn func(Event)) *Runner {
	r := New([]Spec{{}}, "", source, nil, nil)
	r.specs = nil
	r.observe = fn
	return r
}

// SetMaxDepth sets the global limit (0 = DefaultMaxDepth); a hook's own
// max_depth can only lower it.
func (r *Runner) SetMaxDepth(n int) {
	if r == nil {
		return
	}
	if n <= 0 {
		n = DefaultMaxDepth
	}
	r.max = n
}

// SetMaxParallel sets hook_max_parallel, the runs at once over all hooks
// (0 = unlimited); events over it follow each hook's queue mode.
func (r *Runner) SetMaxParallel(n int) {
	if r != nil {
		r.gmax = max(n, 0)
	}
}

// canFire reports whether s may fire in this process: below its depth limit
// and not already part of the chain that started the process.
func (r *Runner) canFire(s *Spec) bool {
	limit := r.max
	if s.MaxDepth > 0 && s.MaxDepth < limit {
		limit = s.MaxDepth
	}
	if r.depth >= limit {
		return false
	}
	for _, n := range r.chain {
		if strings.EqualFold(n, s.Name) {
			return false
		}
	}
	return true
}

// Running is how many started commands have not ended yet, plus the events
// still queued (Wait waits for those too).
func (r *Runner) Running() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := int(r.running.Load())
	for _, st := range r.st {
		n += len(st.q)
	}
	return n
}

// Dropped is how many events the named hook's limits have discarded.
func (r *Runner) Dropped(name string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for s, st := range r.st {
		if strings.EqualFold(s.Name, name) {
			n += st.dropped
		}
	}
	return n
}

// Cancel kills the commands still running, with their child processes. Their
// runs end as errors ("canceled") and are recorded like any other. It is for
// leaving: the Runner is finished afterwards, and a command fired later is
// canceled at once (and never retried) instead of starting.
func (r *Runner) Cancel() {
	if r == nil {
		return
	}
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.st {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		for _, q := range st.q {
			if q.marker != "" {
				os.Remove(q.marker)
			}
			r.wg.Done()
		}
		st.q = nil
	}
}

// Active reports whether anything can fire at this depth (false for nil and
// when every hook is past its limit or already in the chain).
func (r *Runner) Active() bool {
	if r == nil {
		return false
	}
	if r.observe != nil {
		return true
	}
	for i := range r.specs {
		if r.canFire(&r.specs[i]) {
			return true
		}
	}
	return false
}

// SetBoard gives the Runner the board spec, so it can tell which column a
// status belongs to (entered / left events, the column placeholders, WIP).
func (r *Runner) SetBoard(s board.Spec) {
	if r != nil {
		r.bspec, r.hasSpec = s, true
	}
}

// columnOf is the name of the column raw belongs to ("" = unknown or hidden).
func (r *Runner) columnOf(raw string, present ...string) string {
	if !r.hasSpec {
		return ""
	}
	b, err := board.New(r.bspec, append([]string{raw}, present...))
	if err != nil {
		return ""
	}
	if c := b.ColumnOf(raw); c >= 0 {
		return b.Columns[c].Name
	}
	return ""
}

// fireMoved fires task.left.<column> and task.entered.<column> when ev (an
// update, or a change seen on a reload) took the task to another column.
func (r *Runner) fireMoved(ev Event) {
	if !ev.HasOld || ev.Old.Status == ev.New.Status || !r.hasSpec {
		return
	}
	from, to := r.columnOf(ev.Old.Status, ev.New.Status), r.columnOf(ev.New.Status, ev.Old.Status)
	if from == to {
		return
	}
	if from != "" {
		ev.Name = TaskLeftPrefix + from
		r.Fire(ev)
	}
	if to != "" {
		ev.Name = TaskEnteredPrefix + to
		r.Fire(ev)
	}
}

// Tick fires the events that come from time and board state, given the tasks
// of the latest list: task.due (once per task per day) and column.wip_exceeded
// (when a column goes over its limit). Only a long-running process calls it.
func (r *Runner) Tick(tasks []task.Task, now time.Time) {
	if !r.Active() || !r.hasSpec {
		return
	}
	statuses := make([]string, len(tasks))
	for i, t := range tasks {
		statuses[i] = t.Status
	}
	b, err := board.New(r.bspec, statuses)
	if err != nil {
		return
	}
	counts := make([]int, len(b.Columns))
	var due []task.Task
	day := now.Format(task.DateLayout)
	r.mu.Lock()
	if r.dueDay != day {
		r.dueDay, r.dueSeen = day, map[string]bool{}
	}
	for _, t := range tasks {
		c := b.ColumnOf(t.Status)
		if c < 0 {
			continue
		}
		counts[c]++
		if d, ok := t.DueTime(); !ok || b.Columns[c].Done {
			continue
		} else if y, m, dd := now.Date(); d.After(time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)) {
			continue
		}
		if k := backend.Key(t.Backend, t.ID); !r.dueSeen[k] {
			r.dueSeen[k] = true
			due = append(due, t)
		}
	}
	over := map[string]bool{}
	var exceeded []string
	for i, c := range b.Columns {
		if _, o := b.Over(i, counts[i]); o {
			over[c.Name] = true
			if !r.wipOver[c.Name] {
				exceeded = append(exceeded, c.Name)
			}
		}
	}
	r.wipOver = over
	r.mu.Unlock()
	for _, t := range due {
		r.Fire(Event{Name: TaskDue, Source: "poll", New: t})
	}
	for _, name := range exceeded {
		r.Fire(Event{Name: ColumnWIPExceeded, Source: "poll", Column: name})
	}
}

// NeedsOld reports whether a hook listens to an event that needs the task
// as it was before an update.
func (r *Runner) NeedsOld() bool {
	if !r.Active() {
		return false
	}
	for i := range r.specs {
		s := &r.specs[i]
		if r.canFire(s) && (s.listens(TaskMoved) || s.listens(TaskChanged) || s.listens(TaskUpdateSuccess) || s.listensColumns()) {
			return true
		}
	}
	return false
}

// Wait blocks until the started commands have ended (each is bounded by its
// timeout), for a one-shot CLI that is about to exit.
func (r *Runner) Wait() {
	if r != nil {
		r.wg.Wait()
	}
}

// OnWrite is the backend.WriteListener: it turns a finished write into events.
func (r *Runner) OnWrite(w backend.WriteEvent) {
	if !r.Active() {
		return
	}
	ev := Event{Source: w.Source, Old: w.Old, HasOld: w.HasOld, New: w.New}
	if w.Err != nil {
		ev.Err = w.Err.Error()
	}
	var name string
	switch w.Op {
	case backend.OpCreate:
		name = TaskCreate
	case backend.OpUpdate:
		name = TaskUpdate
	case backend.OpDelete:
		name = TaskDelete
	default:
		return
	}
	if w.Err != nil {
		ev.Name = name + ".fail"
		r.Fire(ev)
		return
	}
	ev.Name = name + ".success"
	r.Fire(ev)
	if w.Op == backend.OpUpdate && w.HasOld && w.Old.Status != w.New.Status {
		ev.Name = TaskMoved
		r.Fire(ev)
		r.fireMoved(ev)
	}
}

// FireAction fires the events of an action: started (rec nil) or the end of a run.
func (r *Runner) FireAction(t task.Task, name, source string, rec *actionlog.Record) {
	if !r.Active() {
		return
	}
	ev := Event{Source: source, New: t, Action: name}
	if rec == nil {
		ev.Name = ActionStarted
		r.Fire(ev)
		return
	}
	ev.Name = ActionSucceeded
	if rec.Result != actionlog.ResultOK {
		ev.Name, ev.Err = ActionFailed, rec.Error
	}
	r.Fire(ev)
}

// Fire starts every hook that listens to ev and whose filter matches. It
// returns at once; a hook that cannot start or fails is only recorded.
func (r *Runner) Fire(ev Event) {
	if !r.Active() {
		return
	}
	if ev.Source == "" {
		ev.Source = r.source
	}
	if r.observe != nil {
		r.observe(ev)
	}
	t := ev.subject()
	for i := range r.specs {
		s := &r.specs[i]
		if !r.canFire(s) || !s.listens(ev.Name) || !appliesTo(r.specs, s, t.Backend) {
			continue
		}
		if !s.when.Empty() && !s.when.Match(query.TaskLookup(t, "", board.Deps{}), nil) {
			continue
		}
		marker, ok := r.claim(s, ev, t)
		if !ok {
			r.auditSkipped(s, queued{ev: ev, t: t}, "duplicate")
			continue
		}
		r.submit(s, ev, t, marker)
	}
}

// Find returns the hook named name that runs for the tasks of backend ("" =
// any): a backend's own hook before the shared one it replaces there.
func (r *Runner) Find(name, backend string) (*Spec, bool) {
	if r == nil {
		return nil, false
	}
	for i := range r.specs {
		s := &r.specs[i]
		if strings.EqualFold(s.Name, name) && appliesTo(r.specs, s, backend) {
			return s, true
		}
	}
	return nil, false
}

// Check says why s would not run for ev ("" = it would), as Fire decides:
// the depth limit or chain, an event it does not listen to, a when: that
// does not match the event's task. The limits (max_parallel, ...) are not
// checked; they only delay or drop a run.
func (r *Runner) Check(s *Spec, ev Event) string {
	switch {
	case !r.canFire(s):
		return fmt.Sprintf("it does not fire at hook depth %d (hook_max_depth / max_depth, or already in %s=%s)", r.depth, ChainEnv, strings.Join(r.chain, ChainSep))
	case !s.listens(ev.Name):
		return fmt.Sprintf("it does not listen to %s (on: %s)", ev.Name, strings.Join(s.On, ", "))
	case !s.when.Empty() && !s.when.Match(query.TaskLookup(ev.subject(), "", board.Deps{}), nil):
		return fmt.Sprintf("when: %s does not match the task", s.When)
	}
	return ""
}

// Masked replaces the value of a variable the hook sets in env: (often a token).
const Masked = "***"

// Preview is the command s would run for ev, with the values of its env:
// masked.
func (r *Runner) Preview(s *Spec, ev Event) action.Plan {
	if ev.Source == "" {
		ev.Source = r.source
	}
	_, p := r.plan(s, ev, ev.subject())
	for i, kv := range p.Env {
		if k, _, _ := strings.Cut(kv, "="); s.Env[k] != "" {
			p.Env[i] = k + "=" + Masked
		}
	}
	return p
}

// Test runs s once for ev and waits for it, recorded like a fired run (action
// log, audit trail) and with the same depth and chain variables. It does not
// check s (call Check first) and ignores its limits and retries: a run by
// hand is not one of a burst.
func (r *Runner) Test(s *Spec, ev Event) actionlog.Record {
	if ev.Source == "" {
		ev.Source = r.source
	}
	t := ev.subject()
	spec, p := r.plan(s, ev, t)
	j := r.launch(spec, t, p)
	<-j.Done()
	rec := actionlog.FromJob(j, "hook")
	rec.Event = ev.Name
	r.audit(s, ev, t, rec, 1)
	_ = r.alog.Append(rec)
	return rec
}

// blocked says why s cannot start now ("" = it can): "running" when it is at
// max_parallel, "global limit" at hook_max_parallel, "too soon" within min_interval of its last start. r.mu held.
func (r *Runner) blocked(s *Spec, st *state, now time.Time) string {
	if r.gmax > 0 && r.gactive >= r.gmax {
		return "global limit"
	}
	if p := s.parallel(); p > 0 && st.active >= p {
		return "running"
	}
	if iv := s.interval(); iv > 0 && !st.last.IsZero() && now.Sub(st.last) < iv {
		return "too soon"
	}
	return ""
}

// begin books a run of s on t. r.mu held.
func (r *Runner) begin(s *Spec, st *state, t task.Task, now time.Time) {
	st.active++
	st.last = now
	r.gactive++
	if s.Unique == UniqueTask {
		r.inflight[inflightKey{s, backend.Key(t.Backend, t.ID)}]++
	}
}

// busy reports whether s is already running on t (unique: task). r.mu held.
func (r *Runner) busy(s *Spec, t task.Task) bool {
	return s.Unique == UniqueTask && r.inflight[inflightKey{s, backend.Key(t.Backend, t.ID)}] > 0
}

// submit starts the event, or applies the hook's queue mode when a limit
// stops it.
func (r *Runner) submit(s *Spec, ev Event, t task.Task, marker string) {
	now := time.Now()
	r.mu.Lock()
	st := r.st[s]
	if r.busy(s, t) {
		st.dropped++
		r.mu.Unlock()
		r.auditSkipped(s, queued{ev, t, marker}, "already running")
		return
	}
	reason := r.blocked(s, st, now)
	if reason == "" && len(st.q) == 0 {
		r.begin(s, st, t, now)
		r.mu.Unlock()
		r.start(s, ev, t, false)
		return
	}
	if reason == "" {
		reason = "running"
	}
	var lost []queued
	lostReason := reason
	switch s.queueMode() {
	case QueueLatest:
		lost = st.q
		st.q = []queued{{ev, t, marker}}
		r.wg.Add(1)
		lostReason = "superseded"
	case QueueAll:
		if len(st.q) >= MaxQueue {
			lost, lostReason = []queued{{ev, t, marker}}, "queue full"
			break
		}
		st.q = append(st.q, queued{ev, t, marker})
		r.wg.Add(1)
	default:
		lost = []queued{{ev, t, marker}}
	}
	st.dropped += len(lost)
	for range lost {
		if lostReason == "superseded" {
			r.wg.Done() // the replaced event gave up its token
		}
	}
	r.mu.Unlock()
	for _, q := range lost {
		r.auditSkipped(s, q, lostReason)
	}
	r.pump(s)
}

// auditSkipped records a dropped event, except for reload.* (every refresh
// would flood the trail).
func (r *Runner) auditSkipped(s *Spec, q queued, reason string) {
	if q.marker != "" { // nobody runs it here: another process may take the event
		os.Remove(q.marker)
	}
	if r.rec == nil || strings.HasPrefix(q.ev.Name, "reload.") {
		return
	}
	r.rec.Record(audit.Entry{Op: audit.HookOp(q.ev.Name), Backend: q.t.Backend, ID: q.t.ID, Result: audit.ResultSkipped,
		Details: s.Name + ": skipped (" + reason + ")"})
}

// pump starts the queued events that the limits now allow and, when only
// min_interval holds the rest back, arms a timer for the first free moment.
func (r *Runner) pump(s *Spec) {
	r.mu.Lock()
	st := r.st[s]
	var out, dup []queued
	now := time.Now()
	for len(st.q) > 0 {
		q := st.q[0]
		if r.busy(s, q.t) { // a run for this task started while it waited
			st.q = st.q[1:]
			st.dropped++
			dup = append(dup, q)
			r.wg.Done()
			continue
		}
		if r.blocked(s, st, now) != "" {
			break
		}
		out = append(out, q)
		st.q = st.q[1:]
		r.begin(s, st, q.t, now)
	}
	if len(st.q) == 0 {
		st.q = nil
	} else if st.timer == nil && r.blocked(s, st, now) == "too soon" {
		st.timer = time.AfterFunc(max(st.last.Add(s.interval()).Sub(now), time.Millisecond), func() {
			r.mu.Lock()
			st.timer = nil
			r.mu.Unlock()
			r.pump(s)
		})
	}
	r.mu.Unlock()
	for _, q := range dup {
		r.auditSkipped(s, q, "already running")
	}
	for _, q := range out {
		r.start(s, q.ev, q.t, true)
	}
}

// plan is the command s runs for ev on t: the action's expansion plus the
// event's placeholders, variables and stdin. It returns the spec to run it
// with: s's action carrying a run id of its own (TACK_RUN_ID, and
// TACK_ACTION_LOG when the action log is on), as `tack run` gives its runs, so
// an agent started by a hook can `tack report -run` and the record of the run
// has a run_id. A retry of the run keeps the id: its records share it.
func (r *Runner) plan(s *Spec, ev Event, t task.Task) (action.Spec, action.Plan) {
	spec := s.Spec.WithRun(actionlog.NewRunID(), r.alog.Path())
	oldJSON, _ := json.Marshal(ev.Old)
	if !ev.HasOld {
		oldJSON = nil
	}
	col, oldCol := ev.Column, ""
	if col == "" {
		col = r.columnOf(t.Status)
	}
	if ev.HasOld {
		oldCol = r.columnOf(ev.Old.Status)
	}
	extra := map[string]string{"event": ev.Name, "source": ev.Source, "error": ev.Err, "old_status": ev.Old.Status,
		"old_json": string(oldJSON), "action": ev.Action, "column": col, "old_column": oldCol, "new_status": ev.New.Status}
	p := spec.ExpandWith(t, r.baseDir, extra)
	switch s.Stdin {
	case action.StdinEvent:
		p.Stdin = eventJSON(ev, t)
	case action.StdinEventMarkdown:
		p.Stdin = eventMarkdown(ev, t)
	}
	p.Env = append(p.Env,
		action.EnvVar("TACK_EVENT", ev.Name),
		action.EnvVar("TACK_EVENT_SOURCE", ev.Source),
		action.EnvVar("TACK_EVENT_ERROR", ev.Err),
		action.EnvVar("TACK_OLD_STATUS", ev.Old.Status),
		action.EnvVar("TACK_OLD_TASK_JSON", string(oldJSON)),
		action.EnvVar("TACK_ACTION", ev.Action),
		action.EnvVar("TACK_COLUMN", col),
		action.EnvVar("TACK_OLD_COLUMN", oldCol),
		action.EnvVar("TACK_NEW_STATUS", ev.New.Status),
		DepthEnv+"="+strconv.Itoa(r.depth+1),
		ChainEnv+"="+strings.Join(append(slices.Clone(r.chain), s.Name), ChainSep),
	)
	return spec, p
}

// start runs one event; queuedToken says it already holds a wg token.
func (r *Runner) start(s *Spec, ev Event, t task.Task, queuedToken bool) {
	spec, p := r.plan(s, ev, t)
	launch := func() *action.Job { return r.launch(spec, t, p) }
	j := launch()
	if !queuedToken {
		r.wg.Add(1)
	}
	r.running.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.running.Add(-1)
		defer func() {
			r.mu.Lock()
			r.st[s].active--
			r.gactive--
			if s.Unique == UniqueTask {
				k := inflightKey{s, backend.Key(t.Backend, t.ID)}
				if r.inflight[k]--; r.inflight[k] <= 0 {
					delete(r.inflight, k)
				}
			}
			r.mu.Unlock()
			r.pump(s)
			if r.gmax > 0 { // the freed slot may belong to another hook's queue
				for i := range r.specs {
					if o := &r.specs[i]; o != s {
						r.pump(o)
					}
				}
			}
		}()
		for attempt := 1; ; attempt++ {
			<-j.Done()
			rec := actionlog.FromJob(j, "hook")
			rec.Event = ev.Name
			if rec.Result != actionlog.ResultOK && attempt <= s.Retries && r.ctx.Err() == nil {
				_ = r.alog.Append(rec) // every attempt is in the action log, only the last in the audit trail
				if r.pause(s.retryDelay()) {
					j = launch()
					continue
				}
				r.audit(s, ev, t, rec, attempt)
				return
			}
			r.audit(s, ev, t, rec, attempt)
			_ = r.alog.Append(rec)
			return
		}
	}()
}

// launch starts p, the plan of spec for t.
func (r *Runner) launch(spec action.Spec, t task.Task, p action.Plan) *action.Job {
	return spec.StartPlan(r.ctx, t, p)
}

// pause waits d before a retry; false when the Runner was canceled meanwhile.
func (r *Runner) pause(d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return r.ctx.Err() == nil
	case <-r.ctx.Done():
		return false
	}
}

// audit records the end of a hook run; attempt > 1 says it was retried.
func (r *Runner) audit(s *Spec, ev Event, t task.Task, rec actionlog.Record, attempt int) {
	if r.rec == nil {
		return
	}
	res := audit.ResultOK
	if rec.Result != actionlog.ResultOK {
		res = audit.ResultError
	}
	details := s.Name + ": " + strings.TrimPrefix(rec.Summary(), "action "+s.Name+" ")
	if attempt > 1 {
		details += fmt.Sprintf(" (attempt %d of %d)", attempt, s.Retries+1)
	}
	r.rec.Record(audit.Entry{Op: audit.HookOp(ev.Name), Backend: t.Backend, ID: t.ID, Result: res, Details: details})
}

// Diff compares two lists of one reload and fires appeared / disappeared / moved / changed. own
// holds the tasks this process wrote itself (backend.Key -> the task exists
// after that write), which are not news unless the list says otherwise. skip
// names the backends whose list is incomplete. A key in both lists whose
// fingerprint changed is an ID reused after a delete: it disappeared and a new
// task appeared.
func (r *Runner) Diff(before, after []task.Task, own map[string]bool, skip map[string]bool) {
	if !r.Active() {
		return
	}
	was, is := map[string]task.Task{}, map[string]bool{}
	for _, t := range before {
		was[backend.Key(t.Backend, t.ID)] = t
	}
	var reused []task.Task // the old tasks of reused ids, reported before the new ones
	var appeared []task.Task
	var moved []Event   // same task, another status: a change made outside this run
	var changed []Event // same task, any field differs: task.changed
	for _, t := range after {
		k := backend.Key(t.Backend, t.ID)
		is[k] = true
		if present, mine := own[k]; (mine && present) || skip[t.Backend] {
			continue
		}
		old, ok := was[k]
		if ok && fingerprint(old) != fingerprint(t) {
			reused = append(reused, old)
			ok = false
		}
		if !ok {
			appeared = append(appeared, t)
		} else {
			ev := Event{Source: "poll", Old: old, HasOld: true, New: t}
			if old.Status != t.Status {
				moved = append(moved, ev)
			}
			if !sameTask(old, t) {
				changed = append(changed, ev)
			}
		}
	}
	for _, t := range before {
		k := backend.Key(t.Backend, t.ID)
		if present, mine := own[k]; !is[k] && !(mine && !present) && !skip[t.Backend] {
			r.Fire(Event{Name: TaskDisappeared, Source: "poll", Old: t, HasOld: true})
		}
	}
	for _, t := range reused {
		r.Fire(Event{Name: TaskDisappeared, Source: "poll", Old: t, HasOld: true})
	}
	for _, t := range appeared {
		r.Fire(Event{Name: TaskAppeared, Source: "poll", New: t})
	}
	for _, ev := range moved {
		ev.Name = TaskMoved
		r.Fire(ev)
		r.fireMoved(ev)
	}
	for _, ev := range changed {
		ev.Name = TaskChanged
		r.Fire(ev)
	}
}

// sameTask reports whether two versions of a task hold the same data. The
// claim bookkeeping (lease, claimed_at) and the overlay marks are not a change
// somebody made, so a lease renewal fires nothing.
func sameTask(a, b task.Task) bool {
	a.Lease, b.Lease, a.ClaimedAt, b.ClaimedAt = "", "", "", ""
	a.Overlaid, b.Overlaid = nil, nil
	ja, _ := json.Marshal(a) // map keys marshal sorted
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// fingerprint tells two tasks with one ID apart: the title, plus the created
// field when the store keeps one.
func fingerprint(t task.Task) string { return t.Title + "\x00" + t.Extra["created"] }

// DedupeDir is where dedupe markers live, in the config directory.
const DedupeDir = ".tack-dedupe"

// dedupeTTL is how long a marker holds; older ones are pruned so the same
// task can fire again later.
const dedupeTTL = 10 * time.Minute

// claim reports whether this process may run s for ev: with dedupe on a poll
// event it creates a marker file with O_EXCL, and another process that already
// created it wins. Errors other than "exists" let the event through. marker is
// the file this call created ("" when none), for the caller to remove if the
// event is then dropped by a limit, so that another process can still run it.
func (r *Runner) claim(s *Spec, ev Event, t task.Task) (marker string, ok bool) {
	if !s.Dedupe || (ev.Name != TaskAppeared && ev.Name != TaskDisappeared) {
		return "", true
	}
	dir := filepath.Join(r.baseDir, DedupeDir)
	r.prune(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", true
	}
	h := sha256.Sum256([]byte(strings.Join([]string{s.Name, ev.Name, t.Backend, t.ID, fingerprint(t)}, "\x00")))
	path := filepath.Join(dir, hex.EncodeToString(h[:16]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		f.Close()
		return path, true
	}
	return "", !os.IsExist(err)
}

// prune removes markers older than dedupeTTL, at most once a minute.
func (r *Runner) prune(dir string) {
	now := time.Now()
	r.mu.Lock()
	due := now.Sub(r.pruned) >= time.Minute
	if due {
		r.pruned = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > dedupeTTL {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// eventJSON is stdin: event, one JSON line. error and action are left out when
// empty, old when the event has no previous task; task is the event's subject.
func eventJSON(ev Event, t task.Task) string {
	d := struct {
		Event  string     `json:"event"`
		Source string     `json:"source"`
		Error  string     `json:"error,omitempty"`
		Action string     `json:"action,omitempty"`
		Old    *task.Task `json:"old,omitempty"`
		Task   task.Task  `json:"task"`
	}{Event: ev.Name, Source: ev.Source, Error: ev.Err, Action: ev.Action, Task: t}
	if ev.HasOld {
		d.Old = &ev.Old
	}
	b, err := json.Marshal(d)
	if err != nil { // strings only; cannot fail
		return "{}\n"
	}
	return string(b) + "\n"
}

// EventJSON is the event as the hooks' `stdin: event` gets it: one JSON line.
func EventJSON(ev Event) string { return eventJSON(ev, ev.subject()) }

// eventMarkdown is stdin: event-markdown: the event as a list (empty fields
// left out), a blank line, then the task as stdin: markdown renders it.
func eventMarkdown(ev Event, t task.Task) string {
	var b strings.Builder
	b.WriteString("- event: " + ev.Name + "\n- source: " + ev.Source + "\n")
	if ev.Action != "" {
		b.WriteString("- action: " + ev.Action + "\n")
	}
	if ev.Err != "" {
		b.WriteString("- error: " + ev.Err + "\n")
	}
	if ev.HasOld {
		b.WriteString("- old status: " + ev.Old.Status + "\n")
	}
	b.WriteString("\n" + action.TaskMarkdown(t))
	return b.String()
}
