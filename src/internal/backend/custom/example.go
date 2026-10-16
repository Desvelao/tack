package custom

import "tkan/internal/backend"

func init() { backend.RegisterExample("custom", example) }

// example describes a sample `type: custom` entry; location is ignored.
func example(string) (backend.Example, error) {
	return backend.Example{
		Section: "command: [./tasks.sh]   # program + arguments, run from this file's directory\n" +
			"# cwd: .                # working directory (default: this file's directory)\n" +
			"# timeout: 10s          # per call (default 10s)\n" +
			"# poll: 5s             # re-run `list` this often to notice outside changes (default: never; press r)\n" +
			"# env: {TOKEN: abc}     # extra environment variables\n" +
			"# The command gets one JSON request on stdin and answers with JSON: see docs/backends.md",
		Notes: []string{"custom backend: edit `command` to point at your script (protocol in docs/backends.md)"},
	}, nil
}
