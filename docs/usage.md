# tkan usage

tkan shows a task file as a kanban board: one column per status, one card per task.

## Build (Docker only)

No Go toolchain is needed on the host.

```bash
make build         # ./bin/tkan                (linux/amd64, static)
make build-arm     # ./bin/tkan-linux-armv6    (Raspberry Pi Zero W / 1)
make build-armv7   # ./bin/tkan-linux-armv7    (Raspberry Pi 2 / 3 / 4 with a 32-bit OS)
make build-arm64   # ./bin/tkan-linux-arm64    (Raspberry Pi 3 / 4 / 5 with a 64-bit OS)
make build-darwin-amd64   # ./bin/tkan-darwin-amd64
make build-darwin-arm64   # ./bin/tkan-darwin-arm64
make build-windows-amd64  # ./bin/tkan-windows-amd64.exe (see the Windows note below)
make release       # all of the above plus bin/SHA256SUMS
make install       # build, then copy to ~/.local/bin/tkan (make install PREFIX=/usr/local)
make check         # read-only gate: gofmt -l must be empty, then go vet and go test
make test          # go test ./... in a container
make clean         # remove ./bin
make shell         # dev container with the source mounted at /app
```

`make install-backends` copies the backend scripts (`contrib/backends/*/tkan-*.sh`) to
`~/.local/share/tkan/backends` (`PREFIX=/usr/local` for `/usr/local/share/tkan/backends`).

