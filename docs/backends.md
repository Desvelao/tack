# Backends

tkan talks to a task store only through `backend.Backend`
(`src/internal/backend/backend.go`). The config's `backend:` list names the **type** of each
store (`csv`, `custom`, ...) and its `options`; every entry becomes one `Backend` instance, and
`backend.Set` (`set.go`) combines them for the CLI and TUI: it tags tasks with the entry's name
(`Task.Backend`), unions the statuses, and routes `Update`/`Create`/`Delete` by that name.
Backends themselves never see the name on a task.

## Task ids are not reused

The csv, json, jsonl and yaml backends keep a high-water mark, the highest id they handed out, in a small
sidecar written atomically under the same lock as the create: `<file>.nextid` next to the data file. A new id is `max(max numeric id present, mark) + 1`, so
after `tkan delete 12` the next `tkan add` does not return 12 again. A missing or unreadable sidecar degrades to
`max + 1`. The SQLite script (a `custom` backend) assigns ids itself: tables with `AUTOINCREMENT` never reuse one; for other tables the id is `max + 1`. Every `custom` backend assigns its own ids. Add `*.nextid` to `.gitignore` if the backlog is
in a repository.

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

// A guard from backend.WithExpect(ctx, field, value) travels in the ctx of Update and
// Patch: a backend checks it against the stored task inside its lock (custom: the
// command does, when its list reports "guard":true) and returns backend.ErrPrecondition without
// writing. Backends that cannot (a custom command without "guard") get a best-effort fresh-read
// check from Set.Update instead.
//
// Optional: write only what an edit changed. Patch stores t over the task as the
// store holds it now (read again under the lock), but only the fields and extra
// keys that differ between base, the copy the caller read, and t. csv, json,
// jsonl, yaml and custom (which sends the base to the command) implement it; backend.Set uses it whenever the
// caller knows the original (backend.WithOld: the board's move and edit, `edit`,
// `set-status`), so an agent's edit of another field is not written back
// over. Without Patch (custom) or without the original, Update replaces the whole record.
//
// Extras and deletion: Update never removes an extra key or column; one the copy
// lacks is kept (it may be a column the caller never saw). Patch removes an extra
// that the caller dropped relative to its base. Setting it to "" is not a removal
// (json/yaml keep an empty-string key). The shipped CLI and board do not edit
// extras, so today only a Patcher caller or a direct file edit clears one.
type Patcher interface{ Patch(ctx, base, t task.Task) error }

// Optional, for stores with a fixed set of columns (csv, sqlite): whether a value
// written to the logical field is kept. `tkan claim` refuses a claim whose
// assignee/lease would be dropped. Backends without it are assumed to store every field.
type FieldStorer interface{ Stores(field string) bool }

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

## SQLite: the `tkan-sqlite.sh` script

