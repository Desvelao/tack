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

Flags: `-backend TYPE` (default: from the source's extension, else `csv`; `yaml` needs it), `-source SRC`
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
  - command: [./tracker.sh]   # a command implies type: custom; default name "custom2"

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

action_log: {file: actions.jsonl}   # optional, off by default; see "Action log" below
actions:                   # optional; see "Actions" below
  - name: notify
    command: [notify-send, "tkan #{id}", "{title}"]
```

Unknown keys are rejected.

### `backend` list

| Key | Default | Meaning |
|---|---|---|
| `type` | required unless `command` is set | Backend type: `csv`, `json`, `jsonl`, `yaml` or `custom` (SQLite is a `custom` script, see below). |
| `command` | | An external command (list, no shell): the entry is a `custom` backend, no `type` needed. See [backends.md](backends.md#the-custom-backend). |
| `name` | `<type><position>` | Unique name (case-insensitive; letters, digits, `_`, `.` and `-` only). The default uses the 1-based position in the list: `csv1`, `custom2`, ... Shown on every card and usable as `-b NAME`, `NAME:ID` and the `backend` filter field. |
| `disabled` | `false` | `true` keeps the entry in the file but skips it: it is validated, not opened (no cards, `-b NAME` unknown) and its `actions`/`hooks` are ignored. Other entries keep their default names. `default_backend` cannot name a disabled entry, and at least one entry must stay enabled. |
| `options` | | Settings of the type, below. |
| `actions` | | Actions for this backend's tasks only ([Per-backend actions and hooks](#per-backend-actions-and-hooks)). |
| `hooks` | | Hooks for this backend's tasks only (same section). |

All backends are shown on one board and share the `board` section. A card's column is decided by
its raw status, whichever backend it comes from. Task IDs only need to be unique within a backend.

### `csv` options

| Key | Default | Meaning |
|---|---|---|
| `source` | required | Path of the CSV file (must exist). |
| `header` | `true` | `false`: the file has no header row. Fields must then be given as positions. |
| `poll` | `watch` | How the board notices outside edits of the file: `watch` checks it every second (modification time, size, inode and change time, plus a content hash for small files modified in the last 2 seconds, so a same-size edit on a coarse-clock file system is not missed) and reloads on a change; `none` reads it once at start; a number of seconds (`5`, or `5s`) checks it at most that often. Whatever the value, `r` in the board reloads now and every write re-reads the file first, so outside edits are never overwritten. Subcommands always read the current file. |
| `fields.<field>` | the column named `<field>` | Which source column holds the logical field. A string is a header name (case-insensitive); a number is a 1-based position. |

Logical fields: `id`, `title`, `status` (required), `description`, `priority`, `notes`, `type`, `assignee`, `due`, `start` (dates as `YYYY-MM-DD`), `labels` (comma-separated), `blocked_by` (ids of tasks of the same backend to finish first, comma-separated; see the `blocked`/`ready` filter fields in [filter.md](filter.md)), `lease` (RFC 3339 UTC time the claim of the `assignee` runs out, written by `tkan claim -lease`; synonyms `lease_until`, `claimed_until`), `claimed_at` (RFC 3339 UTC time the claim started, written with every claim and shown by the fleet view; optional even for claims, synonyms `claimed`, `claim_start`). All but the first three are optional; `tkan claim` needs an `assignee` column, and `lease` for `-lease`.

- Unmapped source columns are kept as extra fields and written back unchanged. In a headerless
  file they are named `col<N>`.
- An optional field that is not in the file is added as a new column (named as configured) the
  first time a task gets a value for it.
- Two fields cannot point at the same column.
- IDs may be any string. New tasks get `max(numeric id) + 1` (so `1` if all ids are like `A-1`).

### `json`, `jsonl` and `yaml` options

One file that must already exist (an empty file is fine). `json` holds an array of task objects or an object with a `tasks` array (other top-level keys are kept); `jsonl` has one object per line; `yaml` holds a list of mappings or a mapping with a `tasks` list. `tkan init` guesses `json` from `.json` and `jsonl` from `.jsonl`/`.ndjson`; `yaml` is never guessed from the extension (it would clash with `tkan.yaml`), use `-backend yaml`.

| Key | Default | Meaning |
|---|---|---|
| `source` | required | Path of the file (relative to the config). |
| `fields.<field>` | the key named `<field>` | Which key of the objects holds the logical field (case-insensitive). `id`, `title` and `status` are the usual ones; `title` and `status` are written even when empty. |
| `poll` | `watch` | As for csv: `watch` checks the file every second, `none` reads once at start, a number of seconds checks at most that often. `r` reloads now. |
| `path` | the top level, or the `tasks` key | `json` and `yaml` only: where the task list lives, as dotted object keys, e.g. `data.tasks` for `{"data": {"tasks": [...]}}`. The value must be a list of task objects; every other key (and, in YAML, comment) is kept on writes. A non-empty file without that path is an error; an empty file (or `{}`) gets it on the first `add`. Keys that contain a dot cannot be addressed; there are no list indexes. `tkan init` fills it in when the file has exactly one nested list of objects. |
| `indent` | `2` | Spaces per level when writing (`json`: `0` = compact; `jsonl` always writes one compact object per line). |

- Keys that are not mapped are kept in `extra` and written back; numbers, booleans and nested values are restored with their type (nested ones appear as JSON text in `extra`).
- Ids may be strings or numbers. A file keeps the type its ids have (new `yaml` files use numbers); new tasks get `max(numeric id) + 1`.
- `labels` and `blocked_by` are written as lists (as a string when the file already holds strings; `blocked_by` ids as numbers when the file's ids are numbers); on read both forms work. Dates are always `YYYY-MM-DD` strings (quoted in YAML).
- Writes use a temp file + rename and a `<file>.lock` sidecar, like csv. `yaml` keeps comments outside the task list; comments inside it are lost on a write.

### SQLite

SQLite is a script for the `custom` backend, not a `type`: see [backends.md](backends.md#sqlite-the-tkan-sqlitesh-script).

### `custom` options

| Key | Default | Meaning |
|---|---|---|
| `command` | required | Program and arguments as a list (no shell). Run for every operation. Normally written on the entry itself (`- command: [./tool.sh]`, which implies `type: custom`); `options.command` (with `type: custom`) is equivalent, but not both. |
| `cwd` | the config's directory | Working directory; a relative `command[0]` such as `./tool.sh` is resolved from it. |
| `timeout` | `10s` | Per call; the process is killed after it. |
| `poll` | not set | How often the TUI re-runs `list` in the background to notice outside changes and reload: a number of seconds (`5`) or a duration (`5s`), as for the other backends. Unset or `none`: no polling (press `r`). `watch` has no meaning here (there is no file to watch) and is the same as unset. A number must be greater than zero. |
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
| `agents` | none | Assignee names of AI agents, e.g. `[claude, codex]` (case-insensitive, whole value). Cards assigned to one of them get a coloured border and a bold `@name`, and their details say `agent`. Display only; empty names are rejected at load. They also appear as `idle` in the fleet view (`F`, `tkan fleet`) when they hold nothing. |
| `avatars` | none | Pictures for the fleet view: `NAME: [frame, ...]` per agent (emoji or short text; case-insensitive), e.g. `claude: ["🤖", "🦾"]`. A busy agent cycles its frames twice a second; otherwise the first stands still. Without it: 🤖/🦾 busy, ⌛ expired, 👤 held, 💤 idle. At least one non-empty frame per agent. |
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
(`id title description status priority notes type assignee due start labels blocked_by lease column backend blocked ready` or an extra column); an unknown
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
  max_size: 1024        # optional, KiB; rotate to audit.jsonl.1 past it
```

Without `audit.file` the audit trail (reloads, creates, updates, deletes; see
[usage.md](usage.md#audit-trail)) is kept in memory and ends with the run. With it, every run appends
to the file, so the board shows this session and the previous one. `.jsonl` is one JSON object per
line; `.csv` has the header `time,session,source,op,backend,id,result,details`. Any other extension is
a config error. Lines tkan cannot parse are skipped, a file that cannot be written is reported and
the entries stay in memory. The file is created readable by its owner only (`0600`; entries hold task titles), an existing file keeps
its mode. Without `max_size` it grows for ever, and every read of the trail parses all of it, so set
`max_size` (KiB): when an append finds the file larger, it is first renamed to `FILE.1` (replacing the
previous one), under the same lock the CSV format uses, so several tkan processes stay safe. The board
and `tkan audit` read only the current file, so the trail on disk is at most about twice `max_size`
and the older half is kept in `FILE.1`.

## Actions

An action is a command tkan runs on the selected task: hand the task to Claude Code or another
AI agent, send a notification, call a script. Press `x` in the board (or the card details) to pick
one, or run `tkan run ACTION ID...`. `.` repeats the last action on the selected card; marking cards with `space`
and pressing `x` runs the chosen action on each marked card in the background ([usage.md](usage.md#keys)).

```yaml
actions:
  - name: claude                  # required; letters, digits, _ . -  (used as `tkan run claude 12`)
    label: Work on it with Claude # optional; shown in the picker (default: name)
    key: c                        # optional one-character shortcut in the picker
    mode: interactive             # background (default) | interactive
    command: [claude, "Work on task {id}: {title}\n\n{description}"]
  - name: notify
    command: [notify-send, "tkan #{id}", "{title}"]
    timeout: 5s
  - name: review
    stdin: markdown               # the task as a Markdown document on stdin
    command: [claude, -p, "Review this task and suggest a plan"]
    env: {CLAUDE_CODE_MAX_OUTPUT_TOKENS: "8000"}
```

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Unique (case-insensitive). |
| `label` | name | Text in the picker. |
| `key` | none | One character, unique; `i`, `j`, `k`, `q` and `x` are reserved by the picker (`i` runs with the terminal, `j`/`k` move, `q`/`x`/`esc` close it); other keys, letters or digits, are free, and the match ignores case. |
| `command` | required | Program and arguments as a **list**. No shell is involved: `$(...)`, quotes or `;` in a task's text are plain data. To use shell features write `[sh, -c, '...', sh, "{title}"]` and pass task values as arguments, never inside the script text. `command[0]` takes no placeholders or braces. Only the names listed below are placeholders: other braces stay literal (`awk '{print}'`, `${HOME}`, `jq '.a|{x}'`), except a lower-case word close to a placeholder name (`{titel}`), which is rejected as a typo. Write `{{` and `}}` for a literal `{` and `}` (`{{id}}` gives the text `{id}`; `}}` in a script, as in `jq '{a:{b:1}}'`, must be written `}}}}`). |
| `mode` | `background` | `background`: runs without the terminal, shows `OK`/`FAIL` in the status line (and the first line it printed, on your screen only). `interactive`: the board is suspended and the command gets the terminal (an agent session, an editor); the board returns, reloaded, when it exits. In the picker `i` followed by the action's key runs a background action with the terminal; `tkan run -i` does the same. |
| `cwd` | the config's directory | Working directory; relative paths are relative to the config file. |
| `timeout` | `10s` | Background runs are ended (with their child processes) after this; see `kill_grace`. A command that looks like an AI agent (`claude`, `codex`, `aider`, `gemini`, ...) without an explicit `timeout:` gets a warning on stderr when the config is loaded by a command that opens it (not for `mode: interactive`). |
| `interactive_timeout` | none | Ends an interactive run (`mode: interactive`, or `-i`) after this, e.g. `2h`; none means no limit. The command (not a process group: it must keep the terminal) gets SIGTERM, then SIGKILL after `kill_grace`. |
| `kill_grace` | `5s` | When a run is ended (timeout, `c` in the runs view, quitting, Ctrl-C in `tkan run`) the command and its child processes get SIGTERM first and SIGKILL once this time has passed, so an agent can clean up or release a claim; a command that exits within the grace is not waited for. `0s` kills at once. A run ended this way is still reported as timed out or canceled, even if the command exits cleanly. Not applied on Windows (the process tree is always killed at once; compile-checked only). |
| `max_output` | `256` | KiB kept of each of stdout and stderr of a background run (the start of it; the rest is dropped and the run view says so). `tkan run` streams the full stdout regardless. |
| `claim` | `false` | `true`: claim the task for the run, like `tkan claim` (as `tkan run -as`, else `$TKAN_AGENT`, else the action's name; the board claims as `$TKAN_AGENT`, else the action's name), with a lease of `timeout` + `kill_grace` (an interactive run: `interactive_timeout` + `kill_grace`, none without it), and release it when the run ends, failed or not. A task another agent holds starts nothing (`tkan run` exits 4; on the board an error, and a batch skips the card as busy). Needs an `assignee` and a `lease` column. Not for hooks. |
| `capture` | none | `notes`: after a background run, append `[run RUN_ID ACTION ok\|error exit N]` and the first `capture_max` KiB of stdout (cut with `[output cut]`) to the task's `notes`. Not for interactive actions or hooks. Applies to `tkan run` and the board. |
| `capture_max` | `4` | KiB of stdout `capture: notes` keeps. |
| `on_success`, `on_failure` | none | Column name or raw status the task moves to when a run ends well / badly (a failed or timed-out command is a failure; a canceled run changes nothing). The task is moved only if its status is still what it was when the run started, so an agent that ran `tkan report -status` keeps its answer. Applies after a `claim: true` release, to `tkan run` and the board; unknown names print a warning. Not for hooks. Each also takes a map `{status, append_notes}` (`on_fail` is an alias of `on_failure`): `append_notes` writes `[run RUN_ID ACTION result exit N]` and a source to the task's `notes`, cut at `capture_max` KiB: `stdout` or `summary` (exit status and sizes) after a success; `stdout`, `summary` or `error` (the error, else the start of stderr) after a failure. Example: `on_failure: {status: Blocked, append_notes: error}`. Not for interactive actions. |
| `exclusive` | `false` | `true`: a background run of this action on a card that already has one running (same action, backend and ID) fails at once with `already running`. It covers the runs of one tkan process (the board, or one `tkan run`/hook); two processes, or an interactive run, are not checked. |
| `stdin` | `none` | `json` (the task as one JSON object, like `show -json` without `column`) or `markdown` (title, fields, description and notes). Hooks also accept `event` and `event-markdown` ([Event on stdin](#event-on-stdin)). Not allowed with `interactive`. |
| `show` | none | `output`: when a background run ends, open its output view (unless you are typing in a prompt). Not allowed with `interactive`. |
| `notify` | none | `bell`: ring the terminal bell when a background run ends; `desktop`: ask the terminal for a desktop notification (`OSC 777` and `OSC 9`: VTE, foot, rxvt, iTerm2, Windows Terminal, kitty, ...; others ignore it; inside tmux it needs passthrough). A batch on marked cards notifies once, at the end. Not allowed with `interactive` or in a hook. Whatever this says, every screen shows `N finished (R)` until the runs view is opened. |
| `env` | none | Extra environment variables. |

**Several cards at once.** The top-level `action_max_parallel` (default `2`, `1` to `16`; `0` or unset means the
default, other values are rejected when the config loads) is how many runs of an action started on the board's marked
cards go at once; the rest wait and start as runs end. It does not limit actions started on a single card, hooks, or
`tkan run ID...`, which runs its tasks one after another.

```yaml
action_max_parallel: 3
```

**Not implemented: permission presets.** A list of vetted allowed-tools per agent (for example a
`claude-readonly` preset) is not provided: it would hard-code third-party flags that change between versions.
Write the agent's own flags in `command`.

**Background runs.** Starting a background action returns to the board at once. A strip above the
status line lists the runs (at most 3 lines): `⟳ running name #3 title  1m12s` while a command
runs, then `OK` or `FAIL` with the total time for 8 seconds after it ends. `R` opens the list of
the runs of this session (up to 50); `enter` shows a run's output (stdout, then a `--- stderr ---`
section), live while it is still running and following the end until you scroll up (`G` follows
again); `c` kills a running command and its child processes. Quitting tkan cancels the runs still
going. Each stream keeps the first 256 KiB (`[output truncated]` after that).

**Placeholders** in `command` arguments (single pass: a title containing `{id}` stays as typed;
unknown `{names}` are a config error): `{id}` `{title}` `{description}` `{status}` `{priority}`
`{notes}` `{type}` `{assignee}` `{due}` `{start}` `{labels}` `{blocked_by}` `{lease}` `{backend}` `{json}` `{slug}` (the title as a short
lower-case dashed name, safe for a directory or branch).

`cwd` and the values of `env` take placeholders too. In `cwd` only `{id}`, `{backend}` and `{slug}` are allowed, and
they become one path element (`/`, `\` and a leading `.` are replaced), so a task cannot point a run outside its
directory. The directory must exist when the run starts; to give each task its own checkout, create a git worktree
in the command and work there:

```yaml
command: [sh, -c, 'git worktree add -B "tkan/$1-$2" "../wt/$1" && cd "../wt/$1" && exec claude "$3"', sh, "{id}", "{slug}", "{title}"]
```

**Environment** always set: `TKAN_TASK_ID`, `TKAN_TASK_TITLE`, `TKAN_TASK_STATUS`, `TKAN_BACKEND`,
`TKAN_WORKDIR` (the resolved working directory), `TKAN_TASK_JSON`, plus the `env` above. Use these instead of placeholders when a script reads them.

**Size limits.** The system refuses very large arguments and variables (`argument list too long`), so a card with long
notes must not stop actions that never read them: a variable over 64 KiB (usually `TKAN_TASK_JSON`) is left out and
`NAME_OMITTED=...` says so; read the task from `stdin: json` or `markdown` instead, which has no limit. An argument
that is over about 120 KiB once the placeholders are filled in (or 1 MiB for all of them) is not started: the run
fails at once, naming the placeholder (`{notes} is 200 KiB`). NUL bytes in task text are dropped.

Every run is recorded in the audit trail (`op=run`, details `action NAME`). The board reloads after
an action, so changes an agent makes through `tkan set-status` or `tkan edit` show up at once.

**Security.** Actions come only from your config file, and tkan never builds a shell line from task
text. An AI agent that receives the task as a prompt may still *act* on it, so a task written by
someone else is a prompt-injection path. For unattended use prefer `claude -p` with a restricted
`--allowedTools` list, and keep `interactive` for tasks you have read.

`actions:`, `hooks:` and the `command:` of a backend run programs with your environment, so a `tkan.yaml` from someone
else is as trusted as a script: read it before you use it. `tkan init` creates the file with mode `0600`, since `env:` may hold secrets.

**Placeholders and the shell.** A placeholder is replaced inside its own argument, verbatim and unquoted, and the
argument goes to the program as is (no shell splits or expands it). So `[notify-send, "{title}"]` is safe whatever
the title holds. A shell is different only because *you* hand it a script: in `[sh, -c, 'echo {title} >> log']` the
title is pasted into the script text before the shell reads it, so a title such as `$(rm -rf ~)` or `x; curl ...`
is run as shell code, and adding quotes around `{title}` in the script does not help (the title can contain the
closing quote). Keep task values out of the script and pass them as positional parameters, which the shell never
parses: `[sh, -c, 'echo "$1" >> log', sh, "{title}"]` (the `sh` after the script is `$0`; the values are `$1`,
`$2`, ...), or read the environment (`"$TKAN_TASK_TITLE"`, `"$TKAN_EVENT_ERROR"`) or stdin. Quote the expansions
in the script (`"$1"`, not `$1`). tkan warns on stderr about a placeholder inside the script of `sh|bash|zsh|dash|ash|ksh -c`
(`tkan run` for actions; any command that loads the config, for hooks).

**Text that looks like an option.** Task text sits in its own argument, but the program still parses
it: a title such as `--add-dir /` in `[claude, "{title}"]` is read as a flag. Put `--` before the
text when the program supports it (`[tool, --, "{title}"]`), or send untrusted text on stdin
(`stdin: markdown`) instead of in the arguments. An option that takes a value (`-p "{title}"`) is
not protected by this: use stdin there too. `tkan run` warns on stderr about an argument that
starts with `{title}`, `{description}`, `{notes}`, `{labels}` or `{assignee}` right after a plain
argument and before any `--`.

### Per-backend actions and hooks

A `backend` entry can carry its own `actions:` and `hooks:` lists, with the same fields as the
top-level ones. They apply to that backend's tasks only, on top of the top-level lists:

```yaml
backend:
  - type: custom
    name: gh
    options: {command: [./gh-issues.sh]}
    actions:
      - name: open                   # replaces the top-level `open` for gh's tasks
        key: o
        command: [gh, issue, view, --web, "{id}"]
      - name: pr                     # only on gh's tasks
        command: [gh, pr, create, --fill, --title, "{title}"]
    hooks:
      - name: comment
        on: [task.moved]
        command: [gh, issue, comment, "{id}", --body, "moved to {status}"]
  - type: json
    name: notes
    options: {source: notes.json}
```

* **Replacing.** An action or hook of a backend with the name of a top-level one takes its place for
  that backend's tasks (here `open` runs `gh` on gh's cards and `xdg-open` on the others); other
  backends keep the top-level one. Names are unique within each list; two backends may use the
  same name.
* **Picker and `tkan run`.** `x` lists the top-level actions (replaced where the card's backend has
  its own) followed by the backend's others. With marked cards of several backends it lists only
  the actions every one of them has, and each card runs its own backend's version. `tkan run NAME
  ID...` does the same per task; a task whose backend has no such action makes it start nothing
  (exit 2). `key:` must not clash within what one backend's cards see (checked at load).
* **Events.** A backend's hook fires only on events whose task belongs to it, so `reload.ok` and
  `reload.fail` (the whole board, no task) are rejected in a backend's `hooks:`. `hook_max_depth`,
  the chain (`TKAN_HOOK_CHAIN`) and every other limit work as for top-level hooks.
* **Warnings** name the list: `tkan: warning: backend gh hooks[0] comment: ...`.

## Hooks

`hooks:` runs a command when something happens, with no key press: a task is created, updated,
deleted or moved to another status, tasks appear or disappear after a reload, a reload fails, an
action ends. A hook is an [action](#actions) (same `command`, placeholders, `env`, `stdin`,
`timeout`, `cwd`) plus the events it listens to and an optional filter; to send the event to a URL use `curl` ([Posting to a URL](#posting-to-a-url)):

```yaml
hooks:
  - name: notify-high
    on: [task.create.success, task.moved]
    when: 'priority=high'            # filter language of docs/filter.md, on the task of the event
    command: [notify-send, "{event}: #{id}", "{title} ({old_status} -> {status})"]
  - name: log-failures
    on: [action.failed, reload.fail]
    command: [sh, -c, 'echo "$TKAN_EVENT $TKAN_ACTION $TKAN_EVENT_ERROR" >> events.log']
  - name: chat
    on: [task.create.success]
    stdin: event
    env: {URL: 'https://hooks.example.com/services/T000/B000/XXXX'}
    command: [sh, -c, 'curl -fsS -X POST -H "Content-Type: application/json" --data @- "$URL"']
    retries: 3                       # flaky target: up to 3 more attempts
    retry_delay: 5s
```

| Field | Meaning |
|---|---|
| `name` | required, unique; letters, digits, `_ . -`. Shown in the audit trail |
| `on` | required; list of event names (table below). A parent name (`task.update`) listens to its `.success` and `.fail` variants |
| `when` | optional filter on the event's task. Not allowed with `reload.*` events (they carry no task). Besides the built-in fields it can use the extra columns or keys of your tasks (`customer:acme`, `story_points>=5`); a misspelt built-in field (`priorty=high`) is an error, any other unknown name just matches no task |
| `max_depth` | optional, 1 to 3: lowers the top-level `hook_max_depth` for this hook (see "Behaviour and limits") |
| `max_parallel` | optional, 1 or more: how many runs of this hook may be in flight at once, whatever the card. Default `1` for a hook that listens only to `reload.*` events, unlimited otherwise (so existing configs behave as before). Not the same as `exclusive` or `unique: task` (one run per card). The top-level `hook_max_parallel` caps all hooks together |
| `unique` | optional, `task`: an event is skipped (audited as `skipped (already running)`, never queued) while a run of this hook for the same task (backend and ID) is still going, e.g. `on: task.moved when: status=doing` fired twice for one card. Counts the runs of this process only, like `exclusive` |
| `min_interval` | optional duration (`30s`): the hook does not start again within this time of its last start. Default none |
| `queue` | what happens to an event that exceeds `max_parallel` / `min_interval` / `hook_max_parallel`: `drop` (default) skips it; `latest` keeps only the newest skipped event and starts it when a slot or the interval frees; `all` queues every one (at most 100, further ones are dropped and counted) and starts them in order |
| `retries` | optional, 0 to 10 (default 0): how many times a failed run (non-zero exit, timeout, `curl -f` on a non-2xx answer) is started again, with the same event and values. A run canceled by Ctrl-C or quitting is not retried |
| `retry_delay` | optional duration (default `1s`, needs `retries`): the wait before each retry. The hook keeps its `max_parallel` slot while it waits, and a one-shot `tkan` waits for the retries too (Ctrl-C stops them) |
| `dedupe` | optional `true`, only with `task.appeared` / `task.disappeared` (other events fire only in the process that wrote; anything else is rejected at load): when several boards notice the same outside change, only the first one runs the hook. It creates a marker with `O_EXCL` in `.tkan-dedupe/` next to the config file, named after a hash of hook, event, backend, ID and fingerprint (title and `created`); a board that finds it already there skips the event (audit: `NAME: skipped (duplicate)`). Markers older than 10 minutes are removed, so the same task can fire again later. Best effort: if the directory cannot be written, the hook runs. Add `.tkan-dedupe/` to `.gitignore` |
| `command`, `cwd`, `timeout`, `kill_grace`, `max_output`, `exclusive`, `stdin`, `env` | as for actions. Always runs in the background; `mode: interactive`, `interactive_timeout`, `key`, `label`, `show`, `claim`, `capture`, `on_success` and `on_failure` are not accepted. |
| `stdin` (extra choices) | besides `none`, `json` and `markdown` (the task of the event), a hook accepts `event` and `event-markdown`, which also tell the command what happened (below). Actions reject them |

### Event on stdin

`stdin: event` sends one JSON line (no size limit): `{"event","source","error","action","old","task"}`.
`event` and `source` (`tui` / `cli` / `poll`) are always there; `error` and `action` are left out when empty
(`error` on `*.fail` and `reload.fail`, `action` on `action.*` events); `old` is the task before the change and is
left out when the event has none (a creation, a reload); `task` is the task the `when:` filter saw (the deleted task
for deletes; an empty task for `reload.*`, which carry none).

`stdin: event-markdown` is the same for a prompt: a list with `event`, `source`, then `action`, `error` and
`old status` when they apply, a blank line, and the task as `stdin: markdown` renders it:

```
- event: task.moved
- source: cli
- old status: Todo

# Fix login

- id: 7
- status: Doing
```

### Posting to a URL

tkan has no built-in HTTP client (it keeps the binary small). To send an event to a chat tool or automation
service, run `curl` with `stdin: event`: the body is the JSON line described above, and task text only travels
inside it, never in the command line. `-f` makes a non-2xx answer fail the run, so `retries` applies.

```yaml
hooks:
  - name: chat
    on: [task.create.success]
    stdin: event
    env: {URL: 'https://hooks.example.com/services/T000/B000/XXXX'}   # shown as *** in previews
    command: [sh, -c, 'curl -fsS -X POST -H "Content-Type: application/json" --data @- "$URL"']
```

The URL is often the secret: keep it in `env:` (previews mask the values of `env:`) and keep a config that
holds it private. Add `-H "Authorization: Bearer $TOKEN"` with a second `env:` entry for a service that needs a header.

### Events

| Event | Fires when | `when:` sees |
|---|---|---|
| `task.create` | a task was created (parent of the next two) | |
| `task.create.success` | creation succeeded (board `n`, `tkan add`, undo of a delete) | the new task |
| `task.create.fail` | creation failed; `{error}` has the reason | the requested task |
| `task.update` | a task was written back (parent of the next two) | |
| `task.update.success` | an update succeeded (edit, move, `tkan edit`, `set-status`, undo) | the new task; `{old_status}` is the previous status |
| `task.update.fail` | an update failed; `{error}` has the reason | the requested task |
| `task.delete` | a task was deleted (parent of the next two) | |
| `task.delete.success` | deletion succeeded | the task as it was |
| `task.delete.fail` | deletion failed | often the task id only (a `when:` on other fields warns at load) |
| `task.moved` | an update changed the status (`{old_status}` to `{status}`) | the new task |
| `task.appeared` | a reload found a task that was not there before and was **not** written by this tkan (TUI only) | the new task |
| `task.disappeared` | a reload no longer finds a task, removed by someone else (TUI only) | the task as it was |
| `task.entered.COLUMN` | an update (or an outside change seen on a reload) moved a task into the board column `COLUMN` (name matched ignoring case; two statuses of one column do not count) | the new task; `{column}`, `{old_column}`, `{new_status}` |
| `task.left.COLUMN` | the same move, for the column the task left | the new task |
| `task.due` | a task not in a finished column (Done, Closed, ...) has a `due` of today or earlier; once per task per day while a board is open (TUI only; not remembered across runs) | the task |
| `column.wip_exceeded` | a column went over its `limit` (TUI only; again only after it dropped back); `{column}` is its name | no task |
| `reload.ok` | the board reloaded (every refresh; TUI only) | no task |
| `reload.fail` | the board could not reload; fires once per distinct error, not every second (TUI only) | no task |
| `action.started` | an action is about to start on a task (`{action}` is its name) | the task |
| `action.finished` | an action ended, either way (parent of the next two) | the task |
| `action.succeeded` | an action ended without error | the task |
| `action.failed` | an action ended with an error; `{error}` has it | the task |

`tkan help events` prints this list. To try a hook without the real event, `tkan hooks test NAME ID [-event E]
[-old-status S] -n` prints what it would run (values of its `env:` masked); without `-n` it runs it once, audited.

`task.*` write events fire for every write that goes through tkan, from the board and from the
subcommands. `task.appeared` / `task.disappeared` compare consecutive lists per backend and skip the
first load, backends that failed to load (partial errors) and tasks this process wrote itself, as long as the
list still shows what it wrote (an own update followed by an outside delete still fires `task.disappeared`). An ID
present in both lists whose title (or `created` field, when the store has one) changed is taken for a new task with a
reused ID: `task.disappeared` fires for the old one and `task.appeared` for the new one. An outside title edit looks
the same.

### What the command gets

* The task placeholders of actions (`{id}`, `{title}`, `{status}`, ...) for the task of the event, plus
  `{event}`, `{source}` (`tui`, `cli`, `poll`, `undo` for the board's undo, or `test` for `tkan hooks test`), `{error}`, `{old_status}`, `{old_json}` (the task before the
  change, JSON), `{action}`, `{column}` (the board column of the task's status, or the column of
  `column.wip_exceeded`), `{old_column}` (the column of `{old_status}`) and `{new_status}` (the task's status after the change). Unknown or missing values are empty.
* Environment: the `TKAN_TASK_*` and `TKAN_BACKEND` variables of actions, and `TKAN_EVENT`,
  `TKAN_EVENT_SOURCE`, `TKAN_EVENT_ERROR`, `TKAN_OLD_STATUS`, `TKAN_OLD_TASK_JSON`, `TKAN_ACTION`, `TKAN_COLUMN`, `TKAN_OLD_COLUMN`, `TKAN_NEW_STATUS`,
  `TKAN_HOOK_DEPTH` and `TKAN_HOOK_CHAIN`.

### Recipes: AI agents

A hook can hand a task to a coding agent when it reaches a state you choose. The examples below load as written
(`tkan config` checks them); adjust the agent's flags to the version you have installed.

```yaml
hooks:
  # Work a card when it is moved to Doing and assigned to "claude". The task goes in as the prompt
  # (stdin: markdown has no size limit); the agent reports back with tkan.
  - name: claude-works-doing
    on: [task.moved]
    when: 'status=doing assignee=claude'
    stdin: event-markdown             # the event (with the old status) and then the task
    timeout: 30m                      # the default is 10s: agents need much more
    cwd: ..                           # the repository, relative to this file
    command: [claude, -p, "Work on the task on stdin. When done run: tkan set-status {id} Review",
              --allowedTools, "Bash(tkan:*),Read,Edit", --max-turns, "30"]

  # An agent for every card someone labels "agent", also when it arrives from outside (board only).
  - name: claude-triage-labelled
    on: [task.appeared]
    when: 'labels:agent'
    stdin: markdown
    timeout: 10m
    command: [claude, -p, "Triage the task on stdin and add your findings with: tkan edit {id} -notes ...",
              --allowedTools, "Bash(tkan:*),Read", --max-turns, "10"]

  # When an action fails, put the card back and say why (the child tkan fires no hooks).
  - name: back-to-todo-on-failure
    on: [action.failed]
    command: [sh, -c, 'tkan edit "$1" -status Todo -add-label failed -notes "$2"', sh, "{id}", "{error}"]

  # Other agents: the same shape, different command. Check the flags of your version.
  # - {name: codex, on: [task.moved], when: 'status=doing assignee=codex', stdin: markdown, timeout: 30m, command: [codex, exec, -]}
  # - {name: aider, on: [task.moved], when: 'status=doing assignee=aider', timeout: 30m, command: [aider, --yes, --message, "{title}: {description}"]}
```

Notes on the recipes:

* **Prompts.** Put the task on stdin (`stdin: markdown` or `json`, or `event` / `event-markdown` in a hook to include what happened), not in `{description}` or `{notes}` in the
  command: arguments are limited to about 120 KiB and show up in the process list.
* **Loops.** By default a `tkan` run by a hook fires no hooks (`hook_max_depth: 1`), so an agent moving
  its card to `Review` cannot start itself again; it also cannot start another agent by doing so. For a pipeline
  (the agent's move to `Review` fires the reviewer) set `hook_max_depth: 2`; a hook already in the chain
  (`TKAN_HOOK_CHAIN`) never fires again.
* **Narrow `when:`.** The hook fires on every matching event: `task.moved` into Doing fires again if the card
  is moved out and back in. Nothing limits how many run at once or runs the same card twice, so make the
  condition specific (an `assignee`, a label) and have the agent change the card (status, assignee) when it starts.
* **Where it runs.** The command runs in the board or the CLI process that saw the event. With two boards open
  both fire for `task.appeared` unless the hook sets `dedupe: true`; `task.moved` and the other write events
  fire only in the process that wrote.

### Running AI agents safely

* **Task text is untrusted input.** Titles, descriptions and notes can be written by anyone who can write to
  the backlog (a shared backend, an issue import, another agent), and `task.appeared` fires for them. A task can
  tell the agent to do something else. Give the agent only the tools it needs (`--allowedTools`, a read-only
  checkout, a throwaway worktree); never `--dangerously-skip-permissions` in a hook.
* **Bound the run:** set `timeout:` (agents die at the 10 s default; tkan warns on stderr about an agent-looking command without one) and the agent's own turn or cost limit.
  A timeout sends SIGTERM to the command and its children and SIGKILL after `kill_grace` (default 5 s), so the
  agent can release its claim; trap SIGTERM in a wrapper if it needs to.
* **Secrets:** `env:` values and `TKAN_*` variables go to the command, not to the audit trail or the action
  log (it records the command line and stdin, which hold the task text, so keep that file private).
* **Audit it:** every run is in `tkan audit -q op:hook` and, with `action_log`, in `tkan runs`
  (command, stdin, output).

### Behaviour and limits

* Only **after**-events exist. A hook cannot veto or delay what it reacts to (a backend lock is held
  while it is written).
* A hook **never fails** the write, reload or action it follows. Its start, exit status, duration and first
  output line are recorded in the audit trail (`op=hook:EVENT`, e.g. `hook:task.moved`, details `NAME: ...`; `tkan audit -q 'op:hook'` lists them, `result=error` when
  it failed) and, with `action_log`, in full (`source=hook`, with the `event`: `tkan runs -q 'source=hook event:task.moved'`; `R` lists them as `NAME [event]`).
* **Rate limits.** `reload.ok` fires on every refresh (also after your own writes), so a slow `reload.*` hook
  would pile up processes: it runs one at a time by default and later events are dropped. Tune with `max_parallel`,
  `min_interval` and `queue` (table above); the state is per hook and lives in the process (the board, or one
  command). Queued events count as pending work: quitting (or a one-shot `tkan`) waits for them, including for a
  `min_interval` to elapse, and Ctrl-C discards them. A dropped event is recorded in the audit trail as an `ok`
  entry `NAME: skipped (running|too soon|queue full|superseded)`, except for `reload.*` events, which would flood
  it. There is no debounce (waiting for a quiet period before firing); `min_interval` with `queue: latest` is the closest.
* **Retries.** With `retries: N` a failed run starts again after `retry_delay`, at most N times. Each attempt is a
  separate run in the action log; the audit trail gets one entry for the last attempt, with `(attempt K of N+1)`
  when it was retried. The hook fires once per event either way; a retry is not a new event.
* Commands start asynchronously; a one-shot `tkan` command, and the board when you quit, wait for them
  (each bounded by its `timeout`, default 10 s) before they exit, saying `waiting for N hook run(s)` on
  stderr. Ctrl-C (SIGINT, SIGTERM, SIGHUP) while waiting kills them and their child processes; they are
  recorded as errors like any other run. A command interrupted before it finished does not wait at all.
* Concurrency: the top-level `hook_max_parallel` (default `0` = unlimited, maximum `64`) caps the hook runs in flight over all hooks of this process, on top of each hook's `max_parallel`; an event over it is dropped, or queued as the hook's `queue` says. `unique: task` skips an event while the hook already runs for that task. Every skipped event is an audit entry with `result=skipped` (`tkan audit -q result:skipped`).
* Bounded recursion: commands started by a hook carry `TKAN_HOOK_DEPTH=N+1` (N = the depth of the process
  that fired it, 0 for yours) and `TKAN_HOOK_CHAIN=a>b` (the names of the hooks that led there). A `tkan`
  with that variable set (its audit entries get `source=hook`) fires a hook only while its depth is below
  the top-level `hook_max_depth` (default `1`: it fires nothing; `2` lets one level of follow-up hooks
  fire; the maximum is `3`, larger values are rejected when the config loads). A hook's own `max_depth:`
  (same range) can lower that for it, never raise it. Whatever the depth, a hook whose name is already in
  the chain does not fire again, so `a` -> `a` loops cannot happen. A hook that runs `tkan set-status` is
  safe with the default.
* Updates look the previous task up (one extra list of that backend) only when a hook listens to
  `task.moved` or `task.update*` and the caller did not already know it; the board and `set-status`/`edit` do.
* Two open boards both notice one outside change and both fire `task.appeared` / `task.disappeared`; set
  `dedupe: true` on the hook to run it once. A delete plus a create with the same ID between two reloads is
  seen only when the title (or `created`) differs; a re-created task with the same title and no `created` field
  looks unchanged.

## Action log

Every action run is summarised in the audit trail (`op=run`: action, duration, exit status, output
sizes; never the environment and never what the command printed, because the audit file is more widely readable than the action log). The details are kept only if you ask for
them, in an optional log that is **off by default**:

```yaml
action_log:
  file: actions.jsonl   # one JSON object per run, appended; relative to this file
  # dir: actions        # or: one JSON file per run, in a sub-directory per action
                        #     actions/<action>/<start UTC>-<backend>-<id>.json
```

Set one of `file` (must end in `.jsonl`) and `dir`, not both. A record holds the start and end time,
the duration, session, source (`tui`/`cli`/`hook`), the `event` that fired it (hook runs; `action` is then the hook's name), action, mode, task (backend, id, title), result, error,
exit status, the **command** as run (with the task's text filled in), the working directory, the
**stdin** given, and the captured **stdout** and **stderr** (first 256 KiB each, `truncated` when
cut). Environment variable values (`env:` and `TKAN_TASK_*`) are never stored. The files are created
private (`0600`, directories `0700`) because prompts and command output can be sensitive; keep them out of
version control (add the path to `.gitignore`). A file that cannot be written is reported on the
status line and the run is not affected. The log is never trimmed: delete or rotate it when you like.

See the records with `R` in the board (earlier sessions are listed under this session's runs;
`enter` shows the command, stdin, stdout and stderr; `/` filters; only the newest 200 stored runs are read, `A` loads the rest) or `tkan runs`
([usage.md](usage.md#action-log)).

## Default backend and colour

`default_backend: NAME` (top level, next to `backend:`) makes one backend the default: `add`
creates there and a bare ID in `show`, `set-status`, `edit` and `delete` is looked up only
there, so it is never ambiguous. `-b NAME` and the `NAME:ID` form still pick another backend.
The environment variable `$TKAN_BACKEND` overrides the key. An unknown name is an error.

The board honours [`NO_COLOR`](https://no-color.org) (and `CLICOLOR=0`): with it set the colours
are dropped. CLI output is never coloured.
