// Package custom is a backend that delegates every operation to an external
// command. tkan starts the command once per operation, writes one JSON
// request to its stdin and reads one JSON response from its stdout.
//
//	{"op":"list"}                      -> {"tasks":[Task...], "statuses":["..."]}
//	{"op":"create","task":{...}}       -> {"task":{... with "id"}}
//	{"op":"update","task":{...}}       -> {}
//	{"op":"delete","id":"7"}           -> {}
//
// A failure is a non-zero exit status or {"error":"message","code":"..."};
// code "not_found" (or exit status 3) maps to "task not found" and
// "unsupported" to "operation not supported". Task is the JSON shape printed by
// `tkan list -json` (id, title, description, status, priority, notes, type,
// assignee, due, start, labels, extra). IDs must be non-empty and unique within
// one list. See docs/backends.md.
package custom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"time"

	"tkan/internal/backend"
	"tkan/internal/task"
)

func init() { backend.RegisterType("custom", Open) }

const (
	defaultTimeout = 10 * time.Second
	maxStdout      = 32 << 20 // a command that prints more than this is treated as broken
	maxStderr      = 64 << 10 // only the start of stderr is kept for error messages
)

// capWriter keeps at most max bytes and silently drops the rest, so a runaway
// command cannot exhaust memory (writes never fail, the command keeps running
// until it exits or times out).
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p) // always report everything as written, or exec fails with a short write
	if room := w.max - w.buf.Len(); len(p) > room {
		w.truncated = true
		p = p[:max(room, 0)]
	}
	w.buf.Write(p)
	return n, nil
}

type options struct {
	Command []string          `yaml:"command"`
	Cwd     string            `yaml:"cwd"`
	Timeout string            `yaml:"timeout"`
	Poll    string            `yaml:"poll"`
	Env     map[string]string `yaml:"env"`
}

type request struct {
	Op   string     `json:"op"`
	Task *task.Task `json:"task,omitempty"`
	ID   string     `json:"id,omitempty"`
}

type response struct {
	Error    string      `json:"error"`
	Code     string      `json:"code"`
	Tasks    []task.Task `json:"tasks"`
	Statuses []string    `json:"statuses"`
	Task     *task.Task  `json:"task"`
}

// Backend implements backend.Backend by running the configured command.
type Backend struct {
	command []string
	dir     string
	env     []string
	timeout time.Duration
	poll    time.Duration // 0: never poll in the background

	mu       sync.Mutex
	statuses []string
	primed   []task.Task // the list Open fetched; handed to the first List call
	hasPrime bool
	primedAt time.Time

	// Background polling (only when poll > 0), started by the first Changed call.
	seen     snapshot // what List last returned
	hasSeen  bool
	dirty    bool  // a poll saw something different from seen
	pollErr  error // the last poll failure, reported once
	started  bool
	cancel   context.CancelFunc
	pollDone chan struct{}
}

// snapshot is a fetched list with its statuses, compared to spot changes.
type snapshot struct {
	Tasks    []task.Task
	Statuses []string
}

// Open validates the options and runs one `list` to verify the command works.
func Open(spec backend.Spec) (backend.Backend, error) {
	var o options
	if spec.Options.Kind != 0 {
		if err := spec.Options.Decode(&o); err != nil {
			return nil, fmt.Errorf("custom options: %w", err)
		}
	}
	if len(o.Command) == 0 || strings.TrimSpace(o.Command[0]) == "" {
		return nil, errors.New("custom options: command is required (a list: program and arguments)")
	}
	b := &Backend{command: o.Command, dir: spec.BaseDir, timeout: defaultTimeout}
	if o.Cwd != "" {
		b.dir = spec.Resolve(o.Cwd)
	}
	if o.Timeout != "" {
		d, err := time.ParseDuration(o.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("custom options: invalid timeout %q (e.g. 10s)", o.Timeout)
		}
		b.timeout = d
	}
	if o.Poll != "" {
		d, err := time.ParseDuration(o.Poll)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("custom options: invalid poll %q (e.g. 5s)", o.Poll)
		}
		b.poll = d
	}
	for k, v := range o.Env {
		b.env = append(b.env, k+"="+v)
	}
	first, err := b.List(context.Background())
	if err != nil {
		return nil, err
	}
	// Open's list serves the next List call if it comes right away, so
	// starting up does not run the command twice.
	b.primed, b.hasPrime, b.primedAt = first, true, time.Now()
	return b, nil
}