SQLite is provided as a script for the [`custom` backend](#the-custom-backend), shipped as `contrib/backends/sqlite/tkan-sqlite.sh`. Copy it
next to your `tkan.yaml`, or anywhere and give its path. It needs `bash`, `jq` and the `sqlite3`
command line tool (3.38 or newer, for the JSON functions); tkan links no SQLite library.

```yaml
backend:
  - name: tasks
    command: [./tkan-sqlite.sh, tasks.db, tasks]   # database file, table (default: tasks)
    options:
      env: {TKAN_SQLITE_FIELDS: "id=key,title=summary,status=state"}   # only where the column name differs
      poll: 5s     # outside changes are noticed by re-running `list` (default: never; press r)
```

`tkan init -backend sqlite` writes this entry. `TKAN_SQLITE_CLI` names another SQLite tool
(default `sqlite3`). The script has no file stamp to watch: outside changes are noticed by `poll`
re-running `list` every N seconds, or by `r`.

- **Safety.** The whole request is handed to SQLite as one hex blob literal and read back with
  `json_extract`, so no quote, `;` or leading `.` in a title can alter a statement or reach the CLI as a
  dot command. Table and column names are quoted; the script refuses control characters in them and NUL
  characters in values.
- **Concurrency and claims.** Writes run in one `BEGIN IMMEDIATE` transaction with a 5 s busy timeout,
  so several tkan processes (and other programs using the database) are serialised by SQLite itself.
  The script reports `"guard":true`, so `claim`, `release`, `next`, `set-status -from` and `edit -if` are
  atomic: the `expect` check is part of the `UPDATE`. The table layout read first is checked
  (`schema_version`) inside the transaction, and the write is retried once if another process altered
  the table meanwhile.
- **Partial updates.** tkan sends `base`, the task as it was read, and the script writes only the columns
  whose value differs, so another writer's change to a different field survives.
- **Layout.** The columns are mapped to the fields by name (case-insensitive) or by
  `TKAN_SQLITE_FIELDS`; `id`, `title` and `status` are required. Unmapped columns are kept as extra
  fields and left alone by updates. New tasks get the next rowid when `id` is an `INTEGER PRIMARY KEY`,
  otherwise `max(numeric id) + 1`. A missing table (or database) is an empty backlog; the first `add`
  creates it with one column per field. A `NOT NULL` column without a default that tkan does not map
  makes `add` fail naming it.
- **NULLs.** A NULL reads as an empty string; unchanged columns are left alone, but setting a column to
  an empty value stores `''`.
- **Limits.** One `sqlite3` process (and a few `jq` ones) per operation, noticeable on a Raspberry Pi with
  a huge table; no change detection by file stamp (use `poll`); rows come back in rowid order; a column
  whose name contains a double quote cannot be written; `WITHOUT ROWID` tables are untested. Test it with
  `make test-contrib`.

## The `custom` backend

```yaml
backend:
  - name: tracker
    command: [./tracker.sh, --project, demo]   # program + args, no shell; implies type: custom
    options:
      timeout: 10s
      # poll: 5s     # optional: re-run `list` this often to notice outside changes
```

`command:` on the entry means `type: custom`. `type: custom` with
`options: {command: [...]}` is equivalent; setting the command in both places is an error.

For **every operation** tkan starts the command (working directory: `cwd`, default the config
directory), writes one JSON object to its stdin and reads one JSON object from its stdout. At
start-up tkan runs one `list` to check the command works.

| Request (stdin) | Response (stdout) |
|---|---|
| `{"op":"list"}` | `{"tasks":[Task, ...], "statuses":["todo","done"]}` (`statuses` optional: default is the statuses of `tasks`, first-seen order; give it to expose columns that have no task yet). May also carry `"guard":true` and `"fields":[...]`, see below. |
| `{"op":"create","task":Task}` | `{"task":Task}` with the assigned `id` (required) |
| `{"op":"update","task":Task}` | `{}` (replaces the task with that `id`). May carry `"expect":{"field":"assignee","value":""}`, see below. |
| `{"op":"delete","id":"7"}` | `{}` |

`Task` is the JSON shown by `tkan list -json`: `id`, `title`, `status` (raw value),
`description`, `priority`, `notes`, `type`, `assignee`, `due`, `start`, `labels`, `blocked_by` (comma-separated ids), `lease` (RFC 3339 time a claim runs out, see `tkan claim`), `claimed_at` (RFC 3339 time it started) and `extra` (a string map
to round-trip unknown fields); empty fields may be omitted. In a `list` response `tasks` is required (`[]` for an empty board; a missing or misspelled key is an
error, not an empty list) and every `id` must be non-empty and unique. `column` and `backend` are never sent.

Every request also has `"version":1`.

**Atomic claims.** `tkan claim`, `release`, `next` and `run -claim` write with a guard: an `update`
carries `"expect":{"field":"assignee","value":""}` and must be applied only if the stored task still
has that value in that field. A command that checks this under its own lock or transaction (a
`BEGIN IMMEDIATE`, `flock`, a compare-and-set in an API) answers `{"error":"...","code":"precondition"}`
when it does not hold (tkan exit code 4) and reports `"guard":true` in its `list` answer. Without
`"guard":true` tkan still checks against a fresh `list` before the write, which is not atomic: two
agents racing for one task can both win. A `list` answer may also give `"fields":["title","status",...]`,
the logical fields the store keeps; tkan then knows a missing one (for example `lease`) is not stored.
Without it every field is assumed to be kept.

Errors: any response with `{"error":"message"}` (optionally `"code"`), or a non-zero exit status
(stderr is shown). `"code":"not_found"` or exit status 3 means the ID does not exist (tkan exit code 3);
`"code":"unsupported"` marks an operation the tool cannot do; `"code":"precondition"` means the `expect` guard did not hold. Output that is not valid JSON is an
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
