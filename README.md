# tack

A small terminal kanban board for a task backlog, plus non-interactive subcommands
(`list`, `show`, `add`, `edit`, `set-status`, `delete`, ...) for scripts and AI agents.
Tasks live in pluggable backends (CSV, JSON / JSON Lines / YAML files, and a command-based `custom` backend, which also runs the SQLite script in `contrib/`).
A single static binary: runs on Linux, macOS, Windows and Raspberry Pi (ARM), at 80x24.

> Pre-1.0: the config format, CSV schema and CLI may still change.

## Install

### From a release

The installer picks the binary for your OS and architecture, verifies it against `SHA256SUMS` and
installs it to `~/.local/bin`. It always installs the exact version of the tag it is fetched from,
never "latest":

```sh
curl -fsSL https://raw.githubusercontent.com/Desvelao/tack/v0.2.0/install.sh | sh
```

Options: `-v X.Y.Z` / `$TACK_VERSION` (another release), `-d DIR` / `$TACK_INSTALL_DIR` (install
directory, default `~/.local/bin`), `-b NAMES` / `$TACK_BACKENDS` and `-B DIR` / `$TACK_BACKEND_DIR`
(backend scripts, see [Backend scripts](#backend-scripts)). Pass options to a piped script with `sh -s --`, for
example `... | sh -s -- -d /usr/local/bin`. Linux (x86-64, arm64, ARMv7, ARMv6) and macOS (Intel, Apple
silicon) are supported; on Windows use the manual steps below.

To do it by hand, download the binary for your platform from the
[Releases](https://github.com/Desvelao/tack/releases) page, check it against `SHA256SUMS` and move
it to a directory on your `PATH`:

| Platform | Asset |
|---|---|
| Linux x86-64 | `tack` |
| Linux arm64 | `tack-linux-arm64` |
| Linux ARMv7 (Raspberry Pi 2/3, 32-bit OS) | `tack-linux-armv7` |
| Linux ARMv6 (Raspberry Pi Zero W / 1) | `tack-linux-armv6` |
| macOS Intel / Apple silicon | `tack-darwin-amd64` / `tack-darwin-arm64` |
| Windows x86-64 | `tack-windows-amd64.exe` |

```sh
ASSET=tack          # the asset from the table above
curl -LO https://github.com/Desvelao/tack/releases/download/v0.2.0/$ASSET
curl -LO https://github.com/Desvelao/tack/releases/download/v0.2.0/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS        # macOS: shasum -a 256 -c SHA256SUMS
install -Dm755 $ASSET ~/.local/bin/tack         # or: sudo install -m755 $ASSET /usr/local/bin/tack
tack version
```

`~/.local/bin` must be on your `PATH` (check with `echo $PATH`; otherwise add
`export PATH="$HOME/.local/bin:$PATH"` to your shell profile). On macOS, if Gatekeeper blocks the
binary, run `xattr -d com.apple.quarantine ~/.local/bin/tack`. On Windows, rename the file to
`tack.exe` and put it in a folder on your `PATH`.

### From source

Build it yourself (needs only Docker):

```sh
make build && make install      # ./bin/tack, then ~/.local/bin/tack
```

Or copy a binary you built (`make build` writes `bin/tack`) by hand: `cp bin/tack ~/.local/bin/tack`.

### Backend scripts

Backends that are not compiled in ship as scripts for the [`custom` backend](docs/backends.md#the-custom-backend);
today that is SQLite (`tack-sqlite.sh`, needs `bash`, `jq` and `sqlite3`). They are assets of every release,
listed in the same `SHA256SUMS` as the binaries, so there are several ways to get one:

- **With the installer** (checksum verified, nothing is installed if any file fails): add `-b sqlite` (or
  `-b all`). They go to `~/.local/share/tack/backends` (`-B DIR` to change it). With `-s` only the scripts are
  installed, for a tack you already have:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/Desvelao/tack/v0.2.0/install.sh | sh -s -- -b sqlite
  curl -fsSL https://raw.githubusercontent.com/Desvelao/tack/v0.2.0/install.sh | sh -s -- -s -b all -B ~/bin
  ```

- **By hand** from the release:

  ```sh
  curl -LO https://github.com/Desvelao/tack/releases/download/v0.2.0/tack-sqlite.sh
  curl -LO https://github.com/Desvelao/tack/releases/download/v0.2.0/SHA256SUMS
  sha256sum --ignore-missing -c SHA256SUMS        # macOS: shasum -a 256 -c SHA256SUMS
  install -Dm755 tack-sqlite.sh ~/.local/share/tack/backends/tack-sqlite.sh
  ```

- **From a clone**: `cp contrib/backends/sqlite/tack-sqlite.sh ~/.local/share/tack/backends/` (make the
  directory first, and `chmod +x` the file), or `make install-backends` (to `$PREFIX/share/tack/backends`,
  default `~/.local/share/tack/backends`).

Then point the config at the file, or copy it next to `tack.yaml` and use a relative path
(`command: [./tack-sqlite.sh, tasks.db, tasks]`). See [SQLite](docs/backends.md#sqlite-the-tack-sqlitesh-script).

## Quick start

```sh
tack init -source tasks.csv     # writes ./tack.yaml (also accepted: .tack.yaml and .tack.yml, which win, and tack.yml)
tack                            # open the board
tack list -q 'priority=high' -json
tack set-status 12 Done
```

## Documentation

- [docs/usage.md](docs/usage.md): building, keys, subcommands
- [docs/config.md](docs/config.md): `tack.yaml`, board columns, config lookup
- [docs/filter.md](docs/filter.md): filter language
- [docs/backends.md](docs/backends.md): CSV, JSON, JSON Lines, YAML and custom backends (with the SQLite script)
- [docs/AGENTS.md](docs/AGENTS.md): using tack from scripts and agents
- [AGENTS.md](AGENTS.md): developing this repo

## Development

Everything runs in Docker: `make test`, `make check`, `make build`, `make release`.

## License

[MIT](LICENSE)
