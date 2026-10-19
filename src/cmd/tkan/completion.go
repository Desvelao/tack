package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"tkan/internal/action"
)

// visibleCommands are the subcommands users see (hidden ones start with "__").
func visibleCommands() []command {
	var out []command
	for _, c := range commandTable() {
		if !strings.HasPrefix(c.name, "__") {
			out = append(out, c)
		}
	}
	return out
}

// flagLine matches the flag package's usage lines: "  -name" for a bool,
// "  -name string" (or int, value...) for a flag that takes a value.
// (A one-letter bool puts its usage on the same line after a tab.)
var flagLine = regexp.MustCompile(`(?m)^\s+-(\S+?)(?: (\S+))?(?:\t.*)?$`)

// usageFlags runs the command with -h (flag parsing happens before the app is
// touched, so a nil app is fine) and returns the default usage matches.
func usageFlags(c command) [][]string {
	var buf bytes.Buffer
	switch {
	case c.online != nil:
		c.online(context.Background(), nil, []string{"-h"}, &buf, &buf)
	case c.name == "init":
		c.offline([]string{"-h"}, &buf, &buf)
	}
	return flagLine.FindAllStringSubmatch(buf.String(), -1)
}

// commandFlags lists a command's flags, sorted.
func commandFlags(c command) []string {
	var flags []string
	for _, m := range usageFlags(c) {
		flags = append(flags, m[1])
	}
	sort.Strings(flags)
	return flags
}

// valueFlags is the set of the command's flags that take a value.
func valueFlags(c command) map[string]bool {
	set := map[string]bool{}
	for _, m := range usageFlags(c) {
		if m[2] != "" {
			set[m[1]] = true
		}
	}
	return set
}

// valueKind says what a flag's value is, so the scripts can complete it.
var valueKinds = map[string]string{"status": "columns", "b": "backends"}

// cmdComplete prints completion candidates, one per line: columns, backends or
// ids. Errors print nothing (completion must never get in the way).
func cmdComplete(ctx context.Context, a *app, args []string, out, _ io.Writer) int {
	if a == nil || len(args) != 1 {
		return exitOK
	}
	switch args[0] {
	case "columns":
		if b, err := a.board(); err == nil {
			for _, n := range b.Names() {
				fmt.Fprintln(out, n)
			}
		}
	case "actions":
		for _, n := range action.Names(a.cfg.AllActions()) {
			fmt.Fprintln(out, n)
		}
	case "hooks":
		for _, s := range a.cfg.AllHooks() {
			fmt.Fprintln(out, s.Name)
		}
	case "backends":
		for _, n := range a.be.Names() {
			fmt.Fprintln(out, n)
		}
	case "ids":
		tasks, _ := a.be.List(ctx)
		multi := len(a.be.Names()) > 1
		for _, t := range tasks {
			if multi {
				fmt.Fprintf(out, "%s:%s\n", t.Backend, t.ID)
			} else {
				fmt.Fprintln(out, t.ID)
			}
		}
	}
	return exitOK
}

// cmdCompletion prints a completion script for bash, zsh or fish: subcommands,
// each command's flags, help topics and, through the hidden `__complete`
// command, column names (-status), backend names (-b) and task IDs.
func cmdCompletion(args []string, out, errw io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(errw, "usage: tkan completion bash|zsh|fish")
		return exitUsage
	}
	cmds := visibleCommands()
	var names []string
	flags := map[string][]string{}
	for _, c := range cmds {
		names = append(names, c.name)
		flags[c.name] = commandFlags(c)
	}
	topics := append([]string{"filter", "events"}, names...)
	switch args[0] {
	case "bash":
		writeBash(out, names, flags, topics)
	case "zsh":
		writeZsh(out, names, flags, topics)
	case "fish":
		writeFish(out, names, flags, topics)
	default:
		fmt.Fprintf(errw, "tkan: unknown shell %q (bash, zsh, fish)\n", args[0])
		return exitUsage
	}
	return exitOK
}

func dashed(flags []string) string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = "-" + f
	}
	return strings.Join(out, " ")
}

