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
- **`internal/ollama`** — thin HTTP client (`Client.Chat`) for Ollama's chat API. Defines the wire types (`Message`, `ToolCall`, `Tool`, `ChatRequest`/`ChatResponse`) shared between the agent and the model.
- **`internal/config`** — settings: `Defaults()`, a JSON file, then environment overrides, then `Validate()`. Resolution order is defaults < file < env; a file that exists but is unreadable, malformed, or has an unknown key is a startup error, never a silent fallback. Every tunable that used to be a magic number lives here.
- **`internal/agent`** — the agent loop (`Agent.Step`) and the system prompt that establishes the tool-only-access-to-code contract. Owns the persistent `[]ollama.Message` history across turns. `New` takes a `Chatter` interface (the one-method subset of `*ollama.Client`) so the loop can be driven by a fake in tests.
- **`internal/tools`** — the four tools exposed to the model, each a standalone package function with no shared state:
  - `ctags.go`: `FindSymbol` — lazily builds `.tags` via `ctags -R` (skipped if `.tags` already exists) and greps it for exact symbol matches.
  - `search.go`: `SearchText` — `rg -n --max-count=<per-file>`, with the overall match count capped in Go afterwards. Do not try to push the total budget into ripgrep: `-m` and `--max-count` are the same flag and it is per-file, which is why the original `--max-count=2 -m 10` silently enforced nothing.
  - `slice.go`: `ViewSlice` — reads a line range from one file, capped at `max_slice_lines`, output prefixed with `%5d | ` line numbers.
  - `preflight.go`: `Preflight` (startup dependency check, including the BSD-vs-Universal ctags distinction) and `RebuildTags` (backs the `/tags` command).
  - `missing.go`: `missingBinary` — turns a missing external binary into a message aimed at the *model* ("X is not installed, so <tool> is unavailable. Do not retry it; <alternative>"). This exists because a vague error made a live model retry `search_text` eight times and exhaust the turn budget. Keep tool-failure text actionable.
  - `patch.go`: `ApplyPatch` — applies a unified diff via `git apply --check` (dry run) then `git apply`, both fed the diff over stdin. Patch failures come back as *text* (so the model can read git's complaint and retry); only a missing `git` binary is returned as a Go error.

### Agent loop mechanics (`internal/agent/loop.go`)

- `Agent.Step` appends the user message, then loops up to 10 turns: call the model, append its reply, and if it returned `ToolCalls`, dispatch each one and append the result as a `role: "tool"` message before calling the model again. It returns as soon as a model reply has no tool calls.
- `dispatch` maps a `ToolCall.Function.Name` to the corresponding `internal/tools` function; unknown tool names and tool errors are returned as strings back into message history rather than as Go errors, so the model sees and can react to failures.
- `Agent.Options` carries `MaxTurns`, `CompactThreshold` and the three tool budgets; `Agent.Reset` clears history back to the system prompt (backs `/reset`).
- `compactContext` runs once a `Step` call finishes (i.e., after the model gives a final non-tool-call answer): any `tool`-role message over 400 chars whose content contains `" | "` (the `ViewSlice` line-number delimiter) is replaced with a placeholder. This is what keeps `ViewSlice` output from permanently bloating the conversation history across multiple user turns — it does not affect `search_text` or `find_symbol` output.
- The four tool schemas (JSON Schema `parameters`) live in the package-level `toolDefs` slice and are passed to every `Chat` call. `maxTurns` bounds the loop.

### Adding a new tool

Add the implementation to `internal/tools`, add its JSON schema to the package-level `toolDefs` slice, and add a case in `dispatch`. Then extend `TestDispatchRoutesToolsAndReportsErrors` and `TestStepAdvertisesAllFourTools`. Keep new tools narrow and output-bounded — that constraint is the whole point of this codebase.

## Testing

- External binaries are faked by writing executable shell scripts into a temp dir that becomes the *entire* `PATH` (`shimDir` in `internal/tools/helpers_test.go`); an empty shim dir is how the "binary not installed" branches are covered. Do not add tests that depend on the host having `rg` or Universal Ctags — this machine has neither (macOS ships BSD `ctags`, which rejects `--fields=+nK`).
- Ollama is faked with `httptest` servers that assert on the request body and return scripted replies.
- Two extra tiers sit behind build tags. `-tags=integration` (`internal/tools/integration_test.go`) runs the tools against real ripgrep/ctags/git and is what `make docker-test` exists for — `Dockerfile.test` runs unprivileged on purpose so the permission-failure cases are not skipped as root. `-tags=live` (`live_test.go`) drives a real model on the local Ollama, discovering a tool-capable one from `/api/tags` rather than assuming the default model is installed. Nondeterministic model behaviour is a skip, not a failure; a broken agent loop is a failure.
- Filesystem-touching tests run in `t.TempDir()` via `t.Chdir`, since the tools use relative paths.
- `e2e_test.go` (package `main`) drives the real client + agent + tools through a scripted `find_symbol` -> `view_slice` -> `apply_patch` session against a throwaway git repo.
