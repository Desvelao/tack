# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow [SemVer](https://semver.org/);
before 1.0 the config format, CSV schema and CLI may still change.

## [Unreleased]

## [0.1.0] - 2026-10-16

First release.

- `tkan version` shows the build date.
- `install.sh`: installs a pinned release for the current OS/arch with checksum verification.
- Kanban TUI over a configurable list of task backends (CSV, command-based `custom`).
- Non-interactive subcommands for scripts and agents: `list`, `show`, `add`, `edit`, `set-status`, `delete`, `count`, `labels`, `statuses`, `config`, `init`, `completion`.
- Filter language, configurable board columns, WIP limits and sorting.
- Config file `.tkan.yaml` (also `.tkan.yml`, `tkan.yaml`, `tkan.yml`; first found wins), looked up in the working directory, then `~/.tkan.yaml` / `~/.tkan.yml`.
- Static binaries for Linux (amd64, arm64, armv6, armv7), macOS (amd64, arm64) and Windows (amd64).

[Unreleased]: https://github.com/Desvelao/tkan/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Desvelao/tkan/releases/tag/v0.1.0
