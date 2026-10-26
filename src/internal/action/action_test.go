package action

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"tack/internal/task"
)

var sample = task.Task{ID: "7", Title: "Fix \"quotes\" & $(rm -rf /)", Description: "line1\nline2 {id}", Status: "todo", Backend: "csv1", Labels: "a,b"}

func spec(cmd ...string) Spec {
	s := Spec{Name: "x", Command: cmd}
	s.Normalize()
	return s
}

func TestExpandIsSinglePassAndNeverShellParsed(t *testing.T) {
	p := spec("echo", "#{id}: {title}", "{description}", "{}", "{ id }").Expand(sample, "/cfg")
	want := []string{"echo", "#7: Fix \"quotes\" & $(rm -rf /)", "line1\nline2 {id}", "{}", "{ id }"}
	if strings.Join(p.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q", p.Argv)
	}
	if p.Dir != "/cfg" {
		t.Fatalf("dir = %q", p.Dir)
	}
}

func TestBracesInShellAwkAndJqTextStayLiteral(t *testing.T) {
	args := []string{"awk '{print $1}'", "${HOME}/x", ".a|{x}", "{ id }", "{{id}}", "a}}b", "{{{id}}}", "{HOME}"}
	s := spec(append([]string{"sh"}, args...)...)
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	got := s.Expand(sample, "/cfg").Argv[1:]
	want := []string{"awk '{print $1}'", "${HOME}/x", ".a|{x}", "{ id }", "{id}", "a}b", "{7}", "{HOME}"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestTypoedPlaceholderIsStillAnError(t *testing.T) {
	s := spec("echo", "{titel}")
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown placeholder {titel}") || !strings.Contains(err.Error(), "did you mean {title}") {
		t.Fatalf("%v", err)
	}
	if err := spec("echo", "{{titel}}").Validate(); err != nil {
		t.Fatalf("escaped braces are text: %v", err)
	}
}

func TestHugeTaskDoesNotBreakTheEnvironment(t *testing.T) {
	big := sample
	big.Notes = strings.Repeat("log line\n", 20<<10) // ~180 KiB
	p := spec("cat").Expand(big, "/cfg")
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	var omitted bool
	for _, e := range p.Env {
		if len(e) > maxEnvLen+64 {
			t.Errorf("variable %.30q... is %d bytes", e, len(e))
		}
		omitted = omitted || strings.HasPrefix(e, "TACK_TASK_JSON_OMITTED=") && strings.Contains(e, "stdin")
		if strings.HasPrefix(e, "TACK_TASK_JSON=") {
			t.Error("the oversized TACK_TASK_JSON must be left out")
		}
	}
	if !omitted {
		t.Errorf("no TACK_TASK_JSON_OMITTED in %d variables", len(p.Env))
	}
	// The small variables survive, and the whole task still reaches stdin.
	s := spec("cat")
	s.Stdin = StdinJSON
	if p := s.Expand(big, "/cfg"); !strings.Contains(p.Stdin, "log line") || !slices.Contains(p.Env, "TACK_TASK_ID=7") {
		t.Errorf("stdin %d bytes, env %v", len(p.Stdin), p.Env)
	}
	// The job really starts: exec would fail with "argument list too long" otherwise.
	r, err := spec("sh", "-c", "echo ok").Run(context.Background(), big, t.TempDir())
	if err != nil || r.Stdout != "ok" {
		t.Fatalf("%v %q", err, r.Stdout)
	}
}

func TestOversizedUserEnvIsReportedNotExecuted(t *testing.T) {
	big := sample
	big.Notes = strings.Repeat("x", 200<<10)
	s := spec("echo", "{id}")
	s.Env = map[string]string{"NOTES": "{notes}"}
	p := s.Expand(big, "/cfg")
	if p.Err == nil || !strings.Contains(p.Err.Error(), "env NOTES") || !strings.Contains(p.Err.Error(), "{notes} is 200 KiB") || !strings.Contains(p.Err.Error(), "stdin") {
		t.Fatalf("%v", p.Err)
	}
	// A small value is fine.
	if p := s.Expand(sample, "/cfg"); p.Err != nil {
		t.Fatal(p.Err)
	}
}

func TestOversizedArgumentIsReportedNotExecuted(t *testing.T) {
	big := sample
	big.Notes = strings.Repeat("x", 200<<10)
	s := spec("echo", "{id}", "{notes}")
	p := s.Expand(big, "/cfg")
	if p.Err == nil || !strings.Contains(p.Err.Error(), "command[2]") || !strings.Contains(p.Err.Error(), "{notes} is 200 KiB") || !strings.Contains(p.Err.Error(), "stdin") {
		t.Fatalf("%v", p.Err)
	}
	j := s.Start(context.Background(), big, t.TempDir())
	<-j.Done()
	if snap := j.Snapshot(); snap.Running || snap.Err == nil || !strings.Contains(snap.Err.Error(), "{notes}") {
		t.Fatalf("%+v", snap)
	}
	if _, _, err := s.Cmd(big, "/cfg"); err == nil {
		t.Error("Cmd must refuse it too")
	}
	// Many mid-size arguments add up.
	many := spec("echo")
	for i := 0; i < 12; i++ {
		many.Command = append(many.Command, "{notes}")
	}
	big.Notes = strings.Repeat("y", 100<<10)
	if p := many.Expand(big, "/cfg"); p.Err == nil || !strings.Contains(p.Err.Error(), "arguments are") {
		t.Fatalf("%v", p.Err)
	}
}

func TestNULBytesAreStripped(t *testing.T) {
	n := sample
	n.Title, n.Notes = "a\x00b", "c\x00d"
	p := spec("echo", "{title}", "{notes}").Expand(n, "/cfg")
	if p.Argv[1] != "ab" || p.Argv[2] != "cd" {
		t.Errorf("argv %q", p.Argv)
	}
	for _, e := range p.Env {
		if strings.ContainsRune(e, 0) {
			t.Errorf("NUL in %q", e)
		}
	}
	r, err := spec("sh", "-c", "echo \"$TACK_TASK_TITLE\"", "sh").Run(context.Background(), n, t.TempDir())
	if err != nil || r.Stdout != "ab" {
		t.Fatalf("%v %q", err, r.Stdout)
	}
}

func TestExpandWorkdirEnvSlug(t *testing.T) {
	s := spec("true")
	s.Workdir = "wt/{id}-{slug}"
	s.Env = map[string]string{"WORK": "{backend}/{slug}", "LIT": "{{id}}"}
	p := s.Expand(sample, "/cfg")
	if want := filepath.Join("/cfg", "wt", "7-fix-quotes-rm-rf"); p.Dir != want {
		t.Errorf("dir = %q, want %q", p.Dir, want)
	}
	env := strings.Join(p.Env, "\n")
	for _, w := range []string{"WORK=csv1/fix-quotes-rm-rf", "LIT={id}", "TACK_WORKDIR=" + p.Dir} {
		if !strings.Contains(env, w) {
			t.Errorf("env lacks %q:\n%s", w, env)
		}
	}
	evil := sample
	evil.ID, evil.Title = "../../x", "../../Etc passwd!  Ünï"
	if got, want := s.Expand(evil, "/cfg").Dir, filepath.Join("/cfg", "wt", "_._.._x-etc-passwd-n"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if got := slug("!!!"); got != "task" {
		t.Errorf("slug = %q", got)
	}
	s.Workdir = "wt/{title}"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "workdir") {
		t.Errorf("{title} in workdir: err = %v", err)
	}
}

func TestExpandEnvAndStdin(t *testing.T) {
	s := spec("cat")
	s.Env = map[string]string{"FOO": "bar"}
	s.Stdin = StdinMarkdown
	s.Workdir = "sub"
	p := s.Expand(sample, "/cfg")
	env := strings.Join(p.Env, "\n")
	for _, w := range []string{"FOO=bar", "TACK_TASK_ID=7", "TACK_BACKEND=csv1", "TACK_TASK_STATUS=todo", `TACK_TASK_JSON={"id":"7"`} {
		if !strings.Contains(env, w) {
			t.Errorf("env lacks %q:\n%s", w, env)
		}
	}
	if !strings.HasPrefix(p.Stdin, "# Fix") || !strings.Contains(p.Stdin, "- status: todo") || !strings.Contains(p.Stdin, "## Description") {
		t.Errorf("markdown = %q", p.Stdin)
	}
	if p.Dir != filepath.Join("/cfg", "sub") {
		t.Errorf("dir = %q", p.Dir)
	}
	s.Stdin = StdinJSON
	if got := s.Expand(sample, "/cfg").Stdin; !strings.HasPrefix(got, `{"id":"7"`) || !strings.HasSuffix(got, "\n") {
		t.Errorf("json stdin = %q", got)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Spec)
		want string // "" = valid
	}{
		{"ok", func(*Spec) {}, ""},
		{"no id", func(s *Spec) { s.Name = "" }, "id is required"},
		{"bad name", func(s *Spec) { s.Name = "a b" }, "may only contain"},
		{"no command", func(s *Spec) { s.Command = nil }, "command is required"},
		{"placeholder program", func(s *Spec) { s.Command = []string{"{title}"} }, "command[0]"},
		{"unknown placeholder", func(s *Spec) { s.Command = []string{"echo", "{nope}"} }, "unknown placeholder {nope}"},
		{"bad mode", func(s *Spec) { s.Mode = "fg" }, "mode"},
		{"bad stdin", func(s *Spec) { s.Stdin = "xml" }, "stdin"},
		{"event stdin is hook-only", func(s *Spec) { s.Stdin = StdinEvent }, "stdin"},
		{"event-markdown stdin is hook-only", func(s *Spec) { s.Stdin = StdinEventMarkdown }, "stdin"},
		{"interactive stdin", func(s *Spec) { s.Mode = ModeInteractive; s.Stdin = StdinJSON }, "stdin must be none"},
		{"bad timeout", func(s *Spec) { s.Timeout = "soon" }, "timeout"},
		{"zero timeout", func(s *Spec) { s.Timeout = "0s" }, "timeout"},
		{"long key", func(s *Spec) { s.Key = "ab" }, "single character"},
		{"reserved key", func(s *Spec) { s.Key = "J" }, "reserved"},
		{"bad env", func(s *Spec) { s.Env = map[string]string{"A=B": "x"} }, "env name"},
	}
	for _, c := range cases {
		s := Spec{Name: "x", Command: []string{"echo", "{id}"}}
		c.mod(&s)
		s.Normalize()
		err := s.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

func TestValidateAllDuplicates(t *testing.T) {
	err := ValidateAll([]Spec{{Name: "A", Command: []string{"x"}}, {Name: "a", Command: []string{"x"}}})
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("got %v", err)
	}
	err = ValidateAll([]Spec{{Name: "a", Key: "c", Command: []string{"x"}}, {Name: "b", Key: "C", Command: []string{"x"}}})
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("got %v", err)
	}
	if err := ValidateAll(nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunPassesTaskAndCapturesOutput(t *testing.T) {
	dir := t.TempDir()
	s := spec("sh", "-c", `printf '%s|%s|' "$1" "$TACK_TASK_ID"; cat`, "sh", "{title}")
	s.Stdin = StdinJSON
	res, err := s.Run(context.Background(), sample, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Stdout, sample.Title+"|7|{") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestRunUsesConfigDirAsWorkdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := spec("ls", "marker").Run(context.Background(), sample, dir); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWorkdir(t *testing.T) {
	base := t.TempDir()
	s := spec("pwd")
	s.Workdir = "wt/{slug}"
	if _, err := s.Run(context.Background(), sample, base); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("without create_workdir: %v", err)
	}
	s.CreateWorkdir = true
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	res, err := s.Run(context.Background(), sample, base)
	if err != nil {
		t.Fatal(err)
	}
	dir := s.Expand(sample, base).Dir
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || !strings.Contains(res.Stdout, filepath.Base(dir)) {
		t.Fatalf("dir %q: %v, stdout %q", dir, err, res.Stdout)
	}
	s.Workdir = ""
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "create_workdir") {
		t.Errorf("create_workdir without workdir: %v", err)
	}
}

func TestRunFailureShowsStderr(t *testing.T) {
	_, err := spec("sh", "-c", "echo boom >&2; echo more >&2; exit 4").Run(context.Background(), sample, t.TempDir())
	if err == nil || err.Error() != "x: boom" {
		t.Fatalf("got %v", err)
	}
	_, err = spec("sh", "-c", "exit 4").Run(context.Background(), sample, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "exit status 4") {
		t.Fatalf("got %v", err)
	}
}

func TestRunTimeout(t *testing.T) {
	s := spec("sh", "-c", "sleep 30 & wait")
	s.Timeout = "200ms"
	start := time.Now()
	_, err := s.Run(context.Background(), sample, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestRunOutputIsBounded(t *testing.T) {
	res, err := spec("sh", "-c", "head -c 400000 /dev/zero | tr '\\0' a").Run(context.Background(), sample, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != (DefaultMaxOutput << 10) {
		t.Fatalf("stdout len = %d", len(res.Stdout))
	}
}

func TestMissingProgram(t *testing.T) {
	_, err := spec("definitely-not-a-program-tack").Run(context.Background(), sample, t.TempDir())
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestCmdRejectsStdinAndSetsEnv(t *testing.T) {
	s := spec("claude", "{title}")
	s.Mode = ModeInteractive
	dir := t.TempDir()
	cmd, _, err := s.Cmd(sample, dir)
	if err != nil || cmd.Dir != dir || cmd.Args[1] != sample.Title {
		t.Fatalf("cmd=%+v err=%v", cmd, err)
	}
	s.Stdin = StdinJSON
	if _, _, err := s.Cmd(sample, dir); err == nil {
		t.Fatal("expected stdin conflict")
	}
}

func TestFind(t *testing.T) {
	list := []Spec{{Name: "Notify"}}
	if _, ok := Find(list, " notify "); !ok {
		t.Fatal("not found")
	}
	if _, ok := Find(list, "other"); ok {
		t.Fatal("found")
	}
}

func TestJobShowsOutputWhileRunningAndCancels(t *testing.T) {
	s := spec("sh", "-c", "echo first; sleep 30 & wait")
	j := s.Start(context.Background(), sample, t.TempDir())
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := j.Snapshot()
		if strings.Contains(snap.Stdout, "first") {
			if !snap.Running || snap.Err != nil || !snap.Ended.IsZero() {
				t.Fatalf("snapshot %+v", snap)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no output while running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	j.Cancel()
	select {
	case <-j.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the job")
	}
	snap := j.Snapshot()
	if snap.Running || !snap.Canceled || snap.Err == nil || !strings.Contains(snap.Err.Error(), "canceled") {
		t.Fatalf("after cancel: %+v", snap)
	}
	if snap.Elapsed(time.Now()) <= 0 || snap.Elapsed(time.Now().Add(time.Hour)) > time.Minute {
		t.Fatalf("elapsed %v", snap.Elapsed(time.Now().Add(time.Hour)))
	}
	j.Cancel() // no-op after the end
}

func TestJobTruncatesOutput(t *testing.T) {
	j := spec("sh", "-c", "head -c 400000 /dev/zero | tr '\\0' a").Start(context.Background(), sample, t.TempDir())
	<-j.Done()
	if snap := j.Snapshot(); !snap.Truncated || len(snap.Stdout) != (DefaultMaxOutput<<10) || snap.Err != nil {
		t.Fatalf("truncated=%v len=%d err=%v", snap.Truncated, len(snap.Stdout), snap.Err)
	}
}

func TestShowValidation(t *testing.T) {
	for show, want := range map[string]string{"output": "", "OUTPUT": "", "stdout": "show", "": ""} {
		s := Spec{Name: "x", Command: []string{"echo"}, Show: show}
		s.Normalize()
		if err := s.Validate(); (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("show %q: %v", show, err)
		}
	}
	s := Spec{Name: "x", Command: []string{"echo"}, Show: "output", Mode: ModeInteractive}
	s.Normalize()
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "background") {
		t.Errorf("interactive+show: %v", err)
	}
}

// The command exits 0 but a background child keeps the output pipes open:
// that is a success (Run waits only WaitDelay for the pipes), and the leftover
// child is killed.
func TestBackgroundChildHoldingPipesIsStillSuccess(t *testing.T) {
	start := time.Now()
	r, err := spec("sh", "-c", "sleep 30 & echo done").Run(context.Background(), sample, t.TempDir())
	if err != nil || r.Stdout != "done" {
		t.Fatalf("%v %q", err, r.Stdout)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("waited %s for the child", d)
	}
}

func TestCancelAfterExitIsANoOpAndASuccessIsNotCanceled(t *testing.T) {
	j := spec("true").Start(context.Background(), sample, t.TempDir())
	<-j.Done()
	j.Cancel()
	if snap := j.Snapshot(); snap.Canceled || snap.Err != nil || snap.ExitCode != 0 {
		t.Fatalf("%+v", snap)
	}
}

func TestMissingWorkingDirectoryIsAClearError(t *testing.T) {
	s := spec("true")
	s.Workdir = "nope/sub"
	_, err := s.Run(context.Background(), sample, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "working directory") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("%v", err)
	}
	if _, _, err := s.Cmd(sample, t.TempDir()); err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("Cmd: %v", err)
	}
}

func TestWarningsForTextThatCouldBeAnOption(t *testing.T) {
	for _, c := range []struct {
		cmd  []string
		want int
	}{
		{[]string{"claude", "{title}"}, 1},
		{[]string{"echo", "x", "{description} more"}, 1},
		{[]string{"echo", "x", "--", "{title}"}, 0},
		{[]string{"claude", "-p", "{title}"}, 0},
		{[]string{"echo", "x", "id {title}"}, 0},
		{[]string{"echo", "{id}"}, 0},
		{[]string{"sh", "-c", "echo $1", "sh", "{title}"}, 0},
	} {
		if got := spec(c.cmd...).Warnings(); len(got) != c.want {
			t.Errorf("%q: warnings = %q, want %d", c.cmd, got, c.want)
		}
	}
}

func TestTimeoutWarning(t *testing.T) {
	for _, c := range []struct {
		s    Spec
		want bool
	}{
		{spec("claude", "-p", "x"), true},
		{spec("/usr/bin/codex", "exec"), true},
		{spec("env", "A=1", "aider"), true},
		{spec("echo", "claude"), false},
		{spec("sh", "-c", "claude"), false},
		{Spec{Name: "x", Command: []string{"claude"}, Timeout: "30m"}, false},
		{Spec{Name: "x", Command: []string{"claude"}, Mode: ModeInteractive}, false},
		{Spec{Name: "x", Command: []string{"env"}}, false},
	} {
		c.s.Normalize()
		if got := c.s.TimeoutWarning() != ""; got != c.want {
			t.Errorf("%q mode=%s timeout=%q: warns = %v", c.s.Command, c.s.Mode, c.s.Timeout, got)
		}
	}
}

func TestShellWarningsForPlaceholdersInsideTheScript(t *testing.T) {
	for _, c := range []struct {
		cmd  []string
		want int
	}{
		{[]string{"sh", "-c", "echo {title}"}, 1},
		{[]string{"/bin/bash", "-ec", "notify '{title}' {id}"}, 2},
		{[]string{"sh", "-c", `echo "$1"`, "sh", "{title}"}, 0},
		{[]string{"sh", "-c", "awk '{print}' f"}, 0},
		{[]string{"sh", "script.sh", "{title}"}, 0},
		{[]string{"echo", "-c", "{title}"}, 0},
	} {
		s := spec(c.cmd...)
		if got := s.ShellWarnings(nil); len(got) != c.want {
			t.Errorf("%q: shell warnings = %q, want %d", c.cmd, got, c.want)
		}
		if got := s.Warnings(); c.cmd[0] != "echo" && len(got) != c.want {
			t.Errorf("%q: Warnings = %q, want %d", c.cmd, got, c.want)
		}
	}
	if got := spec("sh", "-c", "echo {event}").ShellWarnings([]string{"event"}); len(got) != 1 {
		t.Errorf("extra placeholder: %q", got)
	}
}

// The documented behaviour: argv elements are never shell-parsed, but a value
// expanded into the script of sh -c is shell code; as $1 it stays data.
func TestPlaceholderInShellScriptIsShellCodeButArgumentIsData(t *testing.T) {
	tk := task.Task{ID: "1", Title: `$(echo INJECTED)'"; echo X`}
	res, err := spec("sh", "-c", `printf '%s' "$1"`, "sh", "{title}").Run(context.Background(), tk, t.TempDir())
	if err != nil || res.Stdout != tk.Title {
		t.Fatalf("argument: %q %v", res.Stdout, err)
	}
	res, _ = spec("sh", "-c", `echo {title}`).Run(context.Background(), task.Task{ID: "1", Title: "$(echo INJECTED)"}, t.TempDir())
	if res.Stdout != "INJECTED" {
		t.Fatalf("inside the script the shell runs the text: %q", res.Stdout)
	}
}

func TestNotifyValidation(t *testing.T) {
	for notify, want := range map[string]string{"bell": "", " Desktop ": "", "loud": "notify", "": ""} {
		s := Spec{Name: "x", Command: []string{"echo"}, Notify: notify}
		s.Normalize()
		if err := s.Validate(); (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("notify %q: %v", notify, err)
		}
	}
	s := Spec{Name: "x", Command: []string{"echo"}, Notify: "bell", Mode: ModeInteractive}
	s.Normalize()
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "background") {
		t.Errorf("interactive+notify: %v", err)
	}
}

// For gives a backend the top-level actions, its own replacing those of the
// same name, then its others; CheckKeys looks at that list.
func TestForBackend(t *testing.T) {
	top := []Spec{{Name: "open", Key: "o", Command: []string{"xdg-open"}}, {Name: "note", Command: []string{"true"}}}
	gh := []Spec{{Name: "Open", Key: "o", Command: []string{"gh"}}, {Name: "pr", Key: "p", Command: []string{"gh"}}}
	md := []Spec{{Name: "edit", Key: "o", Command: []string{"vi"}}}
	if err := ValidateAll(top); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFor(gh, "gh"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFor(md, "md"); err != nil {
		t.Fatal(err)
	}
	all := slices.Concat(top, gh, md)
	names := func(specs []Spec) string {
		var out []string
		for _, s := range specs {
			out = append(out, s.Name+"="+s.Command[0])
		}
		return strings.Join(out, " ")
	}
	if got := names(For(all, "gh")); got != "Open=gh note=true pr=gh" {
		t.Errorf("gh: %s", got)
	}
	if got := names(For(all, "csv")); got != "open=xdg-open note=true" {
		t.Errorf("csv: %s", got)
	}
	if got := strings.Join(Names(all), " "); got != "open note pr edit" {
		t.Errorf("Names: %s", got)
	}
	if err := CheckKeys(all, "gh"); err != nil {
		t.Errorf("gh replaces open, its key is free: %v", err)
	}
	if err := CheckKeys(all, "md"); err == nil || !strings.Contains(err.Error(), `"open" and "edit"`) {
		t.Errorf("md key clash: %v", err)
	}
}

func TestCaptureAndMovesValidation(t *testing.T) {
	ok := Spec{Name: "a", Command: []string{"true"}, Capture: "Notes", OnSuccess: Outcome{Status: " Done "}, OnFailure: Outcome{Status: "Todo"}}
	ok.Normalize()
	if err := ok.Validate(); err != nil || ok.Capture != CaptureNotes || ok.OnSuccess.Status != "Done" {
		t.Fatalf("%v %+v", err, ok)
	}
	for name, s := range map[string]Spec{
		"unknown target": {Name: "a", Command: []string{"true"}, Capture: "stdout"},
		"interactive":    {Name: "a", Command: []string{"true"}, Capture: "notes", Mode: ModeInteractive},
		"negative max":   {Name: "a", Command: []string{"true"}, Capture: "notes", CaptureMax: -1},
	} {
		s.Normalize()
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := ok.CaptureBytes(); got != DefaultCaptureMax<<10 {
		t.Errorf("default %d", got)
	}
}

func TestWithRunAddsEnvWithoutTouchingTheOriginal(t *testing.T) {
	s := Spec{Name: "a", Command: []string{"true"}, Env: map[string]string{"X": "1"}}
	r := s.WithRun("run-1", "/tmp/{a}/log.jsonl")
	p := r.Expand(task.Task{ID: "1"}, "/")
	got := strings.Join(p.Env, "\n")
	if !strings.Contains(got, "TACK_RUN_ID=run-1") || !strings.Contains(got, "TACK_ACTION_LOG=/tmp/{a}/log.jsonl") || !strings.Contains(got, "X=1") {
		t.Fatalf("%s", got)
	}
	if _, set := s.Env["TACK_RUN_ID"]; set {
		t.Fatal("original spec modified")
	}
}

func TestOutcomeYAMLAndValidation(t *testing.T) {
	var s Spec
	src := "name: a\ncommand: [true]\non_success: Done\non_fail: {status: Todo, append_notes: ' Error '}\n"
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	s.Normalize()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.OnSuccess, Outcome{Status: "Done"}) || !reflect.DeepEqual(s.OnFailure, Outcome{Status: "Todo", AppendNotes: NotesError}) {
		t.Fatalf("%+v", s)
	}
	bad := Spec{Name: "b", Command: []string{"true"}, OnSuccess: Outcome{AppendNotes: NotesError}}
	bad.Normalize()
	if err := bad.Validate(); err == nil {
		t.Fatal("error is not a success source")
	}
}

func TestOutcomeCommand(t *testing.T) {
	var s Spec
	src := "name: a\ncommand: [true]\non_success: {status: Done, command: [notify, '{title} {result} {exit}']}\non_failure: {command: [alert, '{id}']}\n"
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	s.Normalize()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(s.OnSuccess.Command) != 2 || s.OnFailure.IsZero() {
		t.Fatalf("%+v", s)
	}
	for name, o := range map[string]Outcome{
		"empty program":       {Command: []string{" "}},
		"unknown placeholder": {Command: []string{"x", "{titel}"}},
	} {
		bad := Spec{Name: "b", Command: []string{"true"}, OnSuccess: o}
		bad.Normalize()
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNormalizeAliasesAndLabels(t *testing.T) {
	s := Spec{LegacyName: " triage ", LegacyLabel: "Triage it", Labels: []string{" research ", "", "research", "code"}, Command: []string{"echo"}}
	s.Normalize()
	if s.Name != "triage" || s.Label != "Triage it" || s.Title() != "Triage it" {
		t.Errorf("legacy keys not folded: %+v", s)
	}
	if len(s.Labels) != 2 || s.Labels[0] != "research" || s.Labels[1] != "code" {
		t.Errorf("labels = %v", s.Labels)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("unexpected %v", err)
	}
	c := Spec{Name: "a", LegacyName: "b", Command: []string{"echo"}}
	c.Normalize()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Errorf("id/name conflict: got %v", err)
	}
}

func TestWithWorkdir(t *testing.T) {
	s := spec("true")
	s.Workdir = "wt/{id}"
	if got := s.WithWorkdir(""); got.Workdir != "wt/{id}" {
		t.Errorf("empty override changed the workdir: %q", got.Workdir)
	}
	o := s.WithWorkdir("/else/{id}")
	if p := o.Expand(sample, "/cfg"); p.Dir != filepath.Join("/else", "7") {
		t.Errorf("dir = %q", p.Dir)
	}
	if s.Workdir != "wt/{id}" {
		t.Errorf("the original changed: %q", s.Workdir)
	}
}

func TestExpandWorkdirEnv(t *testing.T) {
	t.Setenv("TACK_T_ROOT", "/srv/w")
	s := spec("true")
	s.Workdir = "$TACK_T_ROOT/{id}"
	if p := s.Expand(sample, "/cfg"); p.Err != nil || p.Dir != "/srv/w/7" {
		t.Errorf("dir = %q, err %v", p.Dir, p.Err)
	}
	evil := sample
	evil.ID = "$TACK_T_ROOT"
	if p := s.Expand(evil, "/cfg"); p.Dir != "/srv/w/$TACK_T_ROOT" {
		t.Errorf("task text was expanded: %q", p.Dir)
	}
	s.Workdir = "$TACK_T_UNSET/x"
	if p := s.Expand(sample, "/cfg"); p.Err == nil || !strings.Contains(p.Err.Error(), "$TACK_T_UNSET is not set") {
		t.Errorf("unset: %v", p.Err)
	}
	if _, _, err := s.Cmd(sample, "/cfg"); err == nil {
		t.Errorf("Cmd started with an unset variable")
	}
}
