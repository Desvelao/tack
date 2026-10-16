# Configuration (`tkan.yaml`)

tkan is configured by one YAML file: the **list of backends** (where tasks come from) and the
**board** (how raw statuses become kanban columns). Everything under `board` is optional; the
`backend` list is required.

## Where the config comes from

| Order | Source |
|---|---|
| 1 | `-c FILE` |
| 2 | `$TKAN_CONFIG` |
| 3 | `./.tkan.yaml`, else `./.tkan.yml`, `./tkan.yaml`, `./tkan.yml` (first found wins, in this order) |
| 4 | `~/.tkan.yaml`, else `~/.tkan.yml` |

Parent directories are not searched.

If none exists tkan stops and suggests `tkan init`. There is no `-f FILE`, `$TKAN_FILE`
or sidecar file any more: every data source is an entry of the `backend` list. Relative paths in
`options` are resolved against the **config file's directory**.

`tkan config` prints what was actually resolved (config file, backends with their names, final columns).

## Generating a config

```bash
tkan init -source tasks.csv      # writes ./tkan.yaml, derived from the file
```

Flags: `-backend TYPE` (default: from the source's extension, else `csv`), `-source SRC`
(or a positional `SRC`), `-o PATH` (`-` for stdout; default `./tkan.yaml`), `-force` (overwrite).

With an existing CSV the options list the detected columns (unmatched required fields become
commented `TODO` lines), and `board.columns` has one column per status value found, ordered as a
workflow. Data that uses only the default values (`pending`, `in_progress`, `done`) also gets the
missing default columns (`failed`) so cards can be moved there.

## Reference

```yaml
backend:
  - type: csv
    name: tasks            # optional; default "csv1"
    options:
      source: tasks.csv
      header: true
      fields:
        id: id
        title: title
        status: state
        description: 4
  - type: custom           # default name "custom2"
    options:
      command: [./tracker.sh]

board:
  columns:
    - name: Backlog
      values: [pending, todo, ""]
    - name: Doing
      values: [in_progress, wip]
      write: in_progress
    - name: Done
      values: [done]
  unmapped: append
```

Unknown keys are rejected. In particular the former single-backend format (`backend: csv`,
`source:`, a root `csv:` section) is no longer read; move those settings into a `backend` entry.

### `backend` list

| Key | Default | Meaning |
|---|---|---|
| `type` | required | Backend type: `csv`, `sqlite` or `custom`. |
| `name` | `<type><position>` | Unique name (case-insensitive; letters, digits, `_`, `.` and `-` only). The default uses the 1-based position in the list: `csv1`, `custom2`, ... Shown on every card and usable as `-b NAME`, `NAME:ID` and the `backend` filter field. |
| `options` | | Settings of the type, below. |

All backends are shown on one board and share the `board` section. A card's column is decided by
its raw status, whichever backend it comes from. Task IDs only need to be unique within a backend.

### `csv` options

| Key | Default | Meaning |
|---|---|---|
| `source` | required | Path of the CSV file (must exist). |
| `header` | `true` | `false`: the file has no header row. Fields must then be given as positions. |
| `poll` | `watch` | How the board notices outside edits of the file: `watch` checks it every second (modification time and size) and reloads on a change; `none` reads it once at start; a number of seconds (`5`, or `5s`) checks it at most that often. Whatever the value, `r` in the board reloads now and every write re-reads the file first, so outside edits are never overwritten. Subcommands always read the current file. |
| `fields.<field>` | the column named `<field>` | Which source column holds the logical field. A string is a header name (case-insensitive); a number is a 1-based position. |

Logical fields: `id`, `title`, `status` (required), `description`, `priority`, `notes`, `type`, `assignee`, `due`, `start` (dates as `YYYY-MM-DD`), `labels` (comma-separated). All but the first three are optional.

- Unmapped source columns are kept as extra fields and written back unchanged. In a headerless
  file they are named `col<N>`.
- An optional field that is not in the file is added as a new column (named as configured) the
  first time a task gets a value for it.
- Two fields cannot point at the same column.
- IDs may be any string. New tasks get `max(numeric id) + 1` (so `1` if all ids are like `A-1`).

### `sqlite` options

Needs a SQLite command line tool on the machine (see [backends.md](backends.md#the-sqlite-backend)); tkan links no SQLite library.

| Key | Default | Meaning |
|---|---|---|
| `source` | required | Path of the database file (relative to the config). It may not exist yet: the first `add` creates it. |
| `table` | `tasks` | Table (or view) that holds the tasks. Created with one column per field if it does not exist. |
| `cli` | first installed of `sqlite3`, `sqlite` | The CLI to run: a program name, or a list (program and arguments, no shell) such as `[docker, exec, -i, db, sqlite3]`. It must understand `-json` (SQLite 3.33 or newer). |
| `poll` | `watch` | As for csv: `watch` checks the database file (and its `-wal`) every second, `none` reads once at start, a number of seconds checks at most that often. `r` reloads now. |
| `fields.<field>` | the column named `<field>` | Which column holds the logical field (case-insensitive). `id`, `title` and `status` are required. |

- Unmapped columns are kept as extra fields and left untouched by updates.
- New tasks get the next rowid when `id` is an `INTEGER PRIMARY KEY`, otherwise `max(numeric id) + 1`.
- Values are stored as text; an empty field is stored as `''`, never `NULL`. NUL characters are rejected.

### `custom` options

| Key | Default | Meaning |
|---|---|---|
| `command` | required | Program and arguments as a list (no shell). Run for every operation. |
| `cwd` | the config's directory | Working directory; a relative `command[0]` such as `./tool.sh` is resolved from it. |
| `timeout` | `10s` | Per call; the process is killed after it. |
| `poll` | not set | How often (e.g. `5s`) the TUI re-runs `list` in the background to notice outside changes and reload. Unset: no polling (press `r`). Must be greater than zero. |
| `env` | | Extra environment variables (map). |

The JSON protocol is described in [backends.md](backends.md#the-custom-backend).

### `board` section

| Key | Default | Meaning |
|---|---|---|
| `columns` | pending, in_progress, done, failed | Ordered list of columns, left to right. Any count. |
| `columns[].name` | required | Title shown on the board and accepted by `set-status`, `add -status`, `list -status`. Unique, case-insensitive. |
| `columns[].values` | `[name]` | Raw status values shown in this column. Matching ignores case and surrounding spaces. A value can belong to one column only. `""` matches an empty status. |
| `columns[].write` | first of `values` | Raw value stored when a card is moved or created in this column; must be one of `values` (checked at load). |
| `columns[].limit` | `0` (none) | WIP limit. The header shows `NAME (n/limit)` and ends in `!` when the column holds more cards than that; moving or `set-status` into a full column still works but warns (board notice, stderr). Counts every card in the column, ignoring the filter. |
| `columns[].color` | none | Colour of the column header and of its selected card: an ANSI number `0`-`255` or `#rrggbb`. The focused column's header is underlined. Terminals with fewer colours get the nearest one; invalid values are rejected at load. |
| `columns[].sort` | `board.sort` | Sort keys for this column only, same syntax as `board.sort`. |
| `search` | `[id, title, description, notes]` | Fields a plain text filter term is matched against (see [filter.md](filter.md)). Any field name works, including unmapped CSV columns by name; unknown names fail at start-up. |
| `group` | none | Field the cards of each column are grouped by (`priority`, `type`, `assignee`, `labels` or any field); `z` on the board cycles it. A dim `-- value (n) --` line starts each group; cards keep their sort order inside a group and groups appear in order of first appearance. Cards with several labels are grouped under the first label. Unknown fields fail at start-up. |
| `sort` | store order | Display order of cards, see [Sorting cards](#sorting-cards-boardsort). |
| `unmapped` | `append` | Raw values no column claims: `append` (one column each, after the configured ones), `other` (one trailing `other` column), `hide` (not shown). |

### Sorting cards (`board.sort`)

`sort` sets the order of the cards inside each column. It only changes what the board shows:
nothing is written to a backend, and `list`/`show` keep store order.

```yaml
board:
  sort:
    - priority: [high, medium, low]   # explicit order for this field
    - -id                             # leading "-" = descending
```

Each item is `field`, `-field` (descending), `field:v1,v2,v3` (compact explicit order, quote it in
YAML) or the mapping `field: [v1, v2, v3]`. Keys apply in order; later ones break ties. Without an
explicit list, `priority` orders by urgency (critical/urgent/p0, high/p1, medium/normal/p2, low/minor/p3, trivial/lowest/p4; a bare 1-5 with 1 most urgent; unknown values after them), numbers compare numerically and text case-insensitively with digit runs as numbers
(`2 < 10`, `A-2 < A-10`). With a list, listed values come first in that order and unlisted ones
after them. Empty values are always last. Equal cards keep store order. Fields are any filter field
(`id title description status priority notes type assignee due start labels column backend` or an extra column); an unknown
one fails at start-up. In the board, `s` cycles the sort field and `S` reverses it for the session
(a session sort applies to every column).

A column can have its own `sort`, used instead of `board.sort` inside that column:

```yaml
board:
  sort: [priority: [high, medium, low]]
  columns:
    - {name: Backlog}
    - {name: Done, sort: [-id]}      # newest first
```

Moving a card (`H`/`L`, `set-status`) writes the destination column's `write` value; the raw
source value is never rewritten otherwise.

## Examples

### 1. Default columns

Without a `board` section the columns are:

```yaml
board:
  columns:
    - {name: pending}
    - {name: in_progress}
    - {name: done}
    - {name: failed}
```

### 2. Different schema and workflow

`issues.csv`:

```
key,summary,state,assignee
A-1,Ship it,open,ann
A-2,Review it,in review,bob
```

`tkan.yaml`:

```yaml
backend:
  - type: csv
    options:
      source: issues.csv
      fields: {id: key, title: summary, status: state}
board:
  columns:
    - {name: Todo,  values: [open, new]}
    - {name: Doing, values: ["in review", wip], write: "in review"}
    - {name: Done,  values: [closed]}
```

```bash
tkan                                  # board with Todo / Doing / Done (+ any unmapped value)
tkan set-status A-1 Doing             # writes "in review" in the `state` column
```

### 3. Headerless file, columns by position

`todo.csv` (no header): `1,Buy milk,todo,urgent`

```yaml
backend:
  - type: csv
    options:
      source: todo.csv
      header: false
      fields: {id: 1, title: 2, status: 3, notes: 4}
board:
  columns:
    - {name: Todo, values: [todo]}
    - {name: Done, values: [done]}
```

## Errors

Invalid configuration fails at start-up with exit code 1 and a message naming the file and the
problem: duplicate column names, a value mapped to two columns, unknown `fields` keys in a csv entry, a missing or duplicate backend `type`/`name`, unknown top-level keys (including the old single-backend format),
unknown `unmapped` mode, a required field pointing at a missing column, or a headerless file
without positions for `id`, `title` and `status`.

## Saved views

`board.views` names filters you use often. Each is `{name, query}` with the query language of
[filter.md](filter.md):

```yaml
board:
  views:
    - {name: mine,    query: "assignee=ann col!:done"}
    - {name: urgent,  query: "priority=high,medium col:doing"}
```

On the board, `v` applies the next view (the header shows `view NAME: QUERY`) and cycles back to
no filter; typing a filter with `/` or `esc` leaves the view. On the command line,
`tkan list -v mine` applies one (case-insensitive) and combines with `-q`
(`list -v mine -q 'type=fix'` needs both). Names must be unique and the query must parse; field
names are checked when the view is used, so a typo is reported then.

## Audit trail

```yaml
audit:
  file: audit.jsonl     # or audit.csv; relative to this file
```

Without `audit.file` the audit trail (reloads, creates, updates, deletes; see
[usage.md](usage.md#audit-trail)) is kept in memory and ends with the run. With it, every run appends
to the file, so the board shows this session and the previous one. `.jsonl` is one JSON object per
line; `.csv` has the header `time,session,source,op,backend,id,result,details`. Any other extension is
a config error. Lines tkan cannot parse are skipped, a file that cannot be written is reported and
the entries stay in memory. The file may grow; delete or rotate it when you like.

## Default backend and colour

`default_backend: NAME` (top level, next to `backend:`) makes one backend the default: `add`
creates there and a bare ID in `show`, `set-status`, `edit` and `delete` is looked up only
there, so it is never ambiguous. `-b NAME` and the `NAME:ID` form still pick another backend.
The environment variable `$TKAN_BACKEND` overrides the key. An unknown name is an error.

The board honours [`NO_COLOR`](https://no-color.org) (and `CLICOLOR=0`): with it set the colours
are dropped. CLI output is never coloured.
