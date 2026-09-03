# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Metron is a minimal, terminal-based coding agent that talks to a local Ollama model (default `qwen2.5-coder:32b`) over its `/api/chat` endpoint. Its defining design goal is token discipline: it never lets the model read whole files, forcing it through narrow, budgeted tools (ctags symbol lookup, ripgrep search, bounded line-range slices) and purging large tool outputs from history once a turn completes.

## Commands

```bash
go build -ldflags="-s -w" -o bin/metron ./cmd/metron  # build (also: make build)
make install                                       # build + copy to /usr/local/bin (sudo)
make clean                                         # remove bin/, .tags, coverage.out
go run ./cmd/metron                                # run without building
make test                                          # go test ./... (hermetic)
make race                                          # go test -race -count=1 ./...
make cover                                         # coverage profile + total
make vet                                           # go vet ./...
make check                                         # vet + test + race + cover
make lint                                          # golangci-lint (needs it on PATH)
make release-check                                 # validate .goreleaser.yaml
make release-snapshot                              # cross-platform build, no tag
make docker-test                                   # + integration tests, real rg/ctags/git
make test-live                                     # + a real model on the local Ollama
```

The default test suite is hermetic and keeps 100% statement coverage: no Ollama server, no
ripgrep and no Universal Ctags needed (git is used for real, and those tests skip if it is
missing). Keep it that way — see the Testing section below. Runtime requires `ctags` and `rg` (ripgrep) on PATH, and a reachable Ollama server. Connection settings come from `internal/config` (a `.metron.json` file, overridden by `OLLAMA_HOST`/`OLLAMA_MODEL`).

Note for local work on this machine: there is no `rg` binary and `/usr/bin/ctags` is BSD ctags, so `find_symbol` and `search_text` cannot actually run here — use `make docker-test` to exercise them. Streaming is off, so `timeout_seconds` must cover a whole generation; the 180s default is tight for a large local model.

## Architecture

Single-binary CLI with three layers:

