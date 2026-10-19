// Command tkan shows a task backlog as a kanban board, and exposes
// non-interactive subcommands for scripts and AI agents.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tkan/internal/action"
	"tkan/internal/actionlog"
	"tkan/internal/audit"
	"tkan/internal/backend"
	_ "tkan/internal/backend/csv"    // register the csv backend
	_ "tkan/internal/backend/custom" // register the custom (command) backend
	_ "tkan/internal/backend/json"   // register the json and jsonl backends
	_ "tkan/internal/backend/yaml"   // register the yaml backend
	"tkan/internal/board"
	"tkan/internal/config"
	"tkan/internal/hook"
	"tkan/internal/query"
	"tkan/internal/task"
	"tkan/internal/tui"
)

// Exit codes (documented in docs/AGENTS.md).
const (
	exitOK           = 0
	exitError        = 1
	exitUsage        = 2
	exitNotFound     = 3
	exitPrecondition = 4   // set-status -from / edit -if / claim / run: the task was not in the expected state (or claimed by another)
	exitInterrupted  = 130 // killed by SIGINT/SIGTERM/SIGHUP, as a shell reports it
)

// version is stamped at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

// buildDate is stamped at build time: -ldflags "-X main.buildDate=2026-01-02". Empty when unstamped.
var buildDate = ""

// versionString is the one-line output of `tkan version`.
func versionString() string {
	if buildDate == "" {
		return "tkan " + version
	}
	return "tkan " + version + " (built " + buildDate + ")"
}

