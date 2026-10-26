package action

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"tack/internal/proc"
	"tack/internal/task"
)

// busy holds the cards (action name, backend, id) with a running `exclusive`
// background action. It covers this process only.
var busy sync.Map

// Result is what a background run printed (trimmed, possibly cut short).
type Result struct {
	Stdout string
	Stderr string
}

// Job is a background run in progress or finished. It is safe to read from
// another goroutine while the command runs.
type Job struct {
	Spec    Spec
	Task    task.Task
	Plan    Plan // what was run; Plan.Env is deliberately never logged
	Started time.Time

	cancel  context.CancelFunc
	release func() // frees the exclusive claim on the card
	done    chan struct{}
	stdout  *proc.LiveWriter
	stderr  *proc.LiveWriter

	mu       sync.Mutex
	ended    time.Time
	err      error
	canceled bool
	exit     int
}

// Snapshot is a consistent view of a Job.
type Snapshot struct {
	Running   bool
	Started   time.Time
	Ended     time.Time // zero while running
	Err       error
	Canceled  bool
	ExitCode  int // the command's exit status; -1 when it did not exit by itself (killed, not started)
	Stdout    string
	Stderr    string
	Truncated bool // some output was dropped
}

// Elapsed is the run time so far, or the total once it has ended.
func (s Snapshot) Elapsed(now time.Time) time.Duration {
	if s.Running {
		return now.Sub(s.Started)
	}
	return s.Ended.Sub(s.Started)
}

// Done is closed when the command has ended.
func (j *Job) Done() <-chan struct{} { return j.done }

// Cancel kills the command and its child processes. It is a no-op once ended.
func (j *Job) Cancel() {
	j.mu.Lock()
	if j.ended.IsZero() {
		j.canceled = true
	}
	j.mu.Unlock()
	j.cancel()
}

// Snapshot returns the state and the output so far.
func (j *Job) Snapshot() Snapshot {
	out, t1 := j.stdout.String()
	errOut, t2 := j.stderr.String()
	j.mu.Lock()
	defer j.mu.Unlock()
	return Snapshot{Running: j.ended.IsZero(), Started: j.Started, Ended: j.ended, Err: j.err, Canceled: j.canceled, ExitCode: j.exit,
		Stdout: out, Stderr: errOut, Truncated: t1 || t2}
}

// Start launches the action without the terminal and returns at once. The run
// ends by itself, by the action's timeout, by Cancel or when ctx is done.
func (s Spec) Start(ctx context.Context, t task.Task, baseDir string) *Job {
	return s.StartPlan(ctx, t, s.Expand(t, baseDir))
}

// StartPlan is Start for a plan the caller expanded (hooks add their own
// placeholders and environment).
func (s Spec) StartPlan(ctx context.Context, t task.Task, p Plan) *Job {
	return s.startPlan(ctx, t, p, nil, nil)
}

// StartStreaming is Start that also copies the command's stdout and stderr to
// the given writers as they arrive (either may be nil). The job keeps its own
// capped copy, so the cap does not apply to what is streamed. The writers are
// called from two goroutines unless they are the same non-file value, so they
// must be safe for concurrent use.
func (s Spec) StartStreaming(ctx context.Context, t task.Task, baseDir string, stdout, stderr io.Writer) *Job {
	return s.startPlan(ctx, t, s.Expand(t, baseDir), stdout, stderr)
}

