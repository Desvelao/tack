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

To install a published release instead of building, use `install.sh` (see the README): it
downloads the pinned release for your OS/arch and verifies its checksum.

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
| `e` | edit the card: title, description, priority, type, assignee, due, start, labels, notes (`tab`/`shift+tab` move, `enter` saves, `esc` cancels, twice if there are unsaved changes; dates are `YYYY-MM-DD`) |
| `n` | new task in the current column (title prompt; stored with that column's `write` value) in the first backend |
| `b` | with several backends: cycle the backend `n` creates in (shown as `@name` in the prompt) |
| `d` | delete the card (asks `y/N`) |
| `/` | filter (`up`/`down` recall earlier filters of this session): plain text or `field:value`, `field=exact`, `field!=value`, `-not`, ... (see [filter.md](filter.md); `esc` clears) |
| `u` | undo the last move, edit or delete (up to 30 steps this session; a deleted card is created again and may get a new ID) |
| `f` then `t` `a` `p` `l` | filter by the selected card's type / assignee / priority / first label (`esc` clears) |
| `v` | cycle the saved views from `board.views` in the config, then back to no filter ([config.md](config.md#saved-views)) |
| `z` | group the cards of every column by priority, type, assignee, labels, then off (`board.group` sets a default) |
| `s` / `S` | sort cards (display only): `s` next field, `S` reverse; the header shows the sort. `board.sort` in the config sets the default |
| `r` | reload (needed for `custom` backends unless they set `poll`) |
| `?` | help: keys, then (`tab`) the filter syntax |
| `q` / `ctrl+c` | quit |

The board refreshes by itself within ~1 s when a CSV file changes on disk (for example when an
AI agent updates it). If the terminal is narrower than all columns, columns are paged
horizontally; the focused column is always visible, and a strip under the title lists every
column with its card count and number key (`1 Todo 5 · 2 Doing 2 · ...`, shortened to `N:count` on
very narrow terminals).

## Non-interactive commands

```bash
tkan [-c CONFIG] list [-status S] [-q QUERY] [-v VIEW] [-json|-ids|-tsv] [-fields F,..] [-sort [-]F,..] [-limit N]
tkan [-c CONFIG] count [-by column|priority|type|assignee|label] [-status S] [-q QUERY] [-v VIEW] [-json]
tkan [-c CONFIG] labels [-status S] [-q QUERY] [-json]
tkan [-c CONFIG] show ID [-b NAME] [-json]
tkan [-c CONFIG] statuses [-json]
tkan [-c CONFIG] set-status ID... STATUS [-b NAME] [-json]
tkan [-c CONFIG] add -title T [-b NAME] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L] [-status S] [-json] [-description-file F] [-notes-file F]
tkan [-c CONFIG] edit ID [-b NAME] [-status S] [-add-label L] [-rm-label L] [-json] [-description-file F] [-notes-file F] [-title T] [-description D] [-priority P] [-type Y] [-notes N] [-assignee A] [-due DATE] [-start DATE] [-labels L]
tkan [-c CONFIG] delete ID... [-b NAME] [-n]
tkan [-c CONFIG] config
tkan help [filter|COMMAND]        # COMMAND: that command's synopsis and flags
tkan version
tkan completion bash|zsh|fish     # e.g. source <(tkan completion bash); completes commands, flags, help topics, column names (-status), backend names (-b) and task IDs
tkan init [-backend TYPE] [-source SRC] [-o OUT|-] [-force]   # write a sample tkan.yaml (see below)
```

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

`set-status` takes several IDs before the status (`set-status 1 2 3 done`); an ID that is already in the target column is left untouched and reported as `already ...`. `edit -status` moves a card together with other field changes.

`delete` takes several IDs, prints `deleted BACKEND:ID "title"` for each and, with `-n`, only reports what it would delete. If any ID is unknown nothing is deleted (exit 3).

`count` prints how many tasks are in each group (default `-by column`, in board order including empty columns; other groupings biggest first, empty values as `(none)`; `-by label` counts a task once per label) with a TOTAL, and takes the same `-status`, `-q`, `-v` filters as `list`.

`edit -add-label a,b` and `-rm-label c` change single labels without rewriting the list (repeatable, case-insensitive, order kept; applied after `-labels` when both are given). `labels` is `count -by label`: the labels in use with their counts, so you can reuse them instead of inventing near-duplicates.

`add -json`, `edit -json` and `set-status -json` print the resulting task(s) as JSON (like `show -json`, with `column`; `set-status` adds `changed` per task) instead of the one-line text. Multi-line `-description` / `-notes` come from stdin with the value `-` (`printf 'a\nb' | tkan add -title T -description -`; only one field can read stdin) or from a file with `-description-file F` / `-notes-file F` (also on `edit`); one trailing newline is dropped.

`list -fields id,title,labels` picks the columns of the table and `-tsv` output (any filter field, including extra columns; default `backend,id,status,column,priority,type,assignee,due,title`); `-sort -priority,id` orders the output (leading `-` = descending; numbers numerically, otherwise text order, empty last) and `-limit N` keeps the first N. `-json` and `-ids` always print every field / the IDs.

`list -ids` prints only `BACKEND:ID`, one per line (`tkan set-status $(tkan list -q 'col:doing' -ids) done`). `list -tsv` prints the table columns tab-separated without a header, with tabs and newlines in cells escaped as `\t` `\n` (and `\` as `\\`), so empty cells keep their position.

`STATUS` / `-status` accept a **column name** (case-insensitive) or a raw status value. `add`
without `-status` uses the first column. `list`/`show` JSON includes a `column` field.

Exit codes: `0` ok, `1` error, `2` usage / invalid status, `3` task not found.

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
| `id` | yes | unique and not empty: a file with a repeated or empty id is rejected with the line numbers (otherwise one row could not be addressed). New tasks get `max(numeric id)+1`; ids that are not plain numbers (`T-1`) are ignored for that, so a prefixed scheme gets plain numbers for new tasks |
| `title` | yes | |
| `status` | yes | board columns are `pending`, `in_progress`, `done`, `failed` by default, plus one per other value found (see `board.unmapped`) |
| `description`, `priority`, `notes`, `type`, `assignee`, `due`, `start`, `labels` | no | free text; on the board priority is coloured (`high`/`critical`/`p0`... red, `medium`/`normal`/`p2` yellow, `low`/`minor`/`p3` green), each type gets a stable colour the first two labels show as `[a,b+1]`, and a due date turns red when overdue and amber within 3 days (not in finished columns such as Done/Closed/Released; filter them with `due<today`) |
| anything else | no | preserved untouched |

Saving rewrites the file atomically (temp file + rename), keeps unknown columns and the original
line endings (LF or CRLF), and merges changes made by other processes since the last read.
Quoting of fields may be normalised by the rewrite.

Next to the CSV a small empty `<file>.lock` appears after the first write: it serialises writers
(Unix only) and is safe to ignore; add `*.lock` to your `.gitignore` if the backlog is in a repository.
