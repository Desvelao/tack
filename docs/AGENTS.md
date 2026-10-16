# Using tkan from an AI agent

Read this if you need to read or update a task backlog (usually `tasks.csv`) with tkan.
If you are changing tkan's own source, read the root `AGENTS.md` instead.

## Rules

1. **Never start the interactive board** (`tkan` with no subcommand): it needs a
   TTY and blocks. Use the subcommands below.
2. **Never hand-edit the task file** with sed/awk/Edit; quoting and multi-line fields break. Use
   `set-status`, `add`, `edit`, `delete`.
3. Prefer `-json` output; parse it instead of scraping the table.
4. Statuses are the board's **column names** (or the raw values they hold). Run
   `tkan statuses -json` to see them; do not assume `pending`/`done`. `tkan config` shows
   which config and file are in effect.
5. Only change the status/rows you were asked to. Do not commit unless the user asks.

## Commands

The tasks come from the `backend` list of the config: `-c CONFIG`, `$TKAN_CONFIG`, or
`./.tkan.yaml` (also `.tkan.yml`, `tkan.yaml`, `tkan.yml`; the first found wins), then `~/.tkan.yaml` or `~/.tkan.yml`. Parent directories are not searched. There is no `-f FILE`. If the project's config is elsewhere, pass `-c` explicitly.

```bash
tkan -c tkan.yaml list -json                    # all tasks, from every backend
tkan -c tkan.yaml list -status Backlog -json    # a column name, or a raw value
tkan -c tkan.yaml list -q 'priority=high col:doing backend=csv1' -json   # field-aware filter
tkan help filter                                   # filter syntax
tkan -c tkan.yaml show 12 -json                 # one task
tkan -c tkan.yaml statuses -json                # columns: name, values, write
tkan -c tkan.yaml set-status 12 Doing           # move: column name or raw value
tkan -c tkan.yaml set-status 12 13 14 Done      # several IDs, then the status; unknown ID = nothing changes
tkan -c tkan.yaml add -title "Fix X" -description "…" -priority high -type fix [-assignee ann] [-due 2026-12-01] [-start D] [-labels a,b] [-status Doing] [-b NAME]
tkan -c tkan.yaml edit 12 -assignee ann -due 2026-12-01   # only the given fields change; -assignee "" clears; dates YYYY-MM-DD; exit 2 on bad input
tkan -c tkan.yaml edit 12 -status Doing -notes "why"      # -status moves the card too; -notes replaces the notes
tkan -c tkan.yaml edit 12 -add-label ops -rm-label wip     # change single labels (case-insensitive)
tkan -c tkan.yaml labels                         # labels in use, with counts: reuse them
tkan -c tkan.yaml delete 12 13                  # several IDs; unknown ID = nothing deleted
tkan -c tkan.yaml delete -n 12                  # dry run: prints what would be deleted
tkan -c tkan.yaml count -by priority -json        # totals per column|priority|type|assignee|label
tkan -c tkan.yaml list -ids                     # BACKEND:ID per line; -tsv: tab-separated, escaped
tkan help list                                     # a command's synopsis and flags (also: tkan version)
tkan -c tkan.yaml config                        # effective config (YAML)
tkan -c tkan.yaml audit -q "op=delete date=today" -json   # what was reloaded/changed (needs audit.file to see past runs)
```

With several backends, IDs are only unique per backend (unless the config sets `default_backend` or `$TKAN_BACKEND` is set: then bare IDs and `add` use that backend, and `-b`/`NAME:ID` still override): `show`, `set-status`, `edit` and `delete` take
`-b NAME` or `NAME:ID` (for example `set-status csv1:12 Doing`) and exit with code 2, listing the
candidates, if a bare ID is ambiguous. Read the `backend` field of `list -json` and keep it with
the ID. If a backend is unavailable (its tool is down), a bare ID is refused (exit 1: `cannot resolve ... use -b NAME or NAME:ID`) because the ID might also exist there; `NAME:ID` / `-b NAME` still work for the healthy ones.

`add -json` prints the created task instead (like `show -json`; no need to branch on the backend count), `edit -json` and `set-status -json` echo the result, and long text goes through stdin (`-notes -`) or `-notes-file F`. `add` prints the new ID (`NAME:ID` when several backends are configured) on stdout, creates in the first backend unless `-b`, and puts the task in the first column unless `-status` is given.
`set-status` and `-status` accept a column name (case-insensitive) or one of its raw values; anything
else exits with code 2 and lists the valid column names. A column may have a WIP `limit`; moving into a full column still succeeds but prints `warning: column ... is over its WIP limit` on stderr (exit code 0).
`set-status` leaves a task that already is in
the target column untouched and prints `BACKEND:ID already COLUMN`. Combine selection and move:
`tkan set-status $(tkan list -q 'col:doing' -ids) Done`.

Task JSON (`status` is the raw stored value, `column` the board column it appears in):

```json
{"backend":"csv1","id":"12","title":"…","description":"…","status":"in_progress","priority":"high","type":"fix","extra":{"owner":"ann"},"column":"Doing"}
```

(empty fields are omitted; `extra` holds columns tkan does not map. `column` is `""` when the
value is hidden by `unmapped: hide`.)

`board.sort` and the board's `s`/`S` keys only reorder the interactive board; `list -json` is always
in store order (backends in config order).

## Filtering

`list -q QUERY` takes the board's filter language: plain words, `field:value` (contains),
`field=value` (exact), `field!=value`, `field!:value`, `field^value` (prefix), `field~regex`,
`-word` (exclude), `a,b` (any of), `a OR b` (either; AND binds tighter), `field=` (empty), relative dates (`due<today+7d`). Fields: `id title description status priority
notes type assignee due start labels column backend` plus any unmapped column by name. An unknown field exits with code 2 and lists
the valid ones. Prefer `-q` over fetching everything and grepping. Details: `docs/filter.md`.

## Creating a configuration

`tkan init -source FILE` writes `./tkan.yaml`, derived from the file's header and status values;
`tkan init -o - -source FILE` prints it instead. Use `-o -` to inspect first and never pass `-force`
unless the user asked you to replace an existing config. Read the notes printed on stderr: they
list fields that could not be detected and must be fixed by hand.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | runtime error (file missing, parse/config error, unsupported extension, I/O) |
| 2 | usage error or unknown status (message lists valid column names on stderr) |
| 3 | task ID not found |

## Typical workflows

Pick the next pending task and claim it:

```bash
# first column's name from `statuses -json`, then its first task
first=$(tkan -c tkan.yaml statuses -json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["name"])')
id=$(tkan -c tkan.yaml list -status "$first" -json | python3 -c 'import json,sys; t=json.load(sys.stdin); print(t[0]["id"] if t else "")')
[ -n "$id" ] && tkan -c tkan.yaml set-status "$id" "<name of the in-progress column>"
```

Finish or fail it, recording a note with `edit` (it replaces the notes, so read them first with
`show -json` if you want to keep them):

```bash
tkan -c tkan.yaml edit "$id" -status "<name of the done column>" -notes "what was done"
```

Report progress:

```bash
tkan -c tkan.yaml count -json     # {"by":"column","total":N,"counts":[{"key":...,"count":...}]}
```

## Concurrency

The file may be open in the board (a human) while you work. Each write re-reads the file first
and rewrites it atomically, so concurrent edits are merged and the board refreshes within about
a second. Concurrent writers are serialised with a lock on `<file>.lock` (a writer waits up to 5 s, then fails with `locked by another tkan`; the Windows lock has not been run on Windows). Even so, run commands sequentially and do not run several writers in parallel.
