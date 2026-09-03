# Changelog

All notable changes to metron are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While metron
is pre-1.0, the minor version is bumped for breaking changes to configuration or CLI flags.

## [Unreleased]

### Added

- `go install github.com/mdabydeen/metron/cmd/metron@latest` now works: the module has a
  resolvable path and the command lives under `cmd/metron`.
- Apache-2.0 license, contributor guide, security policy and code of conduct.
- Prebuilt binaries for macOS and Linux on amd64 and arm64, published on tag, with a
  Homebrew tap.
- CI enforces the project's two standing invariants: 100% statement coverage, and a
  hermetic default test suite.

### Changed

- `main.go` moved to `cmd/metron/main.go`. Build with `go build ./cmd/metron` rather than
  `go build main.go`; `make build` is unchanged.

## [0.1.0] — unreleased

First tagged release. Everything below was developed before the changelog existed.

### Added

- REPL with `/help`, `/config`, `/reset`, `/history`, `/tags` and `/exit`.
- Five budgeted tools: `list_files`, `find_symbol`, `search_text`, `view_slice` and
  `apply_patch`.
- Per-turn token accounting, with a warning when the prompt approaches `num_ctx`.
- History compaction: large file slices are purged once a turn completes, and history is
  trimmed to a message budget.
- Configuration via `.metron.json`, `$XDG_CONFIG_HOME/metron/config.json` or
  `METRON_CONFIG`, overridden by `OLLAMA_HOST` and `OLLAMA_MODEL`.
- Startup preflight naming any missing dependency and the tool it disables, including the
  BSD-versus-Universal ctags distinction.
- One-shot mode (`-p`) that prints only the answer on stdout, failing closed on patches
  unless `--yes` is given.
- Streaming replies, with an idle watchdog that bounds silence rather than total generation
  time, and Ctrl-C cancellation of a turn in progress.

[Unreleased]: https://github.com/mdabydeen/metron/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mdabydeen/metron/releases/tag/v0.1.0
