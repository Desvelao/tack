# Backends

tkan talks to a task store only through `backend.Backend`
(`src/internal/backend/backend.go`). The config's `backend:` list names the **type** of each
store (`csv`, `custom`, ...) and its `options`; every entry becomes one `Backend` instance, and
`backend.Set` (`set.go`) combines them for the CLI and TUI: it tags tasks with the entry's name
(`Task.Backend`), unions the statuses, and routes `Update`/`Create`/`Delete` by that name.
Backends themselves never see the name on a task.

## Interface

```go
type Backend interface {
    List(ctx) ([]task.Task, error)             // all tasks, store order
    Statuses() []string                        // raw status values present, first-seen order
    Update(ctx, task.Task) error               // replace by ID; ErrNotFound if missing
    Create(ctx, task.Task) (task.Task, error)  // assigns ID; stores Status as given
    Delete(ctx, id string) error               // ErrNotFound if missing
    Close() error
}

// Optional: enables live refresh in the TUI (polled about once a second).
// A backend that may serve a cached copy (csv with `poll: none`/N) can also
// implement `Reload() error` (backend.Reloader); the board's `r` key calls it.
type Watcher interface{ Changed() (bool, error) }

// Registered per type name; Spec is one entry of the config's backend list.
type Factory func(spec backend.Spec) (Backend, error)
type Spec struct {
    Type, Name string
    Options    yaml.Node // the entry's `options:` block (Kind == 0 when absent)
    BaseDir    string    // config directory; spec.Resolve(p) makes relative paths absolute
}
```

Backends are **raw**: `Task.Status` is whatever string the store holds. Kanban columns,
their names/order and the raw-value → column mapping live in `internal/board` and the `board:`
config section, so every backend gets them for free. A backend never needs to know about columns.

## Configuration

A factory receives its entry as a `backend.Spec`. Decode `spec.Options` with
`spec.Options.Decode(&yourStruct)` (when `Kind != 0`), resolve file paths with `spec.Resolve(path)`,
and report problems as errors; they surface at start-up prefixed with the entry name.

Schema mapping (which column/field is the status, etc.) belongs in the backend's own `options`,
as `fields` does for csv; document it in `docs/config.md`.

Rules for implementations:

