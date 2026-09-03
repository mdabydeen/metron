# Changelog

All notable changes to metron are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While metron
is pre-1.0, the minor version is bumped for breaking changes to configuration or CLI flags.

## [Unreleased]

### Added

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
- An OpenAI-compatible provider, selected with `"provider": "openai"`. One wire format
  reaches llama.cpp's server, LM Studio, vLLM, OpenRouter and Ollama's own compatibility
  endpoint, so metron works with whatever you already run. The agent's vocabulary moved to
  `internal/llm`, so the loop no longer knows whose API is on the other end. An API key is
  named by `api_key_env` and read from the environment, never held in a config file.
- Session persistence. With `save_sessions` on, the conversation is written to
  `.metron/sessions/<id>.jsonl` as each turn completes, and `/save`, `/sessions`,
  `--resume <id>` and `--resume-last` manage it. The directory ignores itself in git.
  Off by default, and written 0600, because a transcript holds every file the model read.
  Resuming against a tree that has moved since the session was saved warns first.
- `metron -p ... --json` prints one object describing the run: the answer, the tools it ran
  and how long each took, the tokens it cost, and the files it changed. `files_changed` comes
  from `git status`, so it is true whichever edit format was used. A failed run still emits a
  valid object.

### Security

- **A project's `.metron.json` can no longer grant itself permissions.** It is the
  highest-priority config file and ships inside whatever repository metron is pointed at, so
  a cloned repository could set `auto_approve_patches` and `allowed_commands` and turn
  `git clone && metron` into arbitrary code execution before the first turn. Those settings,
  and `save_sessions`, are now honoured only from the user-level config or a file named by
  `METRON_CONFIG`; a project file that sets them is reported and ignored.
- **`edit_file` can no longer write inside `.git`.** Being under the project root is not the
  same as being safe to write: `.git/config` holds settings git executes, so an edit setting
  `core.pager` runs a command at the next `git log`, invisibly to both `git status` and any
  diff. `git apply` refuses these paths itself, so `apply_patch` was always covered and the
  new edit format was not. `.metron` is refused too, and the check is case-insensitive.
- **`view_slice` bounds no longer overflow.** `end - start` on model-supplied numbers wrapped
  negative, sailed past the budget check, and read a whole file in one call -- defeating the
  guarantee the program exists to make.
- **The approval prompt cannot be spoofed.** The preview is written by the model; control
  characters in it are now escaped rather than executed, so a diff cannot clear the screen
  and redraw a different, innocuous-looking patch. Over-long previews are truncated with a
  count instead of scrolling the real hunk out of the terminal.
- `run_command` no longer blocks past its deadline when a child escapes the process group,
  and no longer claims to have killed a command that is still running.
- `apply_patch` reads rename and copy headers, and unquotes C-escaped paths, so its own path
  check sees diffs that previously reached git unexamined.
- `ctags` no longer follows symlinks out of the project, and `find_symbol` no longer reports
  index paths the other tools would refuse to open.
- `--resume` validates the session id rather than joining it straight onto a path.

- `edit_file`, an anchored search/replace edit format, selected with
  `"edit_format": "search_replace"`. Unified diffs need exact line numbers and hunk headers,
  which is what small models most reliably get wrong; this asks them to quote lines they have
  already read instead. The quote must match exactly once -- ambiguity is an error telling the
  model to quote more, never a guess -- and matching runs a ladder from exact, through
  ignoring trailing whitespace, to ignoring indentation, re-indenting the replacement to the
  file's own indentation and saying when the match was not exact. The operator still approves
  a unified diff. It needs no git, so it is the only working edit path on a machine without
  one. `edit_format` defaults to `diff`; the two edit tools are alternatives and only the
  selected one is advertised.
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
