# Changelog

All notable changes to metron are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While metron
is pre-1.0, the minor version is bumped for breaking changes to configuration or CLI flags.

## [0.1.0] — 2026-09-03

The first tagged release.

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
- The agent loop is importable: `agent`, `tools`, `llm`, `ollama` and `openai` moved out of
  `internal/`. `API.md` documents the surface and states plainly that it is unstable until
  v1.0.0, along with the three things that have to be true first.
- `system_prompt_extra` appends per-model nudges to the generated system prompt. They live in
  config rather than in the binary, so metron ships no unverified claims about particular
  models and `make bench` can decide which nudges earn the tokens they cost.
- `max_prompt_tokens` caps what a single turn may spend, and `/budget` sets it mid-session.
  Every other budget bounds one tool; this bounds the turn. Enforcement is predictive --
  token counts only arrive after the tokens are spent -- and the estimate is corrected
  against every count the server reports, so it converges on the tokeniser in use. On
  approaching the ceiling metron sheds context in order (file slices first, then the oldest
  exchanges, both stated in the history) rather than truncating mid-thought; if there is
  nothing left to shed it stops and says so, as an answer rather than an error.
- `profile` supplies a starting set of budgets: `tight`, `standard`, `roomy`. Individual
  settings in the same file still win. The values are reasoned rather than measured, and the
  documentation says so -- `make bench` is how to check them against your own model.
- `find_symbol` works without Universal Ctags. metron now carries a pure-Go symbol index
  (`go/ast`), so on a stock Mac -- where Apple's BSD ctags rejects the flags metron needs --
  the tool is available rather than withdrawn, on the very language metron is written in.
  Where Universal Ctags is present it is still used, now with `--fields=+neK` so results
  report a symbol's *span* (`greet.go:3-7`) rather than only where it starts, which saves the
  model a guessed `view_slice` range.
- `repo_map_tokens` injects a ranked structural summary of the project once per session, so
  the model starts with a picture instead of guessing filenames. Ranked by recent git churn,
  tie-broken by symbol density. Off by default: it is paid for on every request of the
  session, and whether the turns it saves outweigh that is the benchmark's question.
- An OpenAI-compatible provider, selected with `"provider": "openai"`. One wire format
  reaches llama.cpp's server, LM Studio, vLLM, OpenRouter and Ollama's own compatibility
  endpoint, so metron works with whatever you already run. The agent's vocabulary moved to
  `llm`, so the loop no longer knows whose API is on the other end. An API key is
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

### Decided against

- **MCP support.** Every budget metron enforces is one it implements itself; an MCP server
  brings a tool surface metron did not write and cannot bound -- schemas of arbitrary size on
  every request, results of arbitrary size into the context window. That is the one guarantee
  this program makes, so the README now says why it is absent and what would have to be true
  to change the answer.

### Security

- **A project's `.metron.json` cannot choose the model endpoint.** The privileged list
  covered the settings that grant execution, but not the ones that decide *where the model
  is* and *what it is told* -- so a cloned repository could set `provider`, `endpoint`,
  `api_key_env` and `system_prompt_extra`, pointing metron at a server it ran, naming an
  environment variable for metron to send as a bearer token, and appending its own
  instructions to the system prompt. `git clone && metron` would then exfiltrate a secret and
  hand the attacker a read primitive over the project through tools that never prompt. The
  key list and the code that resets those settings are now one structure, because they were
  two, drifted, and let this happen twice.
- **A non-local endpoint is announced at startup**, on stderr, noting when it is plain HTTP.
- **Model output can no longer drive the terminal.** The streamed reply, the final answer and
  error text carrying a server's own message were printed raw. A comment in a file metron
  reads is enough to steer what the model writes, so those strings could clear the screen,
  overpaint a diff just approved, retitle the window, or write the clipboard with OSC 52.
- **Replies from a model server are bounded** -- total size, assembled text, tool-call count
  and argument length. The idle watchdog resets on every chunk, so a server that streams
  steadily is never idle and, unbounded, exhausts memory.
- **The per-turn ceiling survives a server that misreports token counts.** The
  bytes-per-token estimate is clamped. This is a correctness fix before a security one: a
  server reporting only uncached tokens under prompt caching poisoned it identically, after
  which the ceiling silently stopped holding.
- **Session ids from a transcript are validated on save, not only on load**, and a transcript
  whose recorded id disagrees with its filename is refused. `--resume` adopts the saved
  identity, so the id in the file was the one a later save wrote with.
- **`Restore` allowlists conversational roles** rather than skipping `system`. A role of
  `developer`, or `system ` with a trailing space, would otherwise be restored with authority
  -- and transcripts are exactly the thing people attach to bug reports.
- **`.metron/.gitignore` is checked by content.** A repository shipping a hollow one kept it,
  and transcripts -- every file the model read -- became visible to `git add -A`.
- **The built-in Go symbol index is bounded**, in files examined and file size, like every
  other tool.

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

### Earlier work

The first cut, developed before this changelog existed.


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

[0.1.0]: https://github.com/mdabydeen/metron/releases/tag/v0.1.0
