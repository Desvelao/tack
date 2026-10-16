package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"tkan/internal/backend"
	"tkan/internal/query"
)

// cmdHelp prints the usage text, the filter syntax or one command's synopsis
// and flags. It needs no data file.
func cmdHelp(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(out, usage, strings.Join(backend.Supported(), ", "))
		return exitOK
	}
	topic := strings.ToLower(args[0])
	switch topic {
	case "filter", "query", "search":
		io.WriteString(out, query.Help(query.KnownFields(nil), nil))
		io.WriteString(out, "Plus any other column of the file, by its name (spaces as underscores).\n")
		return exitOK
	}
	if c, ok := findCommand(topic); ok {
		io.WriteString(out, synopsis(topic))
		// Running the command with -h prints its flags (flag parsing happens
		// before the app is touched, so nil is fine).
		switch {
		case c.online != nil:
			c.online(context.Background(), nil, []string{"-h"}, out, out)
		case c.name == "init":
			c.offline([]string{"-h"}, out, out)
		}
		return exitOK
	}
	names := []string{"filter"}
	for _, c := range commandTable() {
		names = append(names, c.name)
	}
	fmt.Fprintf(errw, "tkan: no help topic %q (topics: %s)\n", args[0], strings.Join(names, ", "))
	return exitUsage
}

// synopsis extracts a command's lines from the usage text.
func synopsis(name string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(usage, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "tkan ") {
			f := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(t, "tkan "), "[-c CONFIG] "))
			in = len(f) > 0 && f[0] == name
		} else if !strings.HasPrefix(line, "  ") {
			in = false
		}
		if in {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
