package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"tack/internal/action"
	"tack/internal/backend"
	"tack/internal/board"
	"tack/internal/config"
)

// cmdConfig prints what tack actually resolved: config file, backends
// and the columns (including any created for unmapped values).
func cmdConfig(a *app, out, errw io.Writer) int {
	b, err := a.board()
	if err != nil {
		return fail(errw, err)
	}
	type boardOut struct {
		Unmapped string           `yaml:"unmapped"`
		Search   []string         `yaml:"search,omitempty"`
		Sort     []string         `yaml:"sort,omitempty"`
		Group    string           `yaml:"group,omitempty"`
		Views    []board.ViewSpec `yaml:"views,omitempty"`
		Columns  []board.Column   `yaml:"columns"`
	}
	type backendOut struct {
		Name     string    `yaml:"name"`
		Type     string    `yaml:"type"`
		Command  []string  `yaml:"command,omitempty"`
		Options  yaml.Node `yaml:"options,omitempty"`
		Disabled bool      `yaml:"disabled,omitempty"`
	}
	type actionOut struct {
		Name    string `yaml:"name"`
		Mode    string `yaml:"mode"`
		Key     string `yaml:"key,omitempty"`
		Backend string `yaml:"backend,omitempty"`
		Claim   bool   `yaml:"claim,omitempty"`
	}
	type hookOut struct {
		Name    string   `yaml:"name"`
		On      []string `yaml:"on"`
		Backend string   `yaml:"backend,omitempty"`
	}
	// Names and triggers only: `tack actions` and `tack hooks` list the commands.
	eff := struct {
		Config            string       `yaml:"config"`
		DefaultBackend    string       `yaml:"default_backend,omitempty"`
		Backend           []backendOut `yaml:"backend"`
		Board             boardOut     `yaml:"board"`
		Audit             string       `yaml:"audit,omitempty"`
		AuditMaxSize      int          `yaml:"audit_max_size,omitempty"`
		ActionLog         string       `yaml:"action_log,omitempty"`
		Actions           []actionOut  `yaml:"actions,omitempty"`
		ActionMaxParallel int          `yaml:"action_max_parallel,omitempty"`
		Hooks             []hookOut    `yaml:"hooks,omitempty"`
		HookMaxDepth      int          `yaml:"hook_max_depth,omitempty"`
		HookMaxParallel   int          `yaml:"hook_max_parallel,omitempty"`
	}{Config: a.cfg.Path, DefaultBackend: a.defaultBackend, Audit: a.cfg.AuditPath(), AuditMaxSize: a.cfg.Audit.MaxSize,
		ActionMaxParallel: a.cfg.ActionMaxParallel, HookMaxDepth: a.cfg.HookMaxDepth, HookMaxParallel: a.cfg.HookMaxParallel}
	if l := a.cfg.ActionLog; l.Enabled() {
		eff.ActionLog = strings.TrimSpace(l.File)
		if d := strings.TrimSpace(l.Dir); d != "" {
			eff.ActionLog += " " + d
		}
		eff.ActionLog = strings.TrimSpace(eff.ActionLog)
	}
	for _, s := range a.cfg.AllActions() {
		mode := s.Mode
		if mode == "" {
			mode = action.ModeBackground
		}
		eff.Actions = append(eff.Actions, actionOut{Name: s.Name, Mode: mode, Key: s.Key, Backend: s.Backend, Claim: s.Claim})
	}
	for _, s := range a.cfg.AllHooks() {
		eff.Hooks = append(eff.Hooks, hookOut{Name: s.Name, On: s.On, Backend: s.Backend})
	}
	for _, e := range a.cfg.Backends {
		eff.Backend = append(eff.Backend, backendOut{Name: e.Name, Type: e.Type, Command: e.Command, Options: e.Options, Disabled: e.Disabled})
	}
	eff.Board = boardOut{Unmapped: a.spec.Unmapped, Search: a.spec.Search, Group: a.spec.Group, Views: a.spec.Views, Columns: b.Columns}
	for _, k := range a.spec.Sort {
		eff.Board.Sort = append(eff.Board.Sort, k.String())
	}
	if eff.Board.Unmapped == "" {
		eff.Board.Unmapped = board.Append
	}
	data, err := yaml.Marshal(eff)
	if err != nil {
		return fail(errw, err)
	}
	out.Write(data)
	return exitOK
}

// cmdInit writes a sample configuration adjusted to the backend type and, when
// the source exists, derived from its actual format.
func cmdInit(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errw)
	backendName := fs.String("backend", "", "backend type (default: from the source's extension, else csv)")
	source := fs.String("source", "", "data source the sample is for (for csv: the CSV file)")
	output := fs.String("o", "", "output path, or - for stdout (default: ./tack.yaml)")
	force := fs.Bool("force", false, "overwrite an existing output file")
	// Go's flag package stops at the first positional; accept SOURCE before or between flags.
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		if *source != "" {
			fmt.Fprintf(errw, "tack: init: unexpected argument %q (source is already %q)\n", fs.Arg(0), *source)
			fmt.Fprintln(errw, "usage: tack init [-backend TYPE] [-source SRC] [-o OUT|-] [-force]")
			return exitUsage
		}
		*source, rest = fs.Arg(0), fs.Args()[1:]
	}

	name := strings.ToLower(*backendName)
	if name == "" {
		name = backend.TypeForLocation(*source)
	}
	if name == "" {
		if *source != "" {
			fmt.Fprintf(errw, "tack: cannot infer a backend from %q; use -backend (supported: %s)\n",
				*source, strings.Join(backend.Supported(), ", "))
			return exitUsage
		}
		name = "csv"
	}

	dest := *output
	if dest == "" {
		dest = config.DefaultFile
		if !*force { // an alias (tack.yml, .tack.yaml, ...) would be shadowed by, or shadow, the new file
			for _, n := range config.ConfigNames {
				if st, err := os.Stat(n); err == nil && !st.IsDir() {
					fmt.Fprintf(errw, "tack: %s already exists (use -force to overwrite)\n", n)
					return exitError
				}
			}
		}
	}
	if dest != "-" {
		if dir := filepath.Dir(dest); dir != "." {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				fmt.Fprintf(errw, "tack: cannot write %s: directory %s does not exist\n", dest, dir)
				return exitError
			}
		}
	}
	text, notes, err := config.Generate(config.InitOptions{Backend: name, Location: *source, Output: dest})
	if err != nil {
		fmt.Fprintln(errw, "tack:", err)
		return exitError
	}
	for _, n := range notes {
		fmt.Fprintln(errw, "note:", n)
	}
	if dest == "-" {
		io.WriteString(out, text)
		return exitOK
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(errw, "tack: %s already exists (use -force to overwrite)\n", dest)
		} else {
			fmt.Fprintln(errw, "tack:", err)
		}
		return exitError
	}
	_, werr := io.WriteString(f, text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fmt.Fprintln(errw, "tack:", werr)
		return exitError
	}
	fmt.Fprintf(errw, "wrote %s\n", dest)
	return exitOK
}