func (b *Backend) call(ctx context.Context, req request) (response, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.command[0], b.command[1:]...)
	cmd.Dir = b.dir
	cmd.Env = append(os.Environ(), b.env...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.WaitDelay = time.Second // do not hang on grandchildren that keep the pipes open
	killGroupOnCancel(cmd)
	stdout, stderr := &capWriter{max: maxStdout}, &capWriter{max: maxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	killLeftovers(cmd) // children a wrapper left behind must not outlive the call
	// A command that exited 0 but left a background child holding the pipes open
	// is not a failure: what it printed is judged by the JSON below.
	if errors.Is(runErr, exec.ErrWaitDelay) {
		runErr = nil
	}
	if stdout.truncated {
		return response{}, fmt.Errorf("custom %s: output exceeds %d MiB", req.Op, maxStdout>>20)
	}

	var resp response
	decErr := json.Unmarshal(bytes.TrimSpace(stdout.buf.Bytes()), &resp)
	if resp.Error != "" { // a structured error wins over the exit status
		return resp, protocolError(resp)
	}
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return resp, fmt.Errorf("custom %s: command timed out after %s", req.Op, b.timeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) { // the caller gave up (for example the TUI quit)
			return resp, fmt.Errorf("custom %s: %w", req.Op, context.Canceled)
		}
		msg := strings.TrimSpace(stderr.buf.String())
		if msg == "" {
			msg = runErr.Error()
		}
		var exit *exec.ExitError
		if errors.As(runErr, &exit) && exit.ExitCode() == 3 { // documented "not found" status
			return resp, fmt.Errorf("%w: custom %s: %s", backend.ErrNotFound, req.Op, msg)
		}
		return resp, fmt.Errorf("custom %s: %s", req.Op, msg)
	}
	if decErr != nil {
		return resp, fmt.Errorf("custom %s: invalid JSON response: %v", req.Op, decErr)
	}
	return resp, nil
}

func protocolError(r response) error {
	switch r.Code {
	case "not_found":
		return fmt.Errorf("%w: %s", backend.ErrNotFound, r.Error)
	case "unsupported":
		return fmt.Errorf("%w: %s", backend.ErrUnsupported, r.Error)
	}
	return errors.New(r.Error)
}

// primedMaxAge bounds how long Open's list may stand in for a fresh one.
const primedMaxAge = 2 * time.Second

func (b *Backend) List(ctx context.Context) ([]task.Task, error) {
	b.mu.Lock()
	if b.hasPrime {
		p, fresh := b.primed, time.Since(b.primedAt) < primedMaxAge
		b.primed, b.hasPrime = nil, false
		if fresh {
			b.mu.Unlock()
			return p, nil
		}
	}
	b.mu.Unlock()
	snap, err := b.fetch(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.statuses = snap.Statuses
	b.seen, b.hasSeen, b.dirty = snap, true, false
	b.mu.Unlock()
	return snap.Tasks, nil
}

// fetch runs one `list` and validates the answer.
func (b *Backend) fetch(ctx context.Context) (snapshot, error) {
	resp, err := b.call(ctx, request{Op: "list"})
	if err != nil {
		return snapshot{}, err
	}
	// `[]` decodes to a non-nil empty slice, a missing (or null) key to nil: an
	// author's typo like {"task":[...]} must not look like an empty board.
	if resp.Tasks == nil {
		return snapshot{}, errors.New(`custom list: response must contain a "tasks" array (use [] for no tasks)`)
	}
	ids := map[string]bool{}
	for i, t := range resp.Tasks {
		switch {
		case t.ID == "":
			return snapshot{}, fmt.Errorf("custom list: task %d has no id", i+1)
		case ids[t.ID]:
			return snapshot{}, fmt.Errorf("custom list: duplicate id %q", t.ID)
		}
		ids[t.ID] = true
	}
	statuses := resp.Statuses
	if statuses == nil {
		seen := map[string]bool{}
		for _, t := range resp.Tasks {
			if t.Status != "" && !seen[t.Status] {
				seen[t.Status] = true
				statuses = append(statuses, t.Status)
			}
		}
	}
	for i := range resp.Tasks {
		resp.Tasks[i].Backend = ""
	}
	return snapshot{Tasks: resp.Tasks, Statuses: statuses}, nil
}

// Statuses returns the statuses reported by (or derived from) the last list.
func (b *Backend) Statuses() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.statuses...)
}

func (b *Backend) Update(ctx context.Context, t task.Task) error {
	t.Backend = ""
	_, err := b.call(ctx, request{Op: "update", Task: &t})
	return err
}

func (b *Backend) Create(ctx context.Context, t task.Task) (task.Task, error) {
	t.Backend = ""
	resp, err := b.call(ctx, request{Op: "create", Task: &t})
	if err != nil {
		return task.Task{}, err
	}
	if resp.Task == nil || resp.Task.ID == "" {
		return task.Task{}, errors.New(`custom create: response must contain "task" with an "id"`)
	}
	return *resp.Task, nil
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	_, err := b.call(ctx, request{Op: "delete", ID: id})
	return err
}

// Changed implements backend.Watcher. Without the poll option it is always
// false. Otherwise the first call starts a goroutine that runs `list` every
// poll interval; Changed itself never runs the command (the TUI calls it from
// its event loop) and only reports what the goroutine found since the last List.
func (b *Backend) Changed() (bool, error) {
	if b.poll <= 0 {
		return false, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		b.started = true
		ctx, cancel := context.WithCancel(context.Background())
		b.cancel, b.pollDone = cancel, make(chan struct{})
		go b.pollLoop(ctx, b.pollDone)
	}
	changed, err := b.dirty, b.pollErr
	b.dirty, b.pollErr = false, nil
	return changed, err
}

func (b *Backend) pollLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(b.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		snap, err := b.fetch(ctx)
		if ctx.Err() != nil {
			return
		}
		b.mu.Lock()
		switch {
		case err != nil:
			b.pollErr = err
		case b.hasSeen && !reflect.DeepEqual(snap, b.seen):
			b.dirty = true
		}
		b.mu.Unlock()
	}
}

// Close stops the poller, killing a poll command that is still running.
func (b *Backend) Close() error {
	b.mu.Lock()
	cancel, done := b.cancel, b.pollDone
	b.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}
