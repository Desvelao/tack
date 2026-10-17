# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow [SemVer](https://semver.org/);
before 1.0 the config format, CSV schema and CLI may still change.

## [Unreleased]

- TUI: writes (move, edit, new, delete, undo) now report `OK` (green, fades after ~4 s) or `FAIL` (bold red, stays until a key) with the backend error, cut to the terminal width; a failed re-read after a write is no longer hidden by the success message.

## [0.1.1] - 2026-10-16

- Audit trail of reloads (ok/error) and create/update/delete, from the board and the subcommands: `A` in the board, `tkan audit`, filterable by `op`, `date`, `details`, `result`, `source`, `backend`, `id`, `session`. In memory by default; `audit.file` (`.jsonl` or `.csv`) keeps it across sessions and marks this and the previous one.
- csv option `poll`: `watch` (default, as before), `none` (read once; `r` reloads) or a number of seconds between checks for outside edits.
- Docs: `AGENTS.md` now requires a security review of staged changes (exposed secrets and other concerns) before every commit.
- Fix: the board did not show external changes to the file (not even with `r`) because the one-shot list cache was also on for the board.
- `sqlite` backend: tasks in a table of a SQLite database, configurable column mapping (`source`, `table`, `fields`), `poll`, and `cli` to choose the SQLite command line tool (default: first installed of `sqlite3`, `sqlite`). It runs the CLI, no SQLite library is linked, so the binary stays static; `tkan init` detects `.db`, `.sqlite` and `.sqlite3` files.
- `json`, `jsonl` and `yaml` backends: tasks in a JSON array (or `{"tasks": [...]}`), a JSON Lines file or a YAML list, with `fields` key mapping, `poll` and `indent`; unmapped keys round-trip through `extra`, with their types. `yaml` is selected with `-backend yaml` (the `.yaml` extension is not claimed).
- `json` and `yaml` option `path`: dotted keys to the task list inside a larger document (`data.tasks`); the rest of the document is kept on writes.
- `markdown` backend: a directory with one `.md` file per task (file name = id, YAML frontmatter = fields, body = description).
- `tkan init` writes a default `source` matching the backend (`tasks.json`, `tasks.yaml`, `tasks/`, ...) when none is given.
- Internal: the lock and atomic-write code of csv moved to `internal/backend/filestore`, shared with the new file backends.
- The `poll` option parser is shared (`backend.ParsePoll`).

## [0.1.0] - 2026-10-16

First release.

- `tkan version` shows the build date.
- `install.sh`: installs a pinned release for the current OS/arch with checksum verification.
- Kanban TUI over a configurable list of task backends (CSV, command-based `custom`).
- Non-interactive subcommands for scripts and agents: `list`, `show`, `add`, `edit`, `set-status`, `delete`, `count`, `labels`, `statuses`, `config`, `init`, `completion`.
- Filter language, configurable board columns, WIP limits and sorting.
- Config file `.tkan.yaml` (also `.tkan.yml`, `tkan.yaml`, `tkan.yml`; first found wins), looked up in the working directory, then `~/.tkan.yaml` / `~/.tkan.yml`.
- Static binaries for Linux (amd64, arm64, armv6, armv7), macOS (amd64, arm64) and Windows (amd64).

[Unreleased]: https://github.com/Desvelao/tkan/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/Desvelao/tkan/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/Desvelao/tkan/releases/tag/v0.1.0