- **`cmd/metron/main.go`** — REPL: `main` reads config from the environment and hands off to `run`, which reads a line at a time, calls `agent.Step` once per input, and prints the reply. `run` takes an `io.Reader`/`io.Writer` and a `stepper` interface so the REPL is testable without stdin or a model. No conversation state lives here.
- **`internal/repomap`** — `Build(root, budgetTokens)` returns a ranked structural summary injected once per session. Ranked by git churn, tie-broken by symbol density; a cheap ordering runs first so only the head of the list is ever parsed, which is what keeps a 3,000-file tree at a few milliseconds. Never exceeds its budget.
- **`internal/llm`** — the provider-neutral vocabulary (`Message`, `ToolCall`, `Tool`, `Usage`, `Reply`, `Options`, `Provider`). The agent speaks only this; each provider translates at its own edge.
- **`internal/openai`** — the OpenAI chat-completions provider. The hard part is streaming: content arrives as deltas, but tool calls arrive as fragments keyed by `index`, with the name appearing once and the arguments a JSON *string* split across chunks. Reassembling by index rather than by arrival order is what keeps two tool calls from becoming one corrupt one. `stream_options.include_usage` is requested because most servers otherwise report no token counts while streaming, and measuring that number is the point of the program.
- **`internal/ollama`** — thin HTTP client (`Client.Chat`) for Ollama's chat API. Defines the wire types (`Message`, `ToolCall`, `Tool`, `ChatRequest`/`ChatResponse`) shared between the agent and the model.
- **`internal/session`** — conversation transcripts as JSONL (metadata line, then one message per line). `Save` is atomic via temp-file-and-rename and writes 0600, since a transcript holds every tool result the model saw. `.metron/.gitignore` holds `*` so the directory ignores itself in the project it lives in. Session ids are validated timestamps, because `Path` joins them onto a directory.
- **`internal/config`** — settings: `Defaults()`, a JSON file, then environment overrides, then `Validate()`. Resolution order is defaults < file < env; `auto_approve_patches`, `allowed_commands` and `save_sessions` are *refused* from a project-level `.metron.json`, because that file ships with the repository and would otherwise let a clone grant itself execution before the first turn. a file that exists but is unreadable, malformed, or has an unknown key is a startup error, never a silent fallback. Every tunable that used to be a magic number lives here.
- **`internal/agent`** — the agent loop (`Agent.Step`) and the system prompt that establishes the tool-only-access-to-code contract. Owns the persistent `[]ollama.Message` history across turns. `New` takes a `Chatter` interface (the one-method subset of `*ollama.Client`) so the loop can be driven by a fake in tests.
- **`internal/tools`** — the seven tools exposed to the model, plus the environment they run in:
  - `env.go`: `Env` — the project `Root` every path is resolved against and the `Budgets` every tool enforces. The tools are methods on it rather than free functions: `Root` has to reach all of them, and threading five budget parameters through each call had already stopped scaling. State is passed in, never global, so a caller can drive the tools against a scratch tree without touching process state. `resolve` rejects any path that escapes `Root` *after* following symlinks — checking before would refuse legitimate paths, since on macOS `/var` is a symlink to `/private/var`.
  - `ctags.go`: `FindSymbol` — lazily builds `.tags` via `ctags -R` (skipped if `.tags` already exists) and greps it for exact symbol matches.
  - `search.go`: `SearchText` — `rg -n --max-count=<per-file>`, with the overall match count capped in Go afterwards. Do not try to push the total budget into ripgrep: `-m` and `--max-count` are the same flag and it is per-file, which is why the original `--max-count=2 -m 10` silently enforced nothing.
  - `slice.go`: `ViewSlice` — reads a line range from one file, capped at `max_slice_lines`, output prefixed with `%5d | ` line numbers.
  - `preflight.go`: the canonical tool names (`ToolNames`), the dependency table, and one evaluation of it (`problems`) that both `Preflight` (operator warnings, including the BSD-vs-Universal ctags distinction) and `UnavailableTools` (what the agent may advertise) read from, so the two cannot disagree. Also `RebuildTags`, which backs `/tags`.
  - `missing.go`: `missingBinary` — turns a missing external binary into a message aimed at the *model* ("X is not installed, so <tool> is unavailable. Do not retry it; <alternative>"). This exists because a vague error made a live model retry `search_text` eight times and exhaust the turn budget. Keep tool-failure text actionable.
  - `edit.go`: `EditFile` — the search/replace alternative to `apply_patch`, selected by `Env.EditFormat`. `locate` runs a ladder of comparisons (exact, ignoring trailing whitespace, ignoring indentation) and takes the first rung that finds *exactly one* match; more than one is an error telling the model to quote more, because guessing between two matches is how an agent silently edits the wrong function. A loose match records the indentation delta so the replacement is re-indented to the file's own. `diffHunk` synthesises the unified diff for the approval prompt directly from the matched span -- no diff algorithm is needed, since the replaced range is known exactly -- because an operator should never approve a change in a notation invented for the model's convenience.
  - `command.go`: `RunCommand` — the only tool that can cause an effect metron cannot describe in advance. Never uses a shell: `strings.Fields` then `exec.Command`, so metacharacters are inert. `Env.Allowed` holds argv prefixes matched per token (so `"go test"` refuses `go tool` and `env go test`); an empty allowlist means the tool is not advertised at all. Runs in its own process group and kills the group on timeout, because `go test` spawns a test binary that would otherwise outlive the deadline — and, holding the output pipe open, would stop `CombinedOutput` returning. Returns only a string: a refusal, a timeout, a missing binary and a failing test are all things the model should read, not Go errors.
  - `procgroup_unix.go` / `procgroup_windows.go`: process-group setup and kill. metron ships darwin/linux only; the Windows file exists so the package still builds there.
  - `patch.go`: `ApplyPatch` — applies a unified diff via `git apply --check` (dry run) then `git apply`, both fed the diff over stdin. Patch failures come back as *text* (so the model can read git's complaint and retry); only a missing `git` binary is returned as a Go error.

### Agent loop mechanics (`internal/agent/loop.go`)

- `Agent.Step` appends the user message, then loops up to 10 turns: call the model, append its reply, and if it returned `ToolCalls`, dispatch each one and append the result as a `role: "tool"` message before calling the model again. It returns as soon as a model reply has no tool calls.
- `dispatch` maps a `ToolCall.Function.Name` to the corresponding `internal/tools` function; unknown tool names and tool errors are returned as strings back into message history rather than as Go errors, so the model sees and can react to failures.
- `Agent.Options.MaxPromptTokens` bounds a whole turn. Enforcement is *predictive* -- `prompt_eval_count` arrives after the tokens are spent, so a ceiling that waited for it could only report overruns. `estimatePromptTokens` divides history-plus-schema bytes by a `bytesPerToken` ratio that `calibrate` corrects (EWMA, weighted towards history) against every real count. `enforceBudget` runs a ladder: compact slices, then trim oldest exchanges, then stop with an answer -- never an error, since the operator asked for an answer within a ceiling.
- `Agent.Options` carries `MaxTurns`, `CompactThreshold` and the three tool budgets; `Agent.Reset` clears history back to the system prompt (backs `/reset`).
- `compactContext` runs once a `Step` call finishes (i.e., after the model gives a final non-tool-call answer): any `tool`-role message over 400 chars whose content contains `" | "` (the `ViewSlice` line-number delimiter) is replaced with a placeholder. This is what keeps `ViewSlice` output from permanently bloating the conversation history across multiple user turns — it does not affect `search_text` or `find_symbol` output.
- The tool schemas live in the package-level `toolDefs` map, keyed by name. `New` computes the *advertised* subset once -- everything less what `Env.UnavailableTools` reports broken and less `Options.DisabledTools` -- and only those schemas are sent. Schemas are paid for on every `Chat` call, so an unusable tool is a standing cost; the system prompt is generated from the same subset, since naming a tool the model cannot call invites it to try. `dispatch` refuses an unadvertised call before running it, phrased like `missingBinary`.

### Adding a new tool

Add the implementation to `internal/tools` as a method on `Env`, taking its budget from `Env.Budgets` and routing every path through `Env.resolve`. Add the budget to `tools.Budgets`, `config.Config`, `config.Defaults()` and `Config.Validate()`. Add its JSON schema to the package-level `toolDefs` slice and a case in `dispatch`. Then extend `TestDispatchRoutesToolsAndReportsErrors` and the tool-advertisement test. Keep new tools narrow and output-bounded — that constraint is the whole point of this codebase.

Shelling out: build the argument vector explicitly and never through a shell. Where a model-supplied string is a positional argument, put `--` before it (`search.go`) or pass it as `--flag=value` (`list.go`); without that, a pattern starting with a dash is parsed as a flag and silently changes what the tool does. Set `cmd.Dir = e.Root` so the tool acts on the project rather than on whatever the process working directory happens to be.

## Testing

- External binaries are faked by writing executable shell scripts into a temp dir that becomes the *entire* `PATH` (`shimDir` in `internal/tools/helpers_test.go`); an empty shim dir is how the "binary not installed" branches are covered. Do not add tests that depend on the host having `rg` or Universal Ctags — this machine has neither (macOS ships BSD `ctags`, which rejects `--fields=+nK`).
- Ollama is faked with `httptest` servers that assert on the request body and return scripted replies.
- Two extra tiers sit behind build tags. `-tags=integration` (`internal/tools/integration_test.go`) runs the tools against real ripgrep/ctags/git and is what `make docker-test` exists for — `Dockerfile.test` runs unprivileged on purpose so the permission-failure cases are not skipped as root. `-tags=live` (`live_test.go`) drives a real model on the local Ollama, discovering a tool-capable one from `/api/tags` rather than assuming the default model is installed. Nondeterministic model behaviour is a skip, not a failure; a broken agent loop is a failure.
- Filesystem-touching tests run in `t.TempDir()` via `t.Chdir`, since the tools use relative paths.
- `e2e_test.go` (package `main`) drives the real client + agent + tools through a scripted `find_symbol` -> `view_slice` -> `apply_patch` session against a throwaway git repo.