func (s Spec) startPlan(ctx context.Context, t task.Task, p Plan, tee1, tee2 io.Writer) *Job {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	j := &Job{Spec: s, Task: t, Plan: p, Started: time.Now(), exit: -1, cancel: cancel, done: make(chan struct{}),
		stdout: proc.NewLiveWriter(s.maxOutput()), stderr: proc.NewLiveWriter(s.maxOutput())}
	if p.Err == nil {
		p.Err = s.checkDir(p.Dir)
	}
	if p.Err == nil && s.Exclusive {
		key := s.Name + "\x00" + t.Backend + "\x00" + t.ID
		if _, loaded := busy.LoadOrStore(key, true); loaded {
			p.Err = fmt.Errorf("%s: already running on #%s (exclusive)", s.Name, t.ID)
		} else {
			j.release = func() { busy.Delete(key) }
		}
	}
	if p.Err != nil { // cannot be started as expanded: report it as the run's error
		if j.release != nil {
			j.release()
		}
		j.ended, j.err = j.Started, p.Err
		close(j.done)
		cancel()
		return j
	}
	cmd := exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...)
	cmd.Dir = p.Dir
	cmd.Env = p.environ()
	if p.Stdin != "" {
		cmd.Stdin = strings.NewReader(p.Stdin)
	}
	cmd.WaitDelay = time.Second // do not hang on grandchildren that keep the pipes open
	proc.KillGroupOnCancel(cmd, s.Grace())
	cmd.Stdout, cmd.Stderr = io.Writer(j.stdout), io.Writer(j.stderr)
	if tee1 != nil {
		cmd.Stdout = io.MultiWriter(j.stdout, tee1)
	}
	if tee2 != nil {
		cmd.Stderr = io.MultiWriter(j.stderr, tee2)
	}
	go func() {
		defer close(j.done)
		defer cancel()
		if j.release != nil {
			defer j.release()
		}
		runErr := cmd.Run()
		proc.KillLeftovers(cmd)
		// Exit 0 with a background child holding the pipes open is a success.
		if errors.Is(runErr, exec.ErrWaitDelay) {
			runErr = nil
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		j.ended = time.Now()
		if runErr == nil {
			j.exit = 0
			j.canceled = false // a Cancel that lost the race with a successful exit did not cancel anything
			return
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			j.exit = ee.ExitCode()
		}
		errOut, _ := j.stderr.String()
		msg := firstLine(strings.TrimSpace(errOut))
		if msg == "" {
			msg = runErr.Error()
		}
		j.err = s.endErr(ctx, j.canceled, msg)
	}()
	return j
}

// endErr is the error of a run that did not succeed: canceled, timed out, or
// msg (the reason it failed by itself).
func (s Spec) endErr(ctx context.Context, canceled bool, msg string) error {
	switch {
	case canceled:
		return fmt.Errorf("%s: canceled", s.Name)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%s: timed out after %s", s.Name, s.timeout())
	case errors.Is(ctx.Err(), context.Canceled):
		return fmt.Errorf("%s: %w", s.Name, context.Canceled)
	}
	return fmt.Errorf("%s: %s", s.Name, msg)
}

// Run executes the action and waits for it (the CLI's path).
func (s Spec) Run(ctx context.Context, t task.Task, baseDir string) (Result, error) {
	j := s.Start(ctx, t, baseDir)
	<-j.Done()
	snap := j.Snapshot()
	return Result{Stdout: strings.TrimSpace(snap.Stdout), Stderr: strings.TrimSpace(snap.Stderr)}, snap.Err
}

// Cmd builds the command for an interactive run: it inherits the terminal
// (tea.ExecProcess or the CLI wires stdin/stdout). The stdin option is not
// used here, so an action that needs it cannot run interactively. done
// releases the timeout timer: call it once the command has ended (it is a
// no-op without interactive_timeout, and nil only together with an error).
func (s Spec) Cmd(t task.Task, baseDir string) (cmd *exec.Cmd, done func(), err error) {
	if s.Stdin != StdinNone && s.Stdin != "" {
		return nil, nil, fmt.Errorf("%s: reads the task on stdin (stdin: %s), which cannot be combined with interactive mode", s.Name, s.Stdin)
	}
	p := s.Expand(t, baseDir)
	if p.Err == nil {
		p.Err = s.checkDir(p.Dir)
	}
	if p.Err != nil {
		return nil, nil, p.Err
	}
	done = func() {}
	if d, err := time.ParseDuration(s.InteractiveTimeout); err == nil && d > 0 {
		// No process group: the command must stay in the terminal's foreground
		// group. SIGTERM goes to the command itself; Wait kills it after the grace.
		var ctx context.Context
		ctx, done = context.WithTimeout(context.Background(), d)
		cmd = exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...)
		cmd.Cancel = func() error {
			cmd.WaitDelay = s.Grace()
			if s.Grace() > 0 && cmd.Process.Signal(syscall.SIGTERM) == nil {
				return nil
			}
			return cmd.Process.Kill()
		}
	} else {
		cmd = exec.Command(p.Argv[0], p.Argv[1:]...)
	}
	cmd.Dir = p.Dir
	cmd.Env = p.environ()
	return cmd, done, nil
}

// checkDir creates the working directory when create_workdir is set, else names a missing one; exec reports it as the program
// not being found, which sends people looking in the wrong place.
func (s Spec) checkDir(dir string) error {
	if s.CreateWorkdir {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%s: cannot create working directory %q: %w", s.Name, dir, err)
		}
		return nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s: working directory %q does not exist (workdir is relative to the config file)", s.Name, dir)
	}
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
