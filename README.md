# metron

A minimal, terminal-based coding agent that talks to a **local** [Ollama](https://ollama.com)
model. Its one defining constraint is **token discipline**: the model never reads a whole
file. Every look at your code goes through a narrow, budgeted tool, and large tool output is
purged from the conversation as soon as the turn that needed it is over.

The name is the Greek *metron* — a measure. That is the whole design: measure what the model
is allowed to see.

```
=== metron (model: qwen3.8:27b-mlx) ===
Context-disciplined terminal coder. /help for commands, /exit to quit.
config: .metron.json

metron > make Greet return "hola" instead of "hello"
[executing: find_symbol]
[executing: view_slice]
[executing: apply_patch]

Greet now returns "hola" (greet.go:4).
```

## Why

A 32B model on a laptop has a finite context window, and the fastest way to waste it is to
paste in a 2,000-line file so the model can change one line. metron makes that impossible:

| Budget | Default | Enforced by |
| --- | --- | --- |
| Lines per file read | 121 | `view_slice` rejects wider ranges outright |
| Search matches, total | 10 | `search_text` truncates and says so |
| Search matches per file | 2 | ripgrep's `--max-count` |
| Model round-trips per turn | 10 | the agent loop returns `max turns exceeded` |
| Slice retained after a turn | none | history compaction replaces it with a placeholder |

Every one of those is configurable — see [Configuration](#configuration).

The model is told, in the system prompt, that guessing and asking for whole files are both off
the table. The tool surface is what makes that instruction enforceable rather than aspirational.

## Requirements

- **Go 1.26+** to build.
- **A running Ollama server** with a **tool-capable** model pulled. Tool calling is not
  optional here — without it the agent cannot see your code at all. Check with
  `curl -s localhost:11434/api/tags | grep tools`.
- **[ripgrep](https://github.com/BurntSushi/ripgrep)** (`rg`) — required by `search_text`.
- **[Universal Ctags](https://ctags.io)** — required by `find_symbol`. The BSD `ctags`
  shipped with macOS/Xcode does **not** work; it rejects the `--fields=+nK` flag metron
  relies on. `brew install universal-ctags`, and make sure it comes first on `PATH`.
- **git**, since `apply_patch` edits your files with `git apply`.

metron checks all of this at startup and prints a warning per problem, naming the tool that
will be unavailable and how to fix it. Nothing is fatal: a missing binary disables one tool,
and the model is told so when it tries to use it.

The test suite needs none of these — see [Testing](#testing).

## Install

```bash
make build            # -> bin/metron
make install          # build, then sudo cp to /usr/local/bin
go run main.go        # run without building
```

## Usage

Run metron **from the root of the repository you want it to work on** — every tool resolves
paths relative to the process working directory, and the ctags index is written to `./.tags`.

```bash
cd ~/code/my-project
metron
```

Type a request at the prompt. Each line is one turn: metron sends it to the model, runs
whatever tools the model asks for (echoing `[executing: <tool>]` as it goes), and prints the
final answer. Conversation history persists across turns within a session and is never
written to disk.

### Commands

| Command | Effect |
| --- | --- |
| `/help` | list the commands |
| `/config` | print the effective settings and which file they came from |
| `/reset` | clear the conversation history, keeping the system prompt |
| `/tags` | rebuild the ctags index — do this after a big refactor |
| `/exit` | quit (also `exit`, `quit`, `/quit`, or Ctrl-D) |

### A note on `apply_patch`

metron edits your working tree directly. Patches are dry-run through `git apply --check` first
and rejected as a whole if they do not apply cleanly, so a bad patch leaves your files
untouched — but a *good* patch is applied without asking. **Work on a clean branch and commit
before you start**, so `git diff` shows you exactly what the model did.

## Configuration

Settings resolve in three layers, each overriding the one before:

```
built-in defaults  <  config file  <  environment variables
```

### Config file

JSON, and every key is optional — anything you leave out keeps its default. metron uses the
first file it finds:

1. `$METRON_CONFIG`, if set (an explicit path short-circuits the search)
2. `./.metron.json` — per-project, next to the code you are working on
3. `$XDG_CONFIG_HOME/metron/config.json`, else `~/.config/metron/config.json`

Copy [`metron.example.json`](metron.example.json) to get started; it lists every key at its
default value.

```bash
cp metron.example.json .metron.json
```

| Key | Default | Meaning |
| --- | --- | --- |
| `endpoint` | `http://localhost:11434/api/chat` | Ollama chat endpoint, path included |
| `model` | `qwen2.5-coder:32b` | model name to request |
| `timeout_seconds` | `180` | HTTP timeout for one model call — see the note below |
| `temperature` | `0.1` | sampling temperature |
| `top_p` | `0.95` | nucleus sampling cutoff |
| `num_ctx` | `16384` | context window requested from Ollama |
| `max_turns` | `10` | model round-trips allowed in one user turn |
| `compact_threshold_bytes` | `400` | tool output above this size is purged after the turn |
| `max_slice_lines` | `120` | widest span `view_slice` will read |
| `search_max_matches` | `10` | total `search_text` results |
| `search_max_per_file` | `2` | `search_text` results per file |

A file that exists but cannot be read or parsed is a startup error, not a silent fallback —
including unknown keys, so a typo like `"modle"` is reported instead of ignored. Values are
validated (positive budgets, `top_p` in `(0, 1]`) before the agent starts.

**The default model is probably not yours.** `qwen2.5-coder:32b` is a placeholder; set `model`
to something you have actually pulled and that reports the `tools` capability.

**Raise `timeout_seconds` for large local models.** Streaming is off, so the HTTP timeout has
to cover the *entire* generation, not just the first byte. A big model on a busy laptop can
spend several minutes on one reply and trip the 180s default mid-thought — this is the most
likely cause of `context deadline exceeded (Client.Timeout exceeded while awaiting headers)`.
The live tests use 900s for exactly this reason.

### Environment

Two variables override the file, for one-off runs:

| Variable | Overrides |
| --- | --- |
| `OLLAMA_HOST` | `endpoint` |
| `OLLAMA_MODEL` | `model` |
| `METRON_CONFIG` | which config file is read |

```bash
OLLAMA_MODEL=gemma4:12b-mlx metron
```

`/config` inside the REPL prints the result of all of this, so you never have to guess which
layer won.

## The tools

Four tools, each a standalone function in `internal/tools` with no shared state. This is the
complete surface the model has for touching your code.

### `find_symbol(symbol)`

Answers "where is this defined?" from a ctags index. Builds `.tags` lazily on first use
(`ctags -R --fields=+nK`, excluding `.git` and `vendor`) and reuses it afterwards, then matches
the symbol name exactly.

```
Greet [func] -> greet.go:3
Greet [type] -> internal/other/dup.go:9
```

The index is **not** refreshed automatically. Run `/tags` after a large refactor.

### `search_text(pattern)`

A deliberately crippled ripgrep: `rg -n --max-count=<per-file> <pattern> .`, with the overall
result count capped afterwards. Both caps matter, and only one of them can be delegated:
ripgrep's `-m` and `--max-count` are *the same flag* and it is **per file**, so the total
budget is applied by metron itself. Over-budget output is truncated with a
`[truncated to N matches; narrow the pattern]` line so the model knows it is not seeing
everything. A clean miss returns `No matches found.`; a bad regex or a missing `rg` comes back
as an error the model can read and react to.

### `view_slice(path, start, end)`

Reads one line range from one file, numbered `%5d | ` (that delimiter is also the marker
history compaction looks for later). Ranges wider than `max_slice_lines` are refused, as are
inverted ranges. A range running past EOF is silently clamped.

### `apply_patch(diff)`

Applies a unified diff via `git apply --check -` followed by `git apply -`, both fed over
stdin. Patch failures are returned as *text*, not Go errors, so the model sees git's own
complaint and can correct the diff itself. Only a missing `git` binary surfaces as a real
error, since that is an environment fault rather than a bad patch.

## Architecture

```
main.go              REPL and commands. No conversation state lives here.
internal/config      Settings: defaults, JSON file, environment, validation.
internal/ollama      HTTP client for Ollama's /api/chat, plus the shared wire types.
internal/agent       The agent loop, the system prompt, tool schemas, compaction.
internal/tools       The four tools plus the startup dependency check.
```

Seams are interfaces, so each layer can be driven in isolation: `agent.New` takes a `Chatter`
(the one-method subset of `*ollama.Client`), and `run` takes a `stepper` plus an
`io.Reader`/`io.Writer` instead of touching stdin directly.

### The loop

`Agent.Step` appends the user message, then iterates at most `max_turns` times:

1. Call the model with the full history and all four tool schemas.
2. Append the reply to history.
3. If the reply has no tool calls, compact the history and return the reply — done.
4. Otherwise run every tool call in the reply, appending each result as a `role: "tool"`
   message, and go back to step 1.

Exceeding the limit returns a `max turns exceeded` error. Unknown tool names and tool failures
are appended to history *as strings* rather than raised as Go errors: the model sees its own
mistakes and gets a chance to recover.

### Context compaction

`compactContext` runs once per completed `Step`, after the model has given its final
non-tool-call answer. Any `tool` message longer than `compact_threshold_bytes` that contains
the `view_slice` delimiter (`" | "`) is replaced with:

```
[File slice redacted after turn completion]
```

The model still saw the full slice while it was working; it just does not carry it into the
next user turn. `search_text` and `find_symbol` output is left alone — it is already bounded
and small.

### Adding a tool

Keep new tools narrow and output-bounded — that constraint is the entire point of this
codebase. Three edits:

1. Implement it in `internal/tools` as a plain function returning `(string, error)`.
2. Add its JSON schema to the package-level `toolDefs` slice in `internal/agent/loop.go`.
3. Add a `case` for it in `Agent.dispatch`.

Then extend `TestDispatchRoutesToolsAndReportsErrors` and `TestStepAdvertisesAllFourTools`.

## Testing

Three tiers, in increasing order of what they need from the world.

### 1. Hermetic (default)

```bash
make test     # go test ./...
make race     # go test -race
make cover    # coverage profile + total
make vet      # go vet ./...
make check    # all of the above
```

**100% statement coverage** across all five packages, race-clean, and no external services:

- **Ollama** is replaced by `httptest` servers that assert on the outgoing request body and
  return scripted replies, including tool calls.
- **`rg` and `ctags`** are replaced by executable shell-script shims written into a temp
  directory that becomes the entire `PATH` (see `internal/tools/helpers_test.go`). An empty
  shim directory is how the "binary not installed" branches get exercised.
- **git** is used for real, in throwaway repositories under `t.TempDir()`; those tests skip
  themselves if git is missing.
- Every test that touches the filesystem runs in its own `t.TempDir()` via `t.Chdir`.

`e2e_test.go` ties it together: a scripted fake Ollama drives the *real* HTTP client, agent
loop, and tools through a `find_symbol` → `view_slice` → `apply_patch` session against a real
temporary git repository, and asserts the file on disk actually changed.

### 2. Docker (real binaries)

Shims prove metron calls ripgrep correctly; they cannot prove ripgrep *behaves* the way metron
assumes. The `integration` build tag closes that gap by running the tools against the real
thing, and the image exists because many developer machines cannot — macOS ships BSD ctags and
no ripgrep at all.

```bash
make docker-test    # full suite + integration tests
make docker-race    # ... with the race detector
make docker-cover   # ... with a coverage total
```

The image ([`Dockerfile.test`](Dockerfile.test)) is `golang:1.26-alpine` plus ripgrep,
Universal Ctags, git and a C toolchain, and it runs the tests **unprivileged on purpose**:
several cases assert on permission failures, which root would bypass and silently skip.
`docker-compose.yml` exposes the same three runs as services.

This tier earns its keep. It is what caught the search budget being unenforced: `-m` and
`--max-count` are the same ripgrep flag, so the original `--max-count=2 -m 10` collapsed to
"10 matches per file, no total cap" — the opposite of the advertised budget. No shim would
ever have noticed.

> If `docker pull` hangs on your machine with `error getting credentials`, the Docker
> credential helper is the problem, not the image:
> `DOCKER_CONFIG=$(mktemp -d) docker pull golang:1.26-alpine` does an anonymous pull.

### 3. Live (real model)

```bash
make test-live                                   # smallest tool-capable model installed
METRON_TEST_MODEL=gemma4:12b-mlx make test-live  # pin one
METRON_TEST_TIMEOUT_SECONDS=1800 make test-live  # slower hardware
```

Talks to your actual Ollama server. It discovers a tool-capable model from `/api/tags` rather
than assuming the default is installed, and skips cleanly if the server is unreachable or no
such model exists. It checks that a real model round-trips, that it actually drives the tools
instead of answering from imagination, and that a patch session edits a real file.

Because model behaviour is not deterministic, the patch test reports a model that fails to
produce a valid diff as a **skip with the transcript**, not a failure — the agent loop
breaking is a failure; a small model writing a bad diff is a fact about the model.

Observed on a 12B local model (`gemma4:12b-mlx`), with neither ripgrep nor Universal Ctags
installed, so only `view_slice` and `apply_patch` were actually usable:

```
[executing: find_symbol] [executing: search_text] [executing: find_symbol]
[executing: view_slice]  [executing: view_slice]  [executing: apply_patch]
reply: Changed "hello" to "hola" in `greeter.go`.
--- PASS: TestLivePatchWorkflow (9.45s)
```

That run is also why tool failures are phrased at the model rather than at you. With the
earlier bare `Error: ripgrep error:`, the same model retried `search_text` **eight times** and
exhausted the turn budget without editing anything. Told plainly that the tool is unavailable
and not to retry, it tried once, fell back to `view_slice`, and finished the job.

## Limitations

- No fallback if `rg` or a compatible `ctags` is missing; the affected tool reports the error
  to the model and the startup check warns you.
- `.tags` is built once and never invalidated automatically — use `/tags`.
- History is unbounded apart from slice compaction; a very long session will still grow.
  `/reset` clears it.
- No streaming, so there is no output until the model finishes a reply — and the HTTP timeout
  has to cover the whole generation.
- No approval prompt before a patch is applied.
- Conversation history is lost when you exit.
- Ctrl-C kills the process rather than cancelling an in-flight model call.