// idCommands take task IDs as positional arguments.
var idCommands = []string{"show", "set-status", "edit", "delete", "run", "claim", "release", "report"}

func writeBash(out io.Writer, names []string, flags map[string][]string, topics []string) {
	fmt.Fprint(out, `_tkan() {
  local cur=${COMP_WORDS[COMP_CWORD]} prev=${COMP_WORDS[COMP_CWORD-1]}
  local cmd="" cfg=() i
  for ((i = 1; i < COMP_CWORD; i++)); do
    case ${COMP_WORDS[i]} in
      -c) cfg=(-c "${COMP_WORDS[i+1]}"); ((i++)) ;;
      -*) ;;
      *) [ -z "$cmd" ] && cmd=${COMP_WORDS[i]} ;;
    esac
  done
  local IFS=$'\n'
  case $prev in
    -status) COMPREPLY=($(compgen -W "$(tkan "${cfg[@]}" __complete columns 2>/dev/null)" -- "$cur")); return ;;
    -b) COMPREPLY=($(compgen -W "$(tkan "${cfg[@]}" __complete backends 2>/dev/null)" -- "$cur")); return ;;
    -c) COMPREPLY=($(compgen -f -- "$cur")); return ;;
  esac
  IFS=$' \t\n'
  if [ -z "$cmd" ]; then
`)
	fmt.Fprintf(out, "    COMPREPLY=($(compgen -W \"%s -c -version\" -- \"$cur\"))\n    return\n  fi\n  case $cmd in\n", strings.Join(names, " "))
	fmt.Fprintf(out, "    help) COMPREPLY=($(compgen -W \"%s\" -- \"$cur\")) ;;\n", strings.Join(topics, " "))
	fmt.Fprint(out, "    completion) COMPREPLY=($(compgen -W \"bash zsh fish\" -- \"$cur\")) ;;\n")
	for _, n := range names {
		if n == "help" || n == "completion" {
			continue
		}
		extra := ""
		for _, c := range idCommands {
			if c == n {
				extra = `
      if [[ $cur != -* ]]; then
        local IFS=$'\n'
        COMPREPLY+=($(compgen -W "$(tkan "${cfg[@]}" __complete ids 2>/dev/null)" -- "$cur"))`
				if n == "set-status" {
					extra += `
        COMPREPLY+=($(compgen -W "$(tkan "${cfg[@]}" __complete columns 2>/dev/null)" -- "$cur"))`
				}
				if n == "run" {
					extra += `
        COMPREPLY+=($(compgen -W "$(tkan "${cfg[@]}" __complete actions 2>/dev/null)" -- "$cur"))`
				}
				extra += `
      fi`
			}
		}
		if n == "hooks" {
			extra = `
      if [[ $cur != -* ]]; then
        local IFS=$'\n'
        COMPREPLY+=($(compgen -W "test $(tkan "${cfg[@]}" __complete hooks 2>/dev/null)
$(tkan "${cfg[@]}" __complete ids 2>/dev/null)" -- "$cur"))
      fi`
		}
		fmt.Fprintf(out, "    %s)\n      COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))%s\n      ;;\n", n, dashed(flags[n]), extra)
	}
	fmt.Fprint(out, "  esac\n}\ncomplete -F _tkan tkan\n")
}