const usage = `tkan - kanban board for task backends

Usage:
  tkan [-c CONFIG]                           open the interactive board
  tkan [-c CONFIG] list [-status S] [-q QUERY] [-v VIEW] [-json|-ids|-tsv] [-fields F,..] [-sort [-]F,..] [-limit N]   print tasks (QUERY: see "tkan help filter")
  tkan [-c CONFIG] count [-by column|priority|type|assignee|label] [-status S] [-q QUERY] [-json]   count tasks per group
  tkan [-c CONFIG] labels [-status S] [-q QUERY] [-json]   list the labels in use with counts
  tkan [-c CONFIG] show ID [-b NAME] [-json] print one task
  tkan [-c CONFIG] statuses [-json]          print board columns and the raw values they hold
  tkan [-c CONFIG] set-status ID... STATUS [-b NAME] [-from STATUS] [-json]   move tasks (STATUS: column name or raw value; -from: only if still there, else exit 4)
  tkan [-c CONFIG] add -title T [-b NAME] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L] [-blocked-by IDS] [-status S]
  tkan [-c CONFIG] edit ID [-b NAME] [-if FIELD=VALUE] [-status S] [-add-label L] [-rm-label L] [-append-notes TEXT] [-title T] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L] [-blocked-by IDS]
                                                change only the fields given (-assignee "" clears one)
  tkan [-c CONFIG] delete ID... [-b NAME] [-n] [-json]
  tkan [-c CONFIG] claim ID... [-as AGENT] [-lease D] [-status S] [-b NAME] [-json]   take tasks for AGENT (default $TKAN_AGENT); exit 4 when another holds one
  tkan [-c CONFIG] release ID... [-as AGENT | -force] [-status S] [-b NAME] [-json]   give a claim back
  tkan [-c CONFIG] next [-status S] [-q QUERY] [-v VIEW] [-claim [-as AGENT] [-lease D] [-to S]] [-json]   the most urgent ready, unclaimed task (exit 3: none)
  tkan [-c CONFIG] fleet [-json]             who holds which task: agent, state, task, since, elapsed, lease left
  tkan [-c CONFIG] run ACTION ID... [-b NAME] [-i] [-n] [-force] [-json] [-propagate-exit]   run a configured action on tasks, one after another (-i: with the terminal, -n: only print the commands; a blocked task exits 4 unless -force; -json: summary with run id, exit code, log; -propagate-exit: exit with the command's own status)
  tkan [-c CONFIG] mcp [-read-only]          serve list/get/add/edit/set-status/next/claim/release/count/statuses/schema/run tools to an AI agent over stdio (MCP)
  tkan [-c CONFIG] report ID [-status S] [-note TEXT|-] [-run RUN_ID] [-b NAME] [-json]   an agent says how its run went: move the task and/or append a note ($TKAN_RUN_ID ties it to the run)
  tkan [-c CONFIG] runs [-q QUERY] [-limit N] [-json]   print the stored action runs (needs action_log; fields: action mode result date time source backend id title exit)
  tkan [-c CONFIG] audit [-q QUERY] [-limit N] [-json]   print the audit trail of reloads and writes (fields: op date time details result source backend id session)
  tkan [-c CONFIG] actions [-json]           list the configured actions (name, key, mode, backend, label)
  tkan [-c CONFIG] schema                    print task fields, commands with flags, exit codes, filter fields and actions as JSON (for agents)
  tkan [-c CONFIG] hooks [-json]             list the configured hooks (name, backend, on, when, timeout, command)
  tkan [-c CONFIG] hooks test NAME [ID] [-b NAME] [-event E] [-old-status S] [-n]   fire a hook by hand on a made-up event (-n: only print the command, env: values masked; exit 4 when when: or the depth limit stops it)
  tkan [-c CONFIG] config                    print the effective configuration (YAML)
  tkan help [filter|events|COMMAND]          this text, the filter query syntax, the hook events, or a command's flags
  tkan version                               print the version (also: tkan -version)
  tkan completion bash|zsh|fish              print a shell completion script
  tkan init [-backend TYPE] [-source SRC] [-o OUT|-] [-force]
                                                write a sample tkan.yaml (default OUT: ./tkan.yaml)

CONFIG defaults to $TKAN_CONFIG, then ./.tkan.yaml (or .tkan.yml, tkan.yaml,
tkan.yml), then ~/.tkan.yaml or ~/.tkan.yml (-c may
also follow the subcommand). It lists the backends
(type, optional name, options) and the board columns; see docs/config.md.
ID is unique only within a backend: when several backends hold the same ID use
-b NAME or NAME:ID. Backend types: %s.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// command is one subcommand. offline ones run without opening the config;
// online ones get the opened backends. A nil app means "only parse flags"
// (used by `tkan help CMD`, which runs the command with -h).
type command struct {
	name    string
	offline func(args []string, out, errw io.Writer) int
	online  func(ctx context.Context, a *app, args []string, out, errw io.Writer) int
}

// commandTable is the single list of subcommands: dispatch, the unknown-command
// check, `help CMD` and shell completion all derive from it.
func commandTable() []command {
	return []command{
		{name: "list", online: cmdList},
		{name: "count", online: cmdCount},
		{name: "labels", online: func(ctx context.Context, a *app, args []string, out, errw io.Writer) int {
			return cmdCount(ctx, a, append([]string{"-by", "label"}, args...), out, errw)
		}},
		{name: "show", online: cmdShow},
		{name: "statuses", online: func(_ context.Context, a *app, args []string, out, errw io.Writer) int {
			return cmdStatuses(a, args, out, errw)
		}},
		{name: "set-status", online: cmdSetStatus},
		{name: "add", online: cmdAdd},
		{name: "edit", online: cmdEdit},
		{name: "delete", online: cmdDelete},
		{name: "claim", online: cmdClaim},
		{name: "release", online: cmdRelease},
		{name: "next", online: cmdNext},
		{name: "fleet", online: cmdFleet},
		{name: "run", online: cmdRun},
		{name: "mcp", online: cmdMCP},
		{name: "runs", online: cmdRuns},
		{name: "report", online: cmdReport},
		{name: "audit", online: cmdAudit},
		{name: "hooks", online: cmdHooks},
		{name: "actions", online: cmdActions},
		{name: "schema", online: cmdSchema},
		{name: "config", online: func(_ context.Context, a *app, _ []string, out, errw io.Writer) int {
			if a == nil { // `help config`: the command has no flags
				return exitOK
			}
			return cmdConfig(a, out, errw)
		}},
		{name: "init", offline: cmdInit},
		{name: "help", offline: cmdHelp},
		{name: "version", offline: func(_ []string, out, _ io.Writer) int {
			fmt.Fprintln(out, versionString())
			return exitOK
		}},
		{name: "completion", offline: cmdCompletion},
		{name: "__complete", online: cmdComplete}, // hidden: values for shell completion
	}
}

func findCommand(name string) (command, bool) {
	for _, c := range commandTable() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// app is the resolved runtime: the open backends plus the board definition.
type app struct {
	defaultBackend string // "" = none; set by TKAN_BACKEND or default_backend
	be             *backend.Set
	spec           board.Spec
	cfg            *config.Config
	audit          *audit.Log
	actionLog      *actionlog.Log // nil = details of action runs are not stored
	hooks          *hook.Runner   // nil = no hooks configured
	source         string         // who acts, for runs: "" = cli, "mcp" under `tkan mcp`
}

// src is the source recorded for action runs.
func (a *app) src() string {
	if a.source == "" {
		return "cli"
	}
	return a.source
}

// find locates a task like Set.Find, but a bare ID falls back to the default
// backend (TKAN_BACKEND or default_backend) when no -b / NAME: prefix is given.
func (a *app) find(ctx context.Context, name, ref string) (task.Task, error) {
	if name == "" && a.defaultBackend != "" {
		prefixed := false
		if i := strings.Index(ref, ":"); i > 0 {
			_, prefixed = a.be.Get(ref[:i])
		}
		if !prefixed {
			name = a.defaultBackend
		}
	}
	return a.be.Find(ctx, name, ref)
}

// board resolves columns against the raw statuses present right now.
func (a *app) board() (board.Board, error) {
	return board.New(a.spec, a.be.Statuses())
}

func run(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("tkan", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = func() { fmt.Fprintf(errw, usage, strings.Join(backend.Supported(), ", ")) }
	cfgPath := fs.String("c", os.Getenv("TKAN_CONFIG"), "tkan.yaml config file")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintln(out, versionString())
		return exitOK
	}
	rest := fs.Args()

	var cmd command
	if len(rest) > 0 {
		c, ok := findCommand(rest[0])
		if !ok {
			fmt.Fprintf(errw, "tkan: unknown command %q (run `tkan help`)\n", rest[0])
			return exitUsage
		}
		cmd, rest = c, rest[1:]
	}

	// -c is also accepted after the subcommand: `tkan list -c foo.yaml`.
	if c, left, ok := takeConfigFlag(cmd, rest); ok {
		*cfgPath, rest = c, left
	}

	if cmd.name == "init" && *cfgPath != "" { // -c / TKAN_CONFIG: default output (an explicit -o still wins)
		rest = append([]string{"-o", *cfgPath}, rest...)
	}
	if cmd.offline != nil { // must work without (or despite a broken) config
		return cmd.offline(rest, out, errw)
	}

	a, err := open(*cfgPath, cmd.online == nil)
	if err != nil {
		fmt.Fprintln(errw, "tkan:", err)
		return exitError
	}
	defer a.be.Close()
	for _, s := range scoped(a.cfg.AllActions(), func(s action.Spec) string { return s.Backend }) { // before the board starts: the TUI owns the terminal afterwards
		if w := s.v.TimeoutWarning(); w != "" {
			fmt.Fprintf(errw, "tkan: warning: %sactions[%d] %s: %s\n", s.where, s.i, s.v.Name, w)
		}
	}
	for _, s := range scoped(a.cfg.AllHooks(), func(s hook.Spec) string { return s.Backend }) {
		if w := s.v.TimeoutWarning(); w != "" {
			fmt.Fprintf(errw, "tkan: warning: %shooks[%d] %s: %s\n", s.where, s.i, s.v.Name, w)
		}
		for _, w := range append(s.v.ShellWarnings(), s.v.WhenWarnings()...) { // a hook has no `tkan run` to show them
			fmt.Fprintf(errw, "tkan: warning: %shooks[%d] %s: %s\n", s.where, s.i, s.v.Name, w)
		}
	}
	defer func() { // an unwritable audit file must not lose the trail silently; runs after the hooks' entries
		if err := a.audit.Err(); err != nil {
			fmt.Fprintln(errw, "tkan: warning:", err)
		}
	}()
	defer waitForHooks(a.hooks, errw) // after-jobs finish before the backends close and the process exits

	if cmd.online == nil { // no subcommand: the interactive board
		board := tui.NewAsync(a.be, filepath.Base(a.cfg.Path), a.spec, func(tasks []task.Task) error { return checkFields(a.spec, tasks) }).WithAudit(a.audit).WithActions(a.cfg.AllActions(), a.cfg.Dir()).WithActionLimit(a.cfg.ActionParallel()).WithActionLog(a.actionLog).WithHooks(a.hooks)
		p := tea.NewProgram(board, tea.WithAltScreen())
		hup := make(chan os.Signal, 1) // Bubble Tea handles SIGINT and SIGTERM, not a closed terminal
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go func() {
			if _, ok := <-hup; ok {
				p.Quit()
			}
		}()
		_, err := p.Run()
		board.Shutdown(2 * time.Second) // runs still going are killed and recorded, whichever way the board ended
		if err != nil {
			fmt.Fprintln(errw, "tkan:", err)
			return exitError
		}
		return exitOK
	}
	// SIGINT, SIGTERM and SIGHUP cancel the command: a running action is killed with its
	// children and recorded before tkan exits. A second signal kills tkan at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	go func() { <-ctx.Done(); stop() }()
	code := cmd.online(ctx, a, rest, out, errw)
	if ctx.Err() != nil { // interrupted: do not make the user wait for the hooks too
		a.hooks.Cancel()
	}
	return code
}

// scopedSpec is an action or hook with its index in its own list and where
// that list is: "" for the top level, "backend NAME " for a backend's.
type scopedSpec[T any] struct {
	v     T
	i     int
	where string
}

// scoped numbers specs within their list (top level, then each backend's).
func scoped[T any](specs []T, backendOf func(T) string) []scopedSpec[T] {
	out := make([]scopedSpec[T], len(specs))
	n := map[string]int{}
	for k, s := range specs {
		b := backendOf(s)
		out[k] = scopedSpec[T]{v: s, i: n[b]}
		n[b]++
		if b != "" {
			out[k].where = "backend " + b + " "
		}
	}
	return out
}

func hasColumnSort(s board.Spec) bool {
	for _, c := range s.Columns {
		if len(c.Sort) > 0 {
			return true
		}
	}
	return false
}

// open loads the config and opens its backends. interactive is the board: it
// must not cache lists (reloads would show stale data), unlike the one-shot
// subcommands.
func open(cfgPath string, interactive bool) (*app, error) {
	path, err := config.Locate(cfgPath)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	spec := board.DefaultSpec()
	if len(cfg.Board.Columns) > 0 {
		spec.Columns = cfg.Board.Columns
	}
	if cfg.Board.Unmapped != "" {
		spec.Unmapped = cfg.Board.Unmapped
	}
	spec.Search = cfg.Board.Search
	spec.Sort = cfg.Board.Sort
	spec.Views = cfg.Board.Views
	spec.Group = cfg.Board.Group
	spec.Agents = cfg.Board.Agents
	spec.Avatars = cfg.Board.Avatars
	var be *backend.Set
	if interactive { // the board lists in the background: a slow backend must not delay it
		be, err = cfg.OpenLazy()
	} else {
		be, err = cfg.Open()
	}
	if err != nil {
		return nil, err
	}
	if !interactive { // one CLI invocation: list each backend at most once between writes
		be.CacheLists()
	}
	log, err := audit.New(cfg.AuditPath())
	if err != nil {
		be.Close()
		return nil, err
	}
	log.SetMaxSize(cfg.Audit.MaxSize)
	if interactive {
		log.SetSource("tui")
	}
	be.SetAuditor(log)
	alog, err := actionlog.New(cfg.ActionLog, cfg.Dir(), log.Session())
	if err != nil {
		be.Close()
		return nil, err
	}
	if os.Getenv(hook.DepthEnv) != "" { // started by a hook
		log.SetSource("hook")
	}
	src := "cli"
	if interactive {
		src = "tui"
	}
	runner := hook.New(cfg.AllHooks(), cfg.Dir(), src, alog, log)
	runner.SetMaxDepth(cfg.HookMaxDepth)
	runner.SetMaxParallel(cfg.HookMaxParallel)
	runner.SetBoard(spec)
	be.SetWriteListener(runner.OnWrite, runner.NeedsOld())
	a := &app{be: be, spec: spec, cfg: cfg, audit: log, actionLog: alog, hooks: runner}
	a.defaultBackend = cfg.DefaultBackend
	if env := strings.TrimSpace(os.Getenv("TKAN_BACKEND")); env != "" {
		a.defaultBackend = env
	}
	if a.defaultBackend != "" {
		canonical, ok := "", false
		for _, n := range be.Names() {
			if strings.EqualFold(n, a.defaultBackend) {
				canonical, ok = n, true
			}
		}
		if !ok {
			be.Close()
			return nil, fmt.Errorf("default backend %q is not configured (backends: %s)", a.defaultBackend, strings.Join(be.Names(), ", "))
		}
		a.defaultBackend = canonical
	}
	// Fail early on a column spec that cannot be resolved.
	if _, err := a.board(); err != nil {
		be.Close()
		return nil, err
	}
	// Searchable fields must exist (extra columns can only be checked when there is data).
	// The board lists in the background, so it checks once the first list is in.
	if needsFieldCheck(spec) && !interactive {
		tasks, err := be.List(context.Background())
		if err != nil {
			be.Close()
			return nil, err
		}
		if err := checkFields(spec, tasks); err != nil {
			be.Close()
			return nil, err
		}
	}
	return a, nil
}

// needsFieldCheck reports whether the board spec names fields that can only be
// checked against real tasks.
func needsFieldCheck(spec board.Spec) bool {
	return len(spec.Search) > 0 || len(spec.Sort) > 0 || spec.Group != "" || hasColumnSort(spec)
}

// checkFields validates the fields the board spec names (search, sort, group,
// column sorts) against tasks; nothing is checked while there are none.
func checkFields(spec board.Spec, tasks []task.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	known := query.KnownFields(tasks)
	if err := query.CheckSearch(spec.Search, known); err != nil {
		return err
	}
	if err := query.CheckSort(spec.Sort, known); err != nil {
		return err
	}
	if spec.Group != "" {
		if err := query.CheckSearch([]string{spec.Group}, known); err != nil {
			var uf *query.UnknownFieldError
			if errors.As(err, &uf) {
				uf.Prefix = "board.group"
			}
			return err
		}
	}
	for _, c := range spec.Columns {
		if err := query.CheckSort(c.Sort, known); err != nil {
			return err
		}
	}
	return nil
}

func fail(errw io.Writer, err error) int {
	fmt.Fprintln(errw, "tkan:", err)
	if errors.Is(err, backend.ErrNotFound) {
		return exitNotFound
	}
	if errors.Is(err, backend.ErrPrecondition) {
		return exitPrecondition
	}
	var amb *backend.AmbiguousError
	if errors.As(err, &amb) {
		return exitUsage
	}
	return exitError
}

func writeJSON(out io.Writer, v any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return exitError
	}
	return exitOK
}

// taskOut is a task as printed by -json: the raw task plus its board column.
type taskOut struct {
	task.Task
	Column string `json:"column"`
	// Blockers are the ids of blocked_by still open (list only: it needs every task).
	Blockers []string   `json:"blockers,omitempty"`
	deps     board.Deps // for the blocked/ready fields; zero outside list and count
}

// lookup exposes o to filters, sorting and -fields.
func (o taskOut) lookup() query.Lookup { return query.TaskLookup(o.Task, o.Column, o.deps) }

func withColumn(b board.Board, t task.Task) taskOut {
	o := taskOut{Task: t}
	if c := b.ColumnOf(t.Status); c >= 0 {
		o.Column = b.Columns[c].Name
	}
	return o
}

func unknownStatus(errw io.Writer, b board.Board, s string) int {
	fmt.Fprintf(errw, "tkan: unknown status %q (columns: %s)\n", s, strings.Join(b.Names(), ", "))
	return exitUsage
}

// waitForHooks lets the hook commands started by this run finish before
// tkan exits (each is bounded by its timeout), saying so when it takes a while;
// Ctrl-C kills them instead.
func waitForHooks(r *hook.Runner, errw io.Writer) {
	n := r.Running()
	if n == 0 {
		r.Wait() // their records are still being written
		return
	}
	fmt.Fprintf(errw, "tkan: waiting for %d hook run(s) to finish (Ctrl-C stops them)…\n", n)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	done := make(chan struct{})
	go func() { r.Wait(); close(done) }()
	select {
	case <-done:
	case <-sig:
		r.Cancel()
		<-done
	}
}
