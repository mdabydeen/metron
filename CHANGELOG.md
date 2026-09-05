# Changelog

All notable changes to metron are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While metron
is pre-1.0, the minor version is bumped for breaking changes to configuration or CLI flags.

## [Unreleased]

### Added

- `metron --doctor` performs a side-effect-free readiness check of configuration,
  local tool dependencies, Ollama connectivity, and model tool support.
- `max_output_tokens` bounds each model generation (Ollama `num_predict`) instead of
  relying on Ollama's unlimited default.
- Security automation now runs CodeQL, Go vulnerability scanning, and pull-request
  dependency review. Releases include SPDX SBOMs and GitHub provenance attestations.
- The minimum Go version is 1.26.6, incorporating standard-library security fixes that are
  part of every statically linked metron binary.
- `go install github.com/mdabydeen/metron/cmd/metron@latest` now works: the module has a
  resolvable path and the command lives under `cmd/metron`.
- Homebrew installs via a cask rather than a formula, following GoReleaser's deprecation of
  `brews`. Casks are macOS-only, so Linux users install with `go install` or the release
  tarball. The cask strips the Gatekeeper quarantine attribute on install, without which an
  unsigned binary fails its first run.
- Apache-2.0 license, contributor guide, security policy and code of conduct.
- Prebuilt binaries for macOS and Linux on amd64 and arm64, published on tag, with a
  Homebrew tap.
- CI enforces the project's two standing invariants: 100% statement coverage, and a
  hermetic default test suite.
- Tools are confined to the project directory. Every path is resolved against the enclosing
  git work tree (or the working directory outside one) and refused if it lands outside,
  symlinks included, for files being created as well as read.
- metron can be started from any subdirectory of a project. Tools act on the project root
  rather than the process working directory, and `.tags` is written there.
- Only tools that can actually run are advertised to the model. A missing ripgrep withdraws
  `list_files` and `search_text`, a BSD `ctags` withdraws `find_symbol`, and running outside
  a git repository withdraws `apply_patch`. Their schemas are then not sent at all, and the
  system prompt stops naming them — on a stock Mac that is ~164 fewer prompt tokens on every
  request. A call to an unadvertised tool is refused before it runs.
- `run_command` runs one permitted command in the project and returns its exit status and
  output, so the agent can check its own edit rather than assert it worked. It is off by
  default: with no `allowed_commands` set the tool is not advertised and its schema is not
  sent. There is no shell -- the command is split on whitespace and executed directly, so
  shell metacharacters are inert -- the allowlist matches whole argv tokens, each run is
  approved at the prompt, and the whole process group is killed at the timeout so a spawned
  test binary cannot outlive it. Output is clipped keeping both ends.
- `Approve` now takes the kind of effect as well as the preview, so the prompt can ask
  "Run this command?" rather than "Apply this patch?" for something that is not a patch.
- `disabled_tools` withholds tools by name. An unknown name is a startup error, since a typo
  would otherwise leave the tool enabled.
- `/config` reports the advertised tool set and the size of its schemas, because that cost is
  paid on every request rather than only on the turns that use a tool.

### Changed

- `main.go` moved to `cmd/metron/main.go`. Build with `go build ./cmd/metron` rather than
  `go build main.go`; `make build` is unchanged.
- `agent.Options` carries a `tools.Env` — the project root plus the tool budgets — in place
  of its five separate budget fields. The tools are methods on that `Env`.

### Fixed

- Starting metron in a repository subdirectory now loads `.metron.json` from the
  repository root, matching the project root used by every tool.
- Live-model tests now construct the agent after entering their throwaway repository, so
  tool calls inspect and patch the fixture instead of metron's own checkout.
- `search_text` no longer lets a pattern beginning with a dash be parsed by ripgrep as a
  flag. A search for `--files` listed every file in the repository and applied no match
  budget at all; the pattern now follows a `--` separator. `list_files` passes its glob as
  `--glob=<pattern>` for the same reason.

## [0.1.0] — unreleased

First tagged release. Everything below was developed before the changelog existed.

### Added

- REPL with `/help`, `/config`, `/reset`, `/history`, `/tags` and `/exit`.
- Five budgeted tools: `list_files`, `find_symbol`, `search_text`, `view_slice` and
  `apply_patch`.
- Per-turn token accounting, with a warning when the prompt approaches `num_ctx`.
- History compaction: large file slices are purged once a turn completes, and history is
  trimmed to a message budget.
- Configuration via `.metron.json`, `~/.metron/config.json` (or
  `$METRON_CONFIG_DIR/.metron/config.json`) or `METRON_CONFIG`, overridden by
  `OLLAMA_HOST` and `OLLAMA_MODEL`.
- Startup preflight naming any missing dependency and the tool it disables, including the
  BSD-versus-Universal ctags distinction.
- One-shot mode (`-p`) that prints only the answer on stdout, failing closed on patches
  unless `--yes` is given.
- Streaming replies, with an idle watchdog that bounds silence rather than total generation
  time, and Ctrl-C cancellation of a turn in progress.

[Unreleased]: https://github.com/mdabydeen/metron/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mdabydeen/metron/releases/tag/v0.1.0