- Return `backend.ErrNotFound` (wrapped is fine) for unknown IDs; the CLI maps it to exit code 3.
- Do not default the status in `Create`; the caller supplies it (it comes from the column's `write` value).
- Round-trip fields you do not understand via `Task.Extra`.
- `List`/`Changed` are called often: keep them cheap (cache, conditional requests, ETags).
- Writes must be safe against concurrent edits by other processes (re-read, then apply).
- Operations a store cannot support return `backend.ErrUnsupported`.

## Sample configuration

`tkan init` asks the backend for a sample via `backend.RegisterExample(type, fn)`. `fn(location)`
returns `backend.Example{Section, Statuses, Notes}`:

- `Section`: the YAML text of the entry's `options` body (unindented; `init` indents it), derived
  from the data when it exists (detected field/column mapping) and a generic template when it
  does not. The token `{{source}}` is replaced by the quoted source path, relative to the output file.
- `Statuses`: the raw status values found, first-seen order; the generator turns them into
  `board.columns` (ordered as a workflow).
- `Notes`: what could not be detected; shown on stderr.

## Adding a backend

1. Create `src/internal/backend/<name>/<name>.go` and implement `Backend`.
2. Register it:
   ```go
   func init() { backend.RegisterType("db", Open, ".db") } // Open(spec backend.Spec); extensions are optional (init's guess)
   ```
3. Blank-import it in `src/cmd/tkan/main.go`.
4. Register a sample generator with `backend.RegisterExample` (see `csv/example.go`).
5. Add tests next to it (see `csv/csv_test.go` for round-trip, create/delete, concurrent-edit cases).
6. Document its `options` in `docs/config.md` and add a row below.

## Implemented

| Type | Package | Notes |
|---|---|---|
| `csv` | `internal/backend/csv` | `options.source`/`header`/`fields` map the file by header name or position, headerless files, atomic write (fsync + rename, symlinks followed), CRLF, BOM and a missing final newline preserved, writes serialised across processes with an advisory lock on `<file>.lock` (a harmless empty sidecar file; a writer waits up to 5 s for another tkan, then fails with `locked by another tkan`; implemented with flock on Unix and LockFileEx on Windows, the latter never run there), implements `Watcher` |
| `json`, `jsonl` | `internal/backend/json` | a JSON file (array, or object with `tasks`) or JSON Lines; built on `internal/backend/filestore` (below); implements `Watcher` and `Reloader` |
| `yaml` | `internal/backend/yaml` | a YAML list of tasks (or mapping with `tasks`); same engine as `json`; selected with `type: yaml` only (no extension claim) |
| `markdown` | `internal/backend/markdown` | a directory of `.md` files, one task each (below); implements `Watcher` and `Reloader` |
| `sqlite` | `internal/backend/sqlite` | a table of a SQLite database, accessed through a SQLite command line tool (no library linked, so the binary stays static); `options.source`/`table`/`fields`/`cli`/`poll` (below); writes in one `BEGIN IMMEDIATE` transaction; implements `Watcher` and `Reloader` |
| `custom` | `internal/backend/custom` | runs an external command per operation, JSON over stdin/stdout (below); implements `Watcher` only when `options.poll` is set, otherwise press `r` to reload |

## File backends: `json`, `jsonl`, `yaml`

These share `internal/backend/filestore`: a `Store` keeps the tasks of one file in memory and does
what csv does for safety (see [config.md](config.md#json-jsonl-and-yaml-options) for the options): every
write takes the `<file>.lock` lock, re-reads the file if it changed (outside edits are merged), applies the
change, and replaces the file through a temp file + rename in the same directory (symlinks are followed, the
mode is kept); a failed write leaves the in-memory copy as it was, and a file that no longer parses (or
repeats an id) keeps the last good state. A backend only provides a `filestore.Codec` (`Decode`/`Encode`)
and a key mapping (`filestore.Mapping`: record keys <-> task fields, everything else in `Extra`).
`json` and `yaml` can keep the list inside a larger document (`options.path`, dotted keys): only that list is replaced on a write.
The csv backend uses the same lock and atomic-write helpers but keeps its own engine for its header,
BOM and line-ending handling.

## The `markdown` backend

```yaml
backend:
  - type: markdown
    options:
      source: tasks/        # tasks/12.md, tasks/fix-login.md, ...
```

```markdown
---
title: Fix the login
status: todo
labels:
  - auth
owner: ann            # not a task field: kept in `extra`
---

Steps to reproduce...
```

- The id is the file name without `.md`; new tasks are `max(numeric id) + 1`. `list` order is numeric ids
  ascending, then the other names alphabetically.
- Each write replaces one file atomically under a `.tkan.lock` in the directory (hidden, ignored by the
  scan). A write rewrites the frontmatter from the task's fields: comments inside it are not kept, and
  `\r\n` becomes `\n`. Writing does not touch the other files.
- `Changed` compares the name, size and modification time of every `.md` file, so adding, removing or
  editing a file by hand shows up on the board.
- The body is the description (outer blank lines trimmed); a `description:` key in the frontmatter wins over
  the body when it is present.

## The `sqlite` backend

```yaml
backend:
  - type: sqlite
    options:
      source: tasks.db
      table: tasks
      # cli: sqlite3                          # default: first installed of sqlite3, sqlite
      fields: {id: key, title: summary, status: state}   # only where the column name differs
```

tkan does not link a SQLite library. For every operation it starts a SQLite command line tool,
writes one SQL script to its stdin and reads the rows as JSON (`-json`) from its stdout, so the CLI
must be installed (`apt install sqlite3`, `apk add sqlite`, `brew install sqlite`, ...) and be
SQLite 3.33 or newer.

- **Which CLI.** `options.cli` (a program name or a list, no shell) if set; otherwise the first of
  `sqlite3`, `sqlite` that is on `PATH` and prints JSON. When none works the error lists what was
  tried. A list lets you run the tool elsewhere, e.g. `[docker, exec, -i, db, sqlite3]` (then
  `source` is the path inside the container and outside edits are not noticed by `Changed`).
- **Safety.** Values never travel as SQL text: they are sent as hex literals
  (`CAST(X'..' AS TEXT)`), so no quote, `;` or leading `.` in a title can alter the statement or reach
  the CLI as a dot command. Table and column names come from the config and are quoted.
- **Concurrency.** Writes run in one `BEGIN IMMEDIATE` transaction with a 5 s busy timeout, so
  several tkan processes (and other programs using the database) are serialised by SQLite itself;
  there is no lock file. Updates touch only the mapped columns (and extras the task carries).
- **Changes by others.** `Changed` compares the modification time and size of the database file and
  its `-wal`; see `poll` in [config.md](config.md#sqlite-options).
- **Limits.** One process per operation (a start-up cost per `list`/write, noticeable on a Raspberry
  Pi with a huge table); `WITHOUT ROWID` tables are not supported; rows come back in the table's
  natural (rowid) order; a database that needs an encryption key or a special build must be
  reached through `options.cli`.

## The `custom` backend

```yaml
backend:
  - type: custom
    name: tracker
    options:
      command: [./tracker.sh, --project, demo]   # program + args, no shell
      timeout: 10s
      # poll: 5s     # optional: re-run `list` this often to notice outside changes
```

For **every operation** tkan starts the command (working directory: `cwd`, default the config
directory), writes one JSON object to its stdin and reads one JSON object from its stdout. At
start-up tkan runs one `list` to check the command works.

| Request (stdin) | Response (stdout) |
|---|---|
| `{"op":"list"}` | `{"tasks":[Task, ...], "statuses":["todo","done"]}` (`statuses` optional: default is the statuses of `tasks`, first-seen order; give it to expose columns that have no task yet) |
| `{"op":"create","task":Task}` | `{"task":Task}` with the assigned `id` (required) |
| `{"op":"update","task":Task}` | `{}` (replaces the task with that `id`) |
| `{"op":"delete","id":"7"}` | `{}` |

`Task` is the JSON shown by `tkan list -json`: `id`, `title`, `status` (raw value),
`description`, `priority`, `notes`, `type`, `assignee`, `due`, `start`, `labels` and `extra` (a string map
to round-trip unknown fields); empty fields may be omitted. In a `list` response `tasks` is required (`[]` for an empty board; a missing or misspelled key is an
error, not an empty list) and every `id` must be non-empty and unique. `column` and `backend` are never sent.

Errors: any response with `{"error":"message"}` (optionally `"code"`), or a non-zero exit status
(stderr is shown). `"code":"not_found"` or exit status 3 means the ID does not exist (tkan exit code 3);
`"code":"unsupported"` marks an operation the tool cannot do. Output that is not valid JSON is an
error, as is stdout above 32 MiB. A call that exceeds `timeout` is killed together with the
processes the command started (its process group, on Unix). Processes a command leaves running in
the background are also killed when the call returns, and a command that exits 0 with a valid answer
is not treated as failed just because such a child kept its output open.

Minimal example (`sh`; a real tool would use `jq` or a script language):

```sh
#!/bin/sh
case "$(cat)" in
  *'"op":"list"'*) echo '{"tasks":[{"id":"1","title":"Hello","status":"todo"}]}' ;;
  *)               echo '{"error":"read-only","code":"unsupported"}' ;;
esac
```