func writeZsh(out io.Writer, names []string, flags map[string][]string, topics []string) {
	fmt.Fprint(out, "#compdef tkan\n_tkan() {\n  local -a cfg\n  local i\n  for ((i = 2; i < CURRENT; i++)); do [[ $words[i] == -c ]] && cfg=(-c $words[i+1]); done\n")
	fmt.Fprintf(out, "  if (( CURRENT == 2 )); then compadd -- %s -c -version; return; fi\n", strings.Join(names, " "))
	fmt.Fprint(out, "  case $words[CURRENT-1] in\n")
	fmt.Fprint(out, "    -status) compadd -- ${(f)\"$(tkan $cfg __complete columns 2>/dev/null)\"}; return ;;\n")
	fmt.Fprint(out, "    -b) compadd -- ${(f)\"$(tkan $cfg __complete backends 2>/dev/null)\"}; return ;;\n")
	fmt.Fprint(out, "    -c) _files; return ;;\n  esac\n  case $words[2] in\n")
	fmt.Fprintf(out, "    help) compadd -- %s ;;\n    completion) compadd -- bash zsh fish ;;\n", strings.Join(topics, " "))
	for _, n := range names {
		if n == "help" || n == "completion" {
			continue
		}
		extra := ""
		for _, c := range idCommands {
			if c == n {
				extra = "\n      [[ $PREFIX != -* ]] && compadd -- ${(f)\"$(tkan $cfg __complete ids 2>/dev/null)\"}"
				if n == "set-status" {
					extra += "\n      [[ $PREFIX != -* ]] && compadd -- ${(f)\"$(tkan $cfg __complete columns 2>/dev/null)\"}"
				}
				if n == "run" {
					extra += "\n      [[ $PREFIX != -* ]] && compadd -- ${(f)\"$(tkan $cfg __complete actions 2>/dev/null)\"}"
				}
			}
		}
		if n == "hooks" {
			extra = "\n      [[ $PREFIX != -* ]] && compadd -- test ${(f)\"$(tkan $cfg __complete hooks 2>/dev/null)\"} ${(f)\"$(tkan $cfg __complete ids 2>/dev/null)\"}"
		}
		fmt.Fprintf(out, "    %s)\n      compadd -- %s%s\n      ;;\n", n, dashed(flags[n]), extra)
	}
	fmt.Fprint(out, "  esac\n}\n_tkan \"$@\"\n")
}

func writeFish(out io.Writer, names []string, flags map[string][]string, topics []string) {
	fmt.Fprint(out, "function __tkan_cfg\n  set -l w (commandline -opc)\n  for i in (seq 1 (count $w))\n    if test \"$w[$i]\" = -c; and test (math $i + 1) -le (count $w)\n      echo -c $w[(math $i + 1)]\n    end\n  end\nend\n")
	fmt.Fprintf(out, "complete -c tkan -f -n __fish_use_subcommand -a \"%s\"\n", strings.Join(names, " "))
	fmt.Fprint(out, "complete -c tkan -n __fish_use_subcommand -o c -r -d 'config file'\ncomplete -c tkan -n __fish_use_subcommand -o version -d 'print the version'\n")
	fmt.Fprintf(out, "complete -c tkan -f -n '__fish_seen_subcommand_from help' -a \"%s\"\n", strings.Join(topics, " "))
	fmt.Fprint(out, "complete -c tkan -f -n '__fish_seen_subcommand_from completion' -a \"bash zsh fish\"\n")
	for _, n := range names {
		for _, f := range flags[n] {
			fmt.Fprintf(out, "complete -c tkan -n '__fish_seen_subcommand_from %s' -o %s\n", n, f)
		}
	}
	fmt.Fprint(out, "complete -c tkan -f -n '__fish_seen_subcommand_from list count labels set-status edit add claim release next; and __fish_prev_arg_in -status' -a '(tkan (__tkan_cfg) __complete columns 2>/dev/null)'\n")
	fmt.Fprint(out, "complete -c tkan -f -n '__fish_seen_subcommand_from show set-status edit delete add run claim release hooks; and __fish_prev_arg_in -b' -a '(tkan (__tkan_cfg) __complete backends 2>/dev/null)'\n")
	fmt.Fprint(out, "complete -c tkan -f -n '__fish_seen_subcommand_from show set-status edit delete run claim release' -a '(tkan (__tkan_cfg) __complete ids 2>/dev/null)'\n")
	fmt.Fprint(out, "complete -c tkan -f -n '__fish_seen_subcommand_from run' -a '(tkan (__tkan_cfg) __complete actions 2>/dev/null)'\n")
	fmt.Fprint(out, "complete -c tkan -f -n '__fish_seen_subcommand_from set-status' -a '(tkan (__tkan_cfg) __complete columns 2>/dev/null)'\n")
	fmt.Fprint(out, "complete -c tkan -f -n '__fish_seen_subcommand_from hooks' -a 'test (tkan (__tkan_cfg) __complete hooks 2>/dev/null) (tkan (__tkan_cfg) __complete ids 2>/dev/null)'\n")
}
