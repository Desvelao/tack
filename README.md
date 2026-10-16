# tkan

A small terminal kanban board for a task backlog, plus non-interactive subcommands
(`list`, `show`, `add`, `edit`, `set-status`, `delete`, ...) for scripts and AI agents.
Tasks live in pluggable backends (CSV, SQLite through its command line tool, and a command-based `custom` backend today).
A single static binary: runs on Linux, macOS, Windows and Raspberry Pi (ARM), at 80x24.

> Pre-1.0: the config format, CSV schema and CLI may still change.

## Install

### From a release

The installer picks the binary for your OS and architecture, verifies it against `SHA256SUMS` and
installs it to `~/.local/bin`. It always installs the exact version of the tag it is fetched from,
never "latest":

```sh
curl -fsSL https://raw.githubusercontent.com/Desvelao/tkan/v0.1.1/install.sh | sh
```

Options: `-v X.Y.Z` / `$TKAN_VERSION` (another release), `-d DIR` / `$TKAN_INSTALL_DIR` (install
directory, default `~/.local/bin`). Linux (x86-64, arm64, ARMv7, ARMv6) and macOS (Intel, Apple
silicon) are supported; on Windows use the manual steps below.

To do it by hand, download the binary for your platform from the
[Releases](https://github.com/Desvelao/tkan/releases) page, check it against `SHA256SUMS` and move
it to a directory on your `PATH`:

| Platform | Asset |
|---|---|
| Linux x86-64 | `tkan` |
| Linux arm64 | `tkan-linux-arm64` |
| Linux ARMv7 (Raspberry Pi 2/3, 32-bit OS) | `tkan-linux-armv7` |
| Linux ARMv6 (Raspberry Pi Zero W / 1) | `tkan-linux-armv6` |
| macOS Intel / Apple silicon | `tkan-darwin-amd64` / `tkan-darwin-arm64` |
| Windows x86-64 | `tkan-windows-amd64.exe` |

```sh
ASSET=tkan          # the asset from the table above
curl -LO https://github.com/Desvelao/tkan/releases/download/v0.1.1/$ASSET
curl -LO https://github.com/Desvelao/tkan/releases/download/v0.1.1/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS        # macOS: shasum -a 256 -c SHA256SUMS
install -Dm755 $ASSET ~/.local/bin/tkan         # or: sudo install -m755 $ASSET /usr/local/bin/tkan
tkan version
```

`~/.local/bin` must be on your `PATH` (check with `echo $PATH`; otherwise add
`export PATH="$HOME/.local/bin:$PATH"` to your shell profile). On macOS, if Gatekeeper blocks the
binary, run `xattr -d com.apple.quarantine ~/.local/bin/tkan`. On Windows, rename the file to
`tkan.exe` and put it in a folder on your `PATH`.

### From source

Build it yourself (needs only Docker):

```sh
make build && make install      # ./bin/tkan, then ~/.local/bin/tkan
```

## Quick start

```sh
tkan init -source tasks.csv     # writes ./tkan.yaml (also accepted: .tkan.yaml and .tkan.yml, which win, and tkan.yml)
tkan                            # open the board
tkan list -q 'priority=high' -json
tkan set-status 12 Done
```

## Documentation

- [docs/usage.md](docs/usage.md): building, keys, subcommands
- [docs/config.md](docs/config.md): `tkan.yaml`, board columns, config lookup
- [docs/filter.md](docs/filter.md): filter language
- [docs/backends.md](docs/backends.md): CSV, SQLite and custom backends
- [docs/AGENTS.md](docs/AGENTS.md): using tkan from scripts and agents
- [AGENTS.md](AGENTS.md): developing this repo

## Development

Everything runs in Docker: `make test`, `make check`, `make build`, `make release`.

## License

[MIT](LICENSE)
