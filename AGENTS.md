# AGENTS.md

Guidance for AI agents **developing** this repo. If you only need to *use* tkan to
read or update a task file, read `docs/AGENTS.md` instead.

## The project

**tkan**: a small Go TUI that shows a task backlog as a kanban board, plus
non-interactive subcommands (`list`, `show`, `add`, `edit`, `set-status`, `delete`, `count`, `labels`, `statuses`, `config`, `init`, `completion`) for scripts and agents.
The task stores sit behind a `Backend` interface; the config lists any number of them
(`backend:` entries with a `type`, optional `name` and `options`). CSV, JSON/JSONL/YAML files, a Markdown directory, a SQLite one (drives the `sqlite3` CLI, no
library, to keep the binary static) and a command-based `custom` backend exist; GitHub Projects etc. are expected later.

Stack: Go, Bubble Tea + Lip Gloss + Bubbles, Docker for everything.

## Layout

- `src/cmd/tkan`: flag parsing, subcommands, TUI start-up
- `src/internal/task`: `Task` model (status is free text)
- `src/internal/backend`: `Backend` interface, `Spec`/`Factory` type registry, `Set` (combines the configured backends)
- `src/internal/backend/csv`: the CSV backend (the reference implementation)
- `src/internal/backend/custom`: runs an external command, JSON over stdin/stdout
- `src/internal/backend/filestore`: lock, atomic write and the whole-file `Store`/`Codec`/`Mapping` shared by json, jsonl and yaml
- `src/internal/backend/json`, `yaml`, `markdown`: JSON / JSON Lines, YAML list, one-Markdown-file-per-task (frontmatter) backends
- `src/internal/backend/sqlite`: SQLite table through the sqlite3 CLI (JSON out, SQL on stdin; values as hex literals)
- `src/internal/board`: raw status value -> kanban column mapping (pure, no backend/UI imports)
- `src/internal/config`: `tkan.yaml` loading (`backend:` list), config lookup, sample generation (`init.go`)
- `src/internal/query`: filter language (parser, matcher, `Help()` text used by the TUI and `tkan help filter`)
- `src/internal/tui`: Bubble Tea model (`model.go`) and rendering (`view.go`)
- `docs/`: `usage.md`, `config.md`, `filter.md`, `backends.md`, `AGENTS.md` (for agents using the tool)
- `tkan.example.yaml`: annotated example config

## Development rules

1. **Everything runs in Docker.** No Go toolchain is required on the host:
   `make test`, `make build`, `make build-arm`, `make tidy`, `make fmt`, `make vet`, `make shell`
   (thin wrappers over `docker compose run --rm <service>`). Containers run as the host
   UID/GID (`HOST_UID`/`HOST_GID`, set by the Makefile) so no root-owned files appear.
2. **The TUI and CLI depend only on `backend.Set`/`backend.Backend` and `board.Board`.** Nothing CSV-specific may
   leak out of `src/internal/backend/csv`. Backends stay *raw* (status is whatever the store holds);
   columns, names, order and value mapping live in `board`, configured by `tkan.yaml`.
   Never hardcode `pending`/`done` outside `board.DefaultSpec` and tests. New backends register in `init()` via
   `backend.RegisterType(type, factory, exts...)`, are blank-imported in `cmd/tkan/main.go`, and need
   a sample generator (`backend.RegisterExample`, used by `tkan init`), tests, and a note in
   `docs/backends.md`.
3. **CSV writes must stay safe**: temp file + rename in the same directory, unknown columns and
   line endings (LF/CRLF) preserved, external edits merged (re-read before mutate), never hand-roll
   quoting.
4. **Low-spec friendly**: static binary (`CGO_ENABLED=0`), small dependency set, works at 80x24 and
   on ARM (Raspberry Pi). No mouse-only or hover-only interactions; every action has a key.
5. **Never test against a real backlog.** Use a copy of the file (e.g. in a temp dir).

## Workflow

- Releasing: bump `VERSION` **and** the `VERSION=` line in `install.sh` (checked by `make check` and CI), and the tag in the README install URL; move the `CHANGELOG.md` entries under the new version.
- `make test` must pass (also run `make vet`).
- **Before every commit, check the staged changes for security problems.** Review `git diff --staged`
  (and any new untracked files being added) for exposed secrets (API keys, tokens, passwords, private
  keys, `.env` contents, credentials in `tkan.yaml` backend `options`, real backlog data or personal
  paths/emails in fixtures or docs) and for other concerns (command injection in the `custom` backend,
  unsafe file permissions or paths, unvalidated input, new dependencies of unknown provenance). Use a
  scanner such as `gitleaks`/`git secrets` if available. If anything is found, do not commit: remove it
  (rotate the secret if it was ever pushed) and report it to the user.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`,
  `chore:`, `refactor:`, `test:`, optional scope like `feat(tui):`; `!` or a `BREAKING CHANGE:` footer for breaking
  changes). End every commit message with the sign-off trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- When keys, flags, the config format or the CSV schema change, update `docs/usage.md`,
  `docs/config.md`, `docs/AGENTS.md` and `tkan.example.yaml`.
- Filter operators live in `internal/query` only; `query.Help()` is the single source of the help text
  (TUI page, `tkan help filter`) and `docs/filter.md` must mirror it. Keep the help page within 20 lines.
- Subcommand exit codes are part of the contract for agents (`0` ok, `1` error, `2` usage, `3` not found).
