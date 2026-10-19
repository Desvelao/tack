package custom

import "tkan/internal/backend"

func init() {
	backend.RegisterExample("custom", example)
	backend.RegisterExample("sqlite", sqliteExample)
}

// example describes a sample custom entry (`command:` implies the type); location is ignored.
func example(string) (backend.Example, error) {
	return backend.Example{
		Entry: "command: [./tasks.sh]   # program + arguments, run from this file's directory",
		Section: "# cwd: .                # working directory (default: this file's directory)\n" +
			"# timeout: 10s          # per call (default 10s)\n" +
			"# poll: 5s             # re-run `list` this often to notice outside changes (default: never; press r)\n" +
			"# env: {TOKEN: abc}     # extra environment variables\n" +
			"# The command gets one JSON request on stdin and answers with JSON: see docs/backends.md",
		Notes: []string{"custom backend: edit `command` to point at your script (protocol in docs/backends.md)"},
	}, nil
}

// sqliteExample is the entry for the sqlite script in contrib/backends/sqlite
// (it was a built-in backend); location is the database file.
func sqliteExample(string) (backend.Example, error) {
	return backend.Example{
		Entry: "command: [./tkan-sqlite.sh, {{source}}, tasks]   # database file, table; the script is in contrib/backends/sqlite",
		Section: "# env: {TKAN_SQLITE_FIELDS: \"id=key,title=summary,status=state\"}   # only where a column name differs from the field\n" +
			"# poll: 5s             # re-run `list` this often to notice outside changes (default: never; press r)\n" +
			"# timeout: 10s",
		Notes: []string{"sqlite: copy contrib/backends/sqlite/tkan-sqlite.sh next to this file (needs bash, jq and sqlite3)"},
	}, nil
}