To install a published release instead of building, use `install.sh` (see the README): it
downloads the pinned release for your OS/arch and verifies its checksum, and with `-b sqlite` it installs
the backend scripts too ([Backend scripts](../README.md#backend-scripts)).

`tkan version` (or `tkan -version`) prints the build version. Builds stamp
`VERSION`: on a `vX.Y.Z` tag that is `X.Y.Z`, otherwise `git describe` output, or
`<contents of the VERSION file>-dev` outside a git checkout. Override with
`make build VERSION=1.2.0`. The output also carries the UTC build date,
`tkan 1.2.0 (built 2026-10-16)`; override with `BUILD_DATE=YYYY-MM-DD`. Unstamped builds omit it.

Equivalent without make (BuildKit). The binary is always named `tkan` inside `dest`, so give
each target its own directory. `--platform` builds inside a container of that architecture, which
needs QEMU/binfmt emulation on the host (untested here); `make build-arm` & co. cross-compile
natively instead and are the simpler route:

```bash
docker build --target export --output type=local,dest=bin .
docker build --target export --platform linux/arm/v6 --output type=local,dest=bin/armv6 .
docker build --target export --platform linux/arm64  --output type=local,dest=bin/arm64 .
```

### Windows

The Windows build compiles, but it is the least exercised target: the CSV file lock (`LockFileEx`)
and killing a timed-out `custom` command together with the processes it started (`taskkill /T`)
are implemented but have **never been run on Windows**. Prefer one writer at a time there until
you have tried it.

### Install on a Raspberry Pi

tkan is a single static binary with no runtime dependencies. On the Pi, `uname -m` tells which
build to use:

| `uname -m` | Build |
|---|---|
| `armv6l` (Zero, Zero W, Pi 1) | `tkan-linux-armv6` |
| `armv7l` (Pi 2/3/4/Zero 2 W, 32-bit OS) | `tkan-linux-armv7` |
| `aarch64` (64-bit OS) | `tkan-linux-arm64` |

```bash
scp bin/tkan-linux-armv6 pi@raspberrypi:tkan     # copy under the plain name
ssh pi@raspberrypi 'chmod +x tkan && sudo mv tkan /usr/local/bin/ && tkan version'
```

Check the download against `bin/SHA256SUMS` with `sha256sum -c` if you built with `make release`.

Containers run as your UID/GID (`HOST_UID`/`HOST_GID`); if you call `docker compose` directly,
`export HOST_UID=$(id -u) HOST_GID=$(id -g)` first so files in `bin/` are not root-owned.

## Run

```bash
./bin/tkan                         # open the board of ./tkan.yaml (or ~/.tkan.yaml, see config.md)
./bin/tkan -c path/to/tkan.yaml # or $TKAN_CONFIG=path/to/tkan.yaml
./bin/tkan config                  # show the effective configuration
```

There is no file argument: the data sources are the `backend` list of the config (each entry has
a `type`, an optional `name` and its `options`; see [config.md](config.md)). Several backends are
shown on one board and every card shows the name of the backend it belongs to. Without a config
tkan stops and suggests `tkan init`.

### Keys

| Key | Action |
|---|---|
| `←` `→` / `h` `l` | previous / next column |
| `1`-`9` | jump to column N |
| `↑` `↓` / `k` `j` | previous / next card |
| `g` `G` / `home` `end` | first / last card of the column |
| `pgup` `pgdn` | jump a screen of cards |
| `H` `L` / `shift+←` `shift+→` | move the card to the previous / next status (saved immediately) |
| `enter` | open card details (`esc`/`enter` to close; long cards scroll with `j` `k` `pgup` `pgdn` `g` `G`) |
| `e` | edit the card: title, description, priority, type, assignee, due, start, labels, blocked_by, notes (`tab`/`shift+tab` move, `enter` saves, `esc` cancels, twice if there are unsaved changes; dates are `YYYY-MM-DD`) |
| `n` | new task in the current column (title prompt; stored with that column's `write` value) in the first backend |
| `b` | with several backends: cycle the backend `n` creates in (shown as `@name` in the prompt) |
| `m` | move the card, or all the marked cards, to a column: press `m`, then the column's number `1`-`9` (`esc` cancels). Cards already there are skipped; `u` undoes the whole move in one step |
| `d` | delete the card, or all the marked cards (`space`) at once (asks `y/N`; `u` brings them all back in one step) |
| `/` | filter (`up`/`down` recall earlier filters of this session): plain text or `field:value`, `field=exact`, `field!=value`, `-not`, ... (see [filter.md](filter.md); `esc` clears) |
| `u` | undo the last move, edit or delete (up to 30 steps this session; a deleted card is created again and may get a new ID) |
| `f` then `t` `a` `p` `l` | filter by the selected card's type / assignee / priority / first label (`esc` clears) |
| `v` | cycle the saved views from `board.views` in the config, then back to no filter ([config.md](config.md#saved-views)) |
| `z` | group the cards of every column by priority, type, assignee, labels, then off (`board.group` sets a default) |
| `s` / `S` | sort cards (display only): `s` next field, `S` reverse; the header shows the sort. `board.sort` in the config sets the default |
| `r` | reload now (needed for `custom` backends unless they set `poll`, and for CSV files with `poll: none` or a long `poll`) |
| `x` | run a configured action on the card ([config.md](config.md#actions)): `↑` `↓` / `enter` or the action's `key`; `i` first runs it with the terminal; `esc` cancels. The result shows as `OK` / `FAIL`. With marked cards (`space`) it runs on all of them instead. Starting an action that is already running on the card only warns (`review is already running on #12 (1): start it again (x or .) to run it once more`); the same start again within 10 s runs a second one. An `exclusive` action is refused (`FAIL`) instead |
| `.` | run the last action again on the selected card (also in the card details), with the terminal if `i` gave it the terminal last time |
| `space` | mark / unmark the card (a `✓` before its title, `N marked` in the header). Marks follow the card by backend and ID: they survive reloads, sorting and filters (hidden marked cards stay marked) and are dropped when the card is deleted. `x` on marked cards starts the chosen action on each of them in the background, at most `action_max_parallel` at once (default 2, [config.md](config.md#actions)); the others wait their turn and a card that is gone by then, or that the action is already running on, is skipped. The marks are cleared when it starts, the strip shows the runs and one notice sums up at the end (`OK    ran claude on 3 cards: 3 ok`, or `FAIL` naming the failed, vanished and skipped cards). An `interactive` action (or `i`) cannot run on marked cards; only one such batch runs at a time. `esc` with no filter active unmarks all |
| `R` | action runs ([config.md](config.md#actions)): `↑` `↓` / `pgup` `pgdn` / `home` `end` move through a list that scrolls to fit the screen, `enter` shows a run's command, stdin and output (live while it runs), `/` filters, `c` cancels a running one, `esc` goes back; with `action_log` set, earlier sessions' runs are listed too (only the newest 200 are read, the header says how many older ones were left out and `A` loads them all; a filter searches what is loaded); an open output refreshes four times a second while its run is going; the strip above the status line shows runs in progress and, for 8 s, their result; background runs that ended since `R` was last opened are counted as `N finished (R)` on every screen (the board's title line, the start of the bottom line elsewhere) until `R` opens the list; an action with `notify: bell` or `notify: desktop` also rings or sends a desktop notification |
| `A` | audit trail of reloads and writes ([below](#audit-trail)); `/` filters it, `esc` clears the filter, then goes back |
| `F` | fleet: one line per agent and task it holds (avatar, state `busy` / `expired` / `held` / `idle`, task, when the claim started, elapsed, lease left), built from the claims, so claims made in another terminal show within a second; `↑` `↓` choose, `enter` goes to the card, `r` reloads, `esc` goes back. Names listed in `board.agents` show as `idle` when they hold nothing; `board.avatars` sets the pictures (a busy agent's animate) |
| `?` | help: keys, then (`tab`) the filter syntax, then what each filter field means; in the filter prompt of the audit (`A`) and runs (`R`) views `?` shows the same for their own fields |
| `q` / `ctrl+c` | quit |

**Runs on the cards.** While a background action runs on a card, the card has an extra line with
the action and the time so far (`⟳ review 12s`, `+1` when more run on it); for 8 s after the end it
says `OK review 3s` or `FAIL review 3s`. The card details (`enter`) list every run going on it
(`⟳ running review 12s`) and the ones that just ended. The filter field `running` (`/running=yes`)
shows only the cards with a run going ([filter.md](filter.md#fields)). Cards assigned to one of the
`board.agents` names ([config.md](config.md)) get a coloured border and a bold `@name`, and their
details say `agent`.

**Confirmation of writes.** After a move, edit, new card, delete or undo the bottom line says what the backend
did: a green `OK` (for example `OK    moved #3 to done`, gone after about 4 seconds or at the next key) or a bold red
`FAIL` with the backend's error (`FAIL  locked by another tkan ...`), which stays until the next key. The words
carry the meaning when colours are off (`NO_COLOR`); a long message is cut with `…` to the terminal width. The audit
trail (`A`) keeps the full text.

The board refreshes by itself within ~1 s when a CSV file changes on disk (for example when an
AI agent updates it); the csv option `poll` ([config.md](config.md)) changes that to every N seconds or never. If the terminal is narrower than all columns, columns are paged
horizontally; the focused column is always visible, and a strip under the title lists every
column with its card count and number key (`1 Todo 5 · 2 Doing 2 · ...`, shortened to `N:count` on
very narrow terminals).

## Audit trail

tkan records what it does: every reload (`ok` or `error`, and whether it was manual or because the
file changed) and every create, update and delete (and every action run, `op=run`), from the board and from the subcommands. Idle
polling records nothing, and an error that repeats on every poll is recorded once.
Each entry has a time, an operation (`create`, `update`, `delete`, `reload`), the backend and task
ID, a result (`ok`/`error`) and details (for example `moved to done "Fix login" status=done`). If the audit file cannot be written, the commands warn on stderr (`tkan: warning: audit log: ...`) and the board shows the error; the entries stay in memory for the session.

By default the trail lives in memory and ends with the run. Set `audit.file` to a `.jsonl` or
`.csv` file ([config.md](config.md#audit-trail)) to keep it (and `audit.max_size` to cap its size): the view (`A`) then lists every session
newest first under headings, `this session` and `previous session` first, older ones by start date.
A session is one run of tkan (the board, or one subcommand).

Filter with the usual query language ([filter.md](filter.md)). Fields: `op`, `date` (`YYYY-MM-DD`,
so `date>=2026-10-01`, `date=today`, `date>=today-7d` work), `time`, `details`, `result`, `source`
(`tui`/`cli`/`mcp`/`hook`), `backend`, `id`, `session` (`current`, `previous` or an id). A plain word matches
`details`, `op` or `date`. Examples: `op=delete`, `result=error`, `op:update date=today login`,
`session=previous -reload`.

`tkan audit [-q QUERY] [-limit N] [-json]` prints the same entries oldest first (`-limit N` keeps
the last N). Without `audit.file` it can only show its own run.

## Hooks

With `hooks:` in the config ([config.md](config.md#hooks)) commands start by themselves on events: a task
created, updated, deleted or moved (from the board and from the subcommands), tasks added or removed
by someone else (`task.appeared`, `task.disappeared`; board only), a failed reload, an action that ended. Runs are
audited as `op=hook:EVENT` (for example `hook:task.moved`) and never fail the write they follow. A `tkan` a hook runs fires no hooks unless `hook_max_depth` (default 1,
maximum 3) allows it; a hook never re-fires inside its own chain. A hook can POST the event as JSON to a URL
(`curl` with `stdin: event`, see [Posting to a URL](config.md#posting-to-a-url)), and retry a failed run (`retries:`, `retry_delay:`). A backend entry can have its
own `hooks:` (and `actions:`) that apply to its tasks only ([Per-backend actions and hooks](config.md#per-backend-actions-and-hooks)). Placeholders are never
shell-parsed, except inside a script you give to `sh -c`: pass values to it as arguments
(`[sh, -c, 'echo "$1"', sh, "{title}"]`); see [Placeholders and the shell](config.md#actions).

`tkan hooks` lists the configured hooks (`-json` too). `tkan hooks test NAME ID -event task.moved -old-status todo -n`
fires one by hand on a made-up event: it checks `when:` and the depth limit as a real event would (exit 4 when either
stops it) and prints the command, its variables (values of the hook's `env:` shown as `***`) and the size of its stdin;
without `-n` it runs it once (no limits, no retries), recorded in the audit trail and the action log like a fired one,
with `{source}` = `test`. `tkan help events` lists the events. A `when:` on `task.delete.fail` warns at load: that
event often carries only the task id.

## Action log

With `action_log:` set in the config ([config.md](config.md#action-log)) every action run is stored in
detail: command, stdin, stdout, stderr, start, duration, exit status. `tkan runs` prints them oldest first
(`TIME ACTION EVENT RESULT BACKEND ID TOOK TITLE`); `-q QUERY` filters (fields `action mode result date time
source event backend id title exit`, e.g. `-q 'result=error date=today'` or `-q 'source=hook event:task.moved'` (`EVENT` is the event that fired a hook run, `-` otherwise); plain words match action, title, id and
error), `-limit N` keeps the newest N, and `-json` prints the full records. Without `action_log` the command
exits 1 and says so. The audit trail keeps only a one-line summary per run.

`tkan run ACTION ID... [-json] [-propagate-exit]`: `-json` prints a summary per run (`result`, `exit_code`,
`duration_ms`, `run_id`, `truncated`, `log`, `error`, `status_before`, `status_after`; an object for one ID, a list for several)
and sends the command's stdout to stderr. A failed command makes `run` exit 1; with `-propagate-exit` it exits with the command's own
status (1-125), so `2`, `3` and `4` can mean either tkan's codes or the command's. The command gets `TKAN_RUN_ID` (also stored as
`run_id` in the action log) and, when `action_log` is set, `TKAN_ACTION_LOG`. From inside the run, `tkan report ID [-status S]
[-note TEXT|-] [-run RUN_ID]` moves the task and appends a timestamped note (`-run` defaults to `$TKAN_RUN_ID`); it is audited as
`op=report` and stored in the action log as `source: report`. The actions' `capture: notes`, `on_success` and `on_failure`
([config.md](config.md#actions)) copy the output to the notes and move the task when a run ends.

## Non-interactive commands

```bash
tkan [-c CONFIG] list [-status S] [-q QUERY] [-v VIEW] [-json|-ids|-tsv] [-fields F,..] [-sort [-]F,..] [-limit N]
tkan [-c CONFIG] count [-by column|priority|type|assignee|label] [-status S] [-q QUERY] [-v VIEW] [-json]
tkan [-c CONFIG] labels [-status S] [-q QUERY] [-json]
tkan [-c CONFIG] show ID [-b NAME] [-json]
tkan [-c CONFIG] statuses [-json]
tkan [-c CONFIG] set-status ID... STATUS [-b NAME] [-from STATUS] [-json]
tkan [-c CONFIG] add -title T [-b NAME] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L] [-blocked-by IDS] [-status S] [-json] [-description-file F] [-notes-file F]
tkan [-c CONFIG] edit ID [-b NAME] [-status S] [-add-label L] [-rm-label L] [-append-notes TEXT] [-json] [-description-file F] [-notes-file F] [-title T] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L] [-blocked-by IDS]
tkan [-c CONFIG] delete ID... [-b NAME] [-n] [-json]
tkan [-c CONFIG] claim ID... [-as AGENT] [-lease D] [-status S] [-b NAME] [-json]
tkan [-c CONFIG] release ID... [-as AGENT | -force] [-status S] [-b NAME] [-json]
tkan [-c CONFIG] fleet [-json]
tkan [-c CONFIG] next [-status S] [-q QUERY] [-v VIEW] [-claim [-as AGENT] [-lease D] [-to S]] [-json]
tkan [-c CONFIG] run ACTION ID... [-b NAME] [-i] [-n] [-force] [-as AGENT]   # a background action's stdout appears while it runs; stderr when it ends
tkan [-c CONFIG] runs [-q QUERY] [-limit N] [-json]
tkan [-c CONFIG] audit [-q QUERY] [-limit N] [-json]
tkan [-c CONFIG] config
tkan [-c CONFIG] actions [-json]                        # the configured actions (name, key, mode, backend, label)
tkan [-c CONFIG] schema                                 # JSON for agents: task fields, commands with flags, exit codes, filter fields, actions and their {placeholders}
tkan [-c CONFIG] hooks [-json]                          # the configured hooks
tkan [-c CONFIG] hooks test NAME [ID] [-b NAME] [-event E] [-old-status S] [-n]   # fire a hook by hand (-n: print only)
tkan help [filter|events|COMMAND] # COMMAND: that command's synopsis and flags; events: what a hook can listen to
tkan version
tkan completion bash|zsh|fish     # e.g. source <(tkan completion bash); completes commands, flags, help topics, column names (-status), backend names (-b) and task IDs
tkan init [-backend TYPE] [-source SRC] [-o OUT|-] [-force]   # write a sample tkan.yaml (see below)
```

`run` with several IDs runs the action on each task one after another, in the order given (a task named twice
runs once), printing `tkan: NAME on BACKEND:ID (K of N)` on stderr before each. Every ID is resolved first: an
unknown one starts nothing (exit 3), an ambiguous one exits 2. A run that fails does not stop the others; the exit code
is then 1 and stderr ends with `tkan: NAME failed on F of N tasks: BACKEND:ID ...`. Ctrl-C ends the current run and
starts no more (exit 130). `-n` prints one command per task.

To keep completion across shell sessions, write the script once to the shell's completion directory:

```bash
tkan completion bash > ~/.local/share/bash-completion/completions/tkan
mkdir -p ~/.zfunc && tkan completion zsh > ~/.zfunc/_tkan   # and: fpath+=~/.zfunc before compinit
tkan completion fish > ~/.config/fish/completions/tkan.fish
```

IDs are only unique within a backend. When the same ID exists in several, `show`, `set-status`, `edit` and
`delete` fail with exit code 2 listing them; pass `-b NAME` or write `NAME:ID`. `add` creates in
the first backend unless `-b NAME` is given, and prints `NAME:ID` when more than one backend is
configured. `list` has a `BACKEND` column and a `backend` JSON field; filter with `backend=NAME`.

`-q` takes the same query as the board filter ([filter.md](filter.md)), for example
`tkan list -q 'priority=high col:doing -wip'`.

`set-status` takes several IDs before the status (`set-status 1 2 3 done`); an ID that is already in the target column is left untouched and reported as `already ...`. An unknown ID changes nothing, but writes are not atomic: if one fails part-way (exit 1) the earlier tasks stay moved, `-json` still prints them, and stderr says `stopped after moving N of M tasks` (`delete` likewise reports `stopped after deleting N of M tasks`). `-from STATUS` makes it a compare-and-set: a task not (any more) in that column/status is not touched and the command exits `4` (`edit ID -if FIELD=VALUE ...` does the same for any field, for example `-if status=todo` or `-if assignee=`). The backend checks it under its own lock or transaction, so of two processes running `set-status 7 doing -from todo` exactly one wins; the `custom` backend has no lock and only re-reads just before writing, so it narrows the race but cannot close it. `edit -status` moves a card together with other field changes.

`delete` takes several IDs, prints `deleted BACKEND:ID "title"` for each and, with `-n`, only reports what it would delete. If any ID is unknown nothing is deleted (exit 3).

`count` prints how many tasks are in each group (default `-by column`, in board order including empty columns; other groupings biggest first, empty values as `(none)`; `-by label` counts a task once per label) with a TOTAL, and takes the same `-status`, `-q`, `-v` filters as `list`.

`edit -add-label a,b` and `-rm-label c` change single labels without rewriting the list (repeatable, case-insensitive, order kept; applied after `-labels` when both are given). `labels` is `count -by label`: the labels in use with their counts, so you can reuse them instead of inventing near-duplicates.

`add -json`, `edit -json` and `set-status -json` print the resulting task(s) as JSON (like `show -json`, with `column`; `set-status` adds `changed` per task) instead of the one-line text. Multi-line `-description` / `-notes` come from stdin with the value `-` (`printf 'a\nb' | tkan add -title T -description -`; only one field can read stdin) or from a file with `-description-file F` / `-notes-file F` (also on `edit`); one trailing newline is dropped.

`list -fields id,title,labels` picks the columns of the table and `-tsv` output (any filter field, including extra columns; default `backend,id,status,column,priority,type,assignee,due,title`); `-sort -priority,id` orders the output (leading `-` = descending; numbers numerically, otherwise text order, empty last) and `-limit N` keeps the first N. `-json` and `-ids` always print every field / the IDs.

`list -ids` prints only `BACKEND:ID`, one per line (`tkan set-status $(tkan list -q 'col:doing' -ids) done`). `list -tsv` prints the table columns tab-separated without a header, with tabs and newlines in cells escaped as `\t` `\n` (and `\` as `\\`), so empty cells keep their position.

`run` starts an action from the config's `actions:` list ([config.md](config.md#actions)) on one task. A background action's output goes to stdout (stderr to stderr); `-i` hands it the terminal (always the case for `mode: interactive`); `-n` prints the expanded command (`cd DIR && program args...`) without running it. It exits `0` when the command succeeds, `1` when it fails or times out (its stderr is printed), `2` for an unknown action, `3` for an unknown task, `4` when a task is blocked (one of its `blocked_by` tasks is not in a finished column; nothing starts, also with `-n`): `-force` runs it anyway.

`STATUS` / `-status` accept a **column name** (case-insensitive) or a raw status value. `add`
without `-status` uses the first column. `list`/`show` JSON includes a `column` field; `list -json` also has `blockers`, the ids of `blocked_by` that still block the task (left out when none).

`claim` makes agents take work without stepping on each other: it writes `-as AGENT` (default `$TKAN_AGENT`) as the task's `assignee`, and with `-lease 30m` a `lease` time (RFC 3339 UTC) after which the claim runs out, in one write guarded inside the backend's lock, so of two agents claiming the same task exactly one wins and the other exits `4` (`claimed by X until T`). A task is claimed while it has an assignee, until its lease if it has one; the holder may claim again to renew. `-status S` also moves it. `release` clears assignee and lease (only the holder's own claim unless `-force`; `-status S` moves it too). `next` prints the most urgent task (by `priority`, then store order) that is in an unfinished column, not blocked and not claimed (`-status`/`-q`/`-v` narrow it; exit `3` when there is none); `next -claim` also claims it, trying the next candidate when another agent was faster, and `-to S` moves it. An action with `claim: true` claims the task around each run (see [config.md](config.md)); `run -as` names the agent. `claim -lease` needs a `lease` column (exit 2 otherwise).

Exit codes: `0` ok, `1` error, `2` usage / invalid status, `3` task not found (`next`: no ready, unclaimed task), `4` precondition failed (`set-status -from`, `edit -if`, `claim`/`release` of a task another agent holds, `run` on a blocked task without `-force` or on one another agent holds with `claim: true`).

## MCP server (`tkan mcp`)

`tkan mcp [-read-only]` speaks the [Model Context Protocol](https://modelcontextprotocol.io) over stdin/stdout, so an AI agent gets typed tools instead of building shell command lines (no quoting or escaping, and no broad `Bash(tkan:*)` grant). Register it in the agent's MCP settings, e.g. `{"mcpServers": {"tkan": {"command": "tkan", "args": ["-c", "/path/tkan.yaml", "mcp"]}}}`.

| Tool | Does (same rules and exit codes as the subcommand) |
|---|---|
| `list_tasks` | `list -json` (`status`, `query`, `view`, `sort`, `limit`) |
| `get_task` | `show -json` (`id`, `backend`) |
| `count` | `count -json` (`by`, `status`, `query`, `view`) |
| `statuses` | `statuses -json` |
| `add_task` | `add -json` (`title` + the task fields, `status`, `backend`) |
| `edit_task` | `edit -json` (`id`, fields, `add_labels`, `rm_labels`, `if` guard; `""` clears a field) |
| `set_status` | `set-status -json` (`ids`, `status`, `from` guard) |
| `next_task` | `next -json` (`status`, `query`, `view`): the most urgent ready, unclaimed task, not claimed |
| `claim_next_task` | `next -claim -json` (`agent`, `lease`, `to`, `status`, `query`, `view`) |
| `claim_task` | `claim -json` (`ids`, `agent`, `lease`, `status`, `backend`); exit 4 when another agent holds one |
| `release_task` | `release -json` (`ids`, `agent` or `force`, `status`, `backend`) |
| `schema` | `schema` |
| `run_action` | `run -json` (`action`, `ids`, `dry_run`, `force`, `agent`); only actions from `actions:`, never an interactive one |

A failure is a tool result with `isError` and the exit code's meaning (`2` usage, `3` not found, `4` precondition). `delete` is not offered. `-read-only` leaves only the read tools (`list_tasks`, `get_task`, `count`, `statuses`, `next_task`, `schema`): the write tools are not listed and are refused. Every write and run is audited with `source=mcp`. Values `-` (stdin) are refused for `description`/`notes`. The server reloads the stores before each call, so edits made elsewhere show up; calls run one at a time.

## Generating a configuration

```bash
tkan init -source tasks.csv            # writes ./tkan.yaml
tkan init issues.csv -o - | less       # print instead of writing
tkan init -backend custom -o -         # template for another backend type
tkan init -force -source tasks.csv     # overwrite an existing file
```

The output is a one-entry `backend` list adjusted to the type and, when the CSV exists, derived
from it: header names are matched to the fields (with common synonyms such as `key`→`id`,
`summary`→`title`, `state`→`status`), the board columns are built from the status values found in
the file and ordered as a workflow (backlog → doing → done → failed), and `source` is written
relative to the output file. Anything it could not detect is left as a commented `TODO` and
reported on stderr. `init` never overwrites without `-force` and works even if the existing config
is broken.
See [config.md](config.md) for the reference.

## CSV format

By default the first row is a header; column order does not matter and names are
case-insensitive. Use the `csv:` section of the config to map other names or positions
([config.md](config.md)).

| Field (default column) | Required | Notes |
|---|---|---|
| `id` | yes | unique and not empty: a file with a repeated or empty id is rejected with the line numbers (otherwise one row could not be addressed). New tasks get `max(numeric id, high-water mark)+1` (the mark is the highest id ever given out, kept in `<file>.nextid`, so an id deleted from the end is not reused); ids that are not plain numbers (`T-1`) are ignored for that, so a prefixed scheme gets plain numbers for new tasks |
| `title` | yes | |
| `status` | yes | board columns are `pending`, `in_progress`, `done`, `failed` by default, plus one per other value found (see `board.unmapped`) |
| `blocked_by` (also `depends_on` in `tkan init`) | no | ids of tasks of the same backend to finish first, comma-separated (`2,5`). While one of them is not in a finished column the card shows `blocked by #2` in red, the action picker warns, `tkan run` refuses it (exit 4, `-force`) and the filter fields `blocked`/`ready` say so (see [filter.md](filter.md)); unknown ids do not block |
| `description`, `priority`, `notes`, `type`, `assignee`, `due`, `start`, `labels` | no | free text; on the board priority is coloured (`high`/`critical`/`p0`... red, `medium`/`normal`/`p2` yellow, `low`/`minor`/`p3` green), each type gets a stable colour the first two labels show as `[a,b+1]`, and a due date turns red when overdue and amber within 3 days (not in finished columns such as Done/Closed/Released; filter them with `due<today`) |
| anything else | no | preserved untouched |

Saving rewrites the file atomically (temp file + rename), keeps unknown columns and the original
line endings (LF or CRLF), and merges changes made by other processes since the last read.
Quoting of fields may be normalised by the rewrite.

Next to the CSV a small empty `<file>.lock` appears after the first write: it serialises writers
(Unix only) and is safe to ignore; add `*.lock` and `*.nextid` to your `.gitignore` if the backlog is in a repository.
