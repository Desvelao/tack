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
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"tkan/internal/backend"
	_ "tkan/internal/backend/csv"    // register the csv backend
	_ "tkan/internal/backend/custom" // register the custom (command) backend
	"tkan/internal/board"
	"tkan/internal/config"
	"tkan/internal/query"
	"tkan/internal/task"
	"tkan/internal/tui"
)

// Exit codes (documented in docs/AGENTS.md).
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNotFound = 3
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
  tkan [-c CONFIG] set-status ID... STATUS [-b NAME] [-json]   move tasks (STATUS: column name or raw value)
  tkan [-c CONFIG] add -title T [-b NAME] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L] [-status S]
  tkan [-c CONFIG] edit ID [-b NAME] [-status S] [-add-label L] [-rm-label L] [-title T] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L]
                                                change only the fields given (-assignee "" clears one)
  tkan [-c CONFIG] delete ID... [-b NAME] [-n]
  tkan [-c CONFIG] config                    print the effective configuration (YAML)
  tkan help [filter|COMMAND]                 this text, the filter query syntax, or a command's flags
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

	a, err := open(*cfgPath)
	if err != nil {
		fmt.Fprintln(errw, "tkan:", err)
		return exitError
	}
	defer a.be.Close()

	if cmd.online == nil { // no subcommand: the interactive board
		if _, err := tea.NewProgram(tui.New(a.be, filepath.Base(a.cfg.Path), a.spec), tea.WithAltScreen()).Run(); err != nil {
			fmt.Fprintln(errw, "tkan:", err)
			return exitError
		}
		return exitOK
	}
	return cmd.online(context.Background(), a, rest, out, errw)
}

func hasColumnSort(s board.Spec) bool {
	for _, c := range s.Columns {
		if len(c.Sort) > 0 {
			return true
		}
	}
	return false
}

// open loads the config and opens its backends.
func open(cfgPath string) (*app, error) {
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
	be, err := cfg.Open()
	if err != nil {
		return nil, err
	}
	be.CacheLists() // one CLI invocation: list each backend at most once between writes
	a := &app{be: be, spec: spec, cfg: cfg}
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
	if len(spec.Search) > 0 || len(spec.Sort) > 0 || spec.Group != "" || hasColumnSort(spec) {
		tasks, err := be.List(context.Background())
		if err != nil {
			be.Close()
			return nil, err
		}
		if len(tasks) > 0 {
			known := query.KnownFields(tasks)
			if err := query.CheckSearch(spec.Search, known); err != nil {
				be.Close()
				return nil, err
			}
			if err := query.CheckSort(spec.Sort, known); err != nil {
				be.Close()
				return nil, err
			}
			if spec.Group != "" {
				if err := query.CheckSearch([]string{spec.Group}, known); err != nil {
					var uf *query.UnknownFieldError
					if errors.As(err, &uf) {
						uf.Prefix = "board.group"
					}
					be.Close()
					return nil, err
				}
			}
			for _, c := range spec.Columns {
				if err := query.CheckSort(c.Sort, known); err != nil {
					be.Close()
					return nil, err
				}
			}
		}
	}
	return a, nil
}

func fail(errw io.Writer, err error) int {
	fmt.Fprintln(errw, "tkan:", err)
	if errors.Is(err, backend.ErrNotFound) {
		return exitNotFound
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
}

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
