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

	"tkan/internal/backend"
	"tkan/internal/board"
	"tkan/internal/config"
)

// cmdConfig prints what tkan actually resolved: config file, backends
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
	eff := struct {
		Config  string       `yaml:"config"`
		Backend []backendOut `yaml:"backend"`
		Board   boardOut     `yaml:"board"`
		Audit   string       `yaml:"audit,omitempty"`
	}{Config: a.cfg.Path, Audit: a.cfg.AuditPath()}
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
	output := fs.String("o", "", "output path, or - for stdout (default: ./tkan.yaml)")
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
			fmt.Fprintf(errw, "tkan: init: unexpected argument %q (source is already %q)\n", fs.Arg(0), *source)
			fmt.Fprintln(errw, "usage: tkan init [-backend TYPE] [-source SRC] [-o OUT|-] [-force]")
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
			fmt.Fprintf(errw, "tkan: cannot infer a backend from %q; use -backend (supported: %s)\n",
				*source, strings.Join(backend.Supported(), ", "))
			return exitUsage
		}
		name = "csv"
	}

	dest := *output
	if dest == "" {
		dest = config.DefaultFile
		if !*force { // an alias (tkan.yml, .tkan.yaml, ...) would be shadowed by, or shadow, the new file
			for _, n := range config.ConfigNames {
				if st, err := os.Stat(n); err == nil && !st.IsDir() {
					fmt.Fprintf(errw, "tkan: %s already exists (use -force to overwrite)\n", n)
					return exitError
				}
			}
		}
	}
	if dest != "-" {
		if dir := filepath.Dir(dest); dir != "." {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				fmt.Fprintf(errw, "tkan: cannot write %s: directory %s does not exist\n", dest, dir)
				return exitError
			}
		}
	}
	text, notes, err := config.Generate(config.InitOptions{Backend: name, Location: *source, Output: dest})
	if err != nil {
		fmt.Fprintln(errw, "tkan:", err)
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
			fmt.Fprintf(errw, "tkan: %s already exists (use -force to overwrite)\n", dest)
		} else {
			fmt.Fprintln(errw, "tkan:", err)
		}
		return exitError
	}
	_, werr := io.WriteString(f, text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fmt.Fprintln(errw, "tkan:", werr)
		return exitError
	}
	fmt.Fprintf(errw, "wrote %s\n", dest)
	return exitOK
}
