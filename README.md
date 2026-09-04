# metron

[![CI](https://github.com/mdabydeen/metron/actions/workflows/ci.yml/badge.svg)](https://github.com/mdabydeen/metron/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mdabydeen/metron.svg)](https://pkg.go.dev/github.com/mdabydeen/metron)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

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
| Characters per line | 500 | `view_slice` clips longer lines and marks the cut |
| Files listed | 60 | `list_files` truncates and says so |
| Search matches, total | 10 | `search_text` truncates and says so |
| Search matches per file | 2 | ripgrep's `--max-count` |
| Model round-trips per turn | 10 | the agent loop returns `max turns exceeded` |
| Slice retained after a turn | none | history compaction replaces it with a placeholder |
| Messages carried between turns | 60 | oldest exchanges are dropped once a turn completes |

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
go install github.com/mdabydeen/metron/cmd/metron@latest
```

Or build from a clone:

```bash
make build            # -> bin/metron
make install          # build, then sudo cp to /usr/local/bin
go run ./cmd/metron   # run without building
```

`go install` does not stamp a version, so `metron --version` reports `dev` for those builds.
Released binaries and `make build` both carry the real one.

## Usage

Run metron anywhere inside the repository you want it to work on. It finds the enclosing
git work tree and treats that as the project: tools resolve paths against it, and the ctags
index is written to `<project>/.tags` regardless of which subdirectory you started in.
Outside a repository, the working directory is the project.

```bash
cd ~/code/my-project
metron
```

**The project directory is also a boundary.** Every path a tool touches is resolved and
refused if it lands outside — `view_slice` will not read `~/.ssh/id_rsa`, and `apply_patch`
will not write to `../..`. Symlinks are followed before the check, so a link out of the tree
does not get around it. See [SECURITY.md](SECURITY.md).

### One-shot mode

For scripts and CI, `-p` runs a single request and exits. The answer goes to stdout and
everything else to stderr, so the result is pipeable:

```bash
metron -p "which files define Greet?" 2>/dev/null
```

There is nobody to answer the approval prompt in this mode, so `apply_patch` fails closed:
patches are refused unless you pass `--yes` (or set `auto_approve_patches`). `--version` prints
the build stamp; `-h` lists the flags.

Type a request at the prompt. Each line is one turn: metron sends it to the model, runs
whatever tools the model asks for (echoing `[executing: <tool>]` as it goes), and prints the
final answer. Conversation history persists across turns within a session and is never
written to disk.

### Flags

| Flag | Effect |
| --- | --- |
| `-p`, `--prompt` | run one request non-interactively and exit |
| `--yes` | apply patches without asking (required by `-p` to edit files) |
| `--version` | print the version and exit |
| `-h` | list the flags |

### Commands

| Command | Effect |
| --- | --- |
| `/help` | list the commands |
| `/config` | print the effective settings and which file they came from |
| `/reset` | clear the conversation history, keeping the system prompt |
| `/history` | show how many messages and bytes the session is carrying |
| `/tags` | rebuild the ctags index — do this after a big refactor |
| `/exit` | quit (also `exit`, `quit`, `/quit`, or Ctrl-D) |

### A note on `apply_patch`

metron edits your working tree, but not behind your back: it prints the model's diff and waits
for a `y` before touching anything. Anything else — `n`, a bare Return, or EOF — declines, and
the refusal goes back to the model as text so it explains the change instead of retrying.

Patches are also dry-run through `git apply --check` first and rejected as a whole if they do
not apply cleanly, so a bad patch leaves your files untouched. Even so, **work on a clean branch
and commit before you start**, so `git diff` shows you exactly what the model did.

Set `auto_approve_patches` to `true` to skip the prompt.

### Interrupting a reply

A big local model can spend minutes on one answer. Ctrl-C during a reply cancels that turn and
returns you to the prompt with the session intact; Ctrl-C at an idle prompt quits.

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
| `profile` | `"standard"` | budget preset: `tight`, `standard` or `roomy` |
| `provider` | `"ollama"` | wire format: `ollama` or `openai` |
| `api_key_env` | `""` | environment variable holding the API key, if the endpoint wants one |
| `endpoint` | `http://localhost:11434/api/chat` | Ollama chat endpoint, path included |
| `model` | `qwen2.5-coder:32b` | model name to request |
| `timeout_seconds` | `180` | seconds of *silence* before a model call is abandoned |
| `stream` | `true` | print the reply as it is generated |
| `temperature` | `0.1` | sampling temperature |
| `top_p` | `0.95` | nucleus sampling cutoff |
| `num_ctx` | `16384` | context window requested from Ollama |
| `max_turns` | `10` | model round-trips allowed in one user turn |
| `compact_threshold_bytes` | `400` | tool output above this size is purged after the turn |
| `max_prompt_tokens` | `0` | ceiling on prompt tokens for one turn; `0` means none |
| `repo_map_tokens` | `0` | tokens for a project map injected once per session; `0` disables it |
| `max_history_messages` | `60` | messages kept after a turn, excluding the system prompt |
| `max_slice_lines` | `120` | widest span `view_slice` will read |
| `max_line_chars` | `500` | longest single line `view_slice` will emit |
| `auto_approve_patches` | `false` | apply patches without showing the diff and asking |
| `search_max_matches` | `10` | total `search_text` results |
| `search_max_per_file` | `2` | `search_text` results per file |
| `list_max_entries` | `60` | paths `list_files` will return |
| `disabled_tools` | `[]` | tool names to withhold from the model entirely |
| `save_sessions` | `false` | write each conversation to `.metron/sessions` |
| `edit_format` | `"diff"` | how edits are expressed: `diff` (`apply_patch`) or `search_replace` (`edit_file`) |
| `allowed_commands` | `[]` | argv prefixes `run_command` may execute; empty withdraws the tool |
| `command_timeout_seconds` | `120` | wall clock one `run_command` gets |
| `max_command_output_bytes` | `4000` | combined output `run_command` returns |

**Some settings are not accepted from a project file.** `.metron.json` travels with a
repository, and it is the highest-priority config file — so a repository you cloned could
otherwise switch off the approval prompt and switch on `run_command` before the first turn.
`auto_approve_patches`, `allowed_commands`, `save_sessions`, `provider`, `endpoint`,
`api_key_env` and `system_prompt_extra` are honoured only from your user-level config or a
file you name with `METRON_CONFIG` — the last four because a repository that could set them
would point metron at a server it ran and name an environment variable for metron to send as
a bearer token. `model` is not on that list: with the endpoint pinned, it can only choose
among models on your own server. A project file that sets them is
reported on stderr and ignored.

A file that exists but cannot be read or parsed is a startup error, not a silent fallback —
including unknown keys, so a typo like `"modle"` is reported instead of ignored. Values are
validated (positive budgets, `top_p` in `(0, 1]`) before the agent starts.

**The default model is probably not yours.** `qwen2.5-coder:32b` is a placeholder; set `model`
to something you have actually pulled and that reports the `tools` capability.

**`timeout_seconds` bounds silence, not total generation time.** It is an idle watchdog: it
resets every time a chunk arrives, so a reply that takes ten minutes but keeps producing tokens
is never cut off, while a genuinely hung server still gives up after 180 seconds. Raise it only
if your hardware needs longer than that to produce the *first* token.

Setting `stream` to `false` restores the old behaviour — one blocking request, no output until
the reply is complete. The watchdog then covers the whole call, which is where the old
`context deadline exceeded` failure came from, so raise `timeout_seconds` if you turn streaming
off on slow hardware.

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

### Which tools get offered

metron only advertises tools that can actually run. A missing ripgrep withdraws `list_files`
and `search_text`; a BSD `ctags` withdraws `find_symbol`; running outside a git repository
withdraws `apply_patch`; an empty `allowed_commands` withdraws `run_command`. The two edit
tools are alternatives, so `edit_format` selects one and withdraws the other.
`disabled_tools` withdraws whatever you name, and an unknown name there is a startup error
rather than a silent no-op.

This is a budget, not just tidiness. **Tool schemas are sent with every single request**, so
an unusable tool is a tax on every turn — and naming it in the system prompt invites the
model to call it and waste a turn finding out. `/config` reports both the set and its cost:

```
tools: list_files, find_symbol, search_text, view_slice, apply_patch (1150 schema bytes, ~287 tokens per request)
```

On a Mac with no ripgrep and the system ctags, that same line reads:

```
tools: view_slice, apply_patch (493 schema bytes, ~123 tokens per request)
```

If the model calls a tool that was not advertised, the call is refused before it runs, with a
message telling it not to retry.

## The tools

Seven tools, all methods on the `tools.Env` that holds the project root, the budgets, the
command allowlist and the edit format. Five are always on offer; the two edit tools are
alternatives and `edit_format` picks one. This is the complete surface the model has for
touching your code.

### `list_files(pattern)`

Answers "what is here?" — the question the other four tools all assume you already know the
answer to. Runs `rg --files`, optionally narrowed by a glob (`internal/**/*.go`), capped at
`list_max_entries` with a `[truncated to N entries; narrow with a glob]` line.

Because it goes through ripgrep it honours `.gitignore` for free, so build output and vendored
trees never reach the model's context.

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

### `edit_file(path, search, replace)`

The alternative to `apply_patch`, selected with `"edit_format": "search_replace"`.

Unified diffs ask a model for the two things it is worst at: exact line numbers and correct
hunk headers. A small model reads a slice, counts wrong, produces a diff `git apply` rejects,
and then spends the rest of the turn budget adjusting numbers. `edit_file` asks it instead to
quote the lines it already read.

```
edit_file(path="tools/slice.go",
          search="\tif end-start > maxLines {",
          replace="\tif end-start >= maxLines {")
```

The quote is located by matching, so it must occur **exactly once** — two matches is an error
telling the model to quote more context, never a guess. Matching runs a ladder from strict to
forgiving, stopping at the first rung that finds exactly one match: exact, then ignoring
trailing whitespace, then ignoring indentation. A loose match re-indents the replacement to
the file's own indentation, and says which rung it used.

An empty `replace` deletes the matched lines; an empty `search` creates the file. **You still
approve a unified diff** — the format exists to make the model's job easier, not yours.

It also needs no `git`, so on a machine without it this is the only working edit path.

### Budgets for a whole turn

Every other budget bounds one tool. `max_prompt_tokens` bounds the turn:

```
metron > /budget 8000
Per-turn ceiling set to 8000 prompt tokens.
```

Enforcement is predictive, and has to be: a server reports `prompt_eval_count` only once it
has evaluated the prompt, which is *after* the tokens are spent — a ceiling that waited for a
real number could report an overrun but never prevent one. metron estimates from the size of
what it is about to send, and corrects that estimate against every count the server does
report, so it converges on the tokeniser actually in use. `/budget` shows both numbers.

When a turn approaches its ceiling, metron sheds rather than truncates, because cutting a
turn off mid-thought wastes everything it has done:

1. purge file slices already read — the largest and most re-requestable thing in the history;
2. drop the oldest exchanges, leaving the note that says a gap is there;
3. if there is nothing left to shed, stop and say so, with what was learned.

Running out of budget is an outcome, not an error. The exit code stays 0 and the answer
explains itself.

### Profiles

Eleven numbers is too many to tune blind, so `profile` supplies a starting point — `tight`
for a 7B on a laptop, `standard`, `roomy` for a 32B with room to spare. Individual settings
in the same file still win, so a profile is a baseline rather than a lock.

**These are reasoned, not measured.** metron ships a benchmark so that claims like these can
be checked: run `make bench` against your own model and adjust. Presenting a guess as a
finding would be exactly the thing this project exists to argue against.

### Sessions

With `save_sessions` on, each conversation is written to `.metron/sessions/<id>.jsonl` as it
goes, so it survives exiting. `/save` writes one now, `/sessions` lists them, and
`--resume <id>` or `--resume-last` continues one. The directory contains a `.gitignore` of
`*`, so it never shows up as untracked in your project.

It is **off by default**: a transcript contains every tool result the model saw, which is the
contents of every file it read. Transcripts are written `0600`, but they persist. Resuming a
session whose tree has moved since it was saved prints a warning, because the conversation is
full of line numbers and quoted code that may no longer be true.

### One-shot JSON

`metron -p "..." --json` prints exactly one object on stdout and nothing else:

```json
{
  "answer": "Greet now returns hola.",
  "ok": true,
  "tool_calls": 3,
  "tools": [{"name": "view_slice", "ms": 2}, {"name": "edit_file", "ms": 1}],
  "usage": {"prompt": 7341, "generated": 511},
  "files_changed": ["greet.go"]
}
```

`files_changed` is derived from `git status`, so it is true whichever edit format was used,
and true for a file a permitted command wrote as a side effect. A failed run still emits a
valid object with `"ok": false` and an `error`; the exit code carries the verdict.

### `run_command(command)`

Runs one command in the project and returns its exit status and output, so the model can
check its own work instead of asserting it. **Off by default** — with no `allowed_commands`
set, the tool is not offered at all.

There is no shell. The command is split on whitespace and executed directly, so `;`, `&&`,
`|`, redirection and globs are never interpreted; they arrive as literal arguments and the
program rejects them. The allowlist matches whole argv tokens, so `"go test"` permits
`go test ./...` and refuses `go tool`, `gotcha test` and `env go test`.

Each run is asked about at the prompt, killed with its whole process group at
`command_timeout_seconds`, and clipped to `max_command_output_bytes` — keeping the head and
the tail, since that is where a compiler puts the offending file and the summary.

A non-zero exit is data, not an error: "the tests still fail" is exactly what the model
asked to find out.

```json
{ "allowed_commands": ["go test", "go build", "go vet"] }
```

Choose these entries with the care you would give a sudoers file. `"go"` permits
`go run ./anything`; `"make"` permits whatever the Makefile does. See [SECURITY.md](SECURITY.md).

## Using it as a library

The agent loop is importable — `agent`, `tools`, `llm` and the two providers sit outside
`internal/`. See [API.md](API.md) for the smallest useful program and, more importantly, for
what is *not* stable yet and why.

## Architecture

```
cmd/metron/main.go   REPL and commands. No conversation state lives here.
internal/config      Settings: defaults, JSON file, environment, validation.
llm                  The provider-neutral vocabulary the loop speaks.
ollama, openai       The two providers, each translating at its own edge.
agent                The agent loop, the system prompt, tool schemas, compaction, budgets.
tools                The seven tools, the project confinement, the dependency check.
internal/repomap     The ranked project summary injected once per session.
internal/session     Conversation transcripts as JSONL.
```

Seams are interfaces, so each layer can be driven in isolation: `agent.New` takes a `Chatter`
(the one-method subset of `*ollama.Client`), and `run` takes a `stepper` plus a scanner and an
`io.Writer` instead of touching stdin directly.

### The loop

`Agent.Step` appends the user message, then iterates at most `max_turns` times:

1. Call the model with the full history and all five tool schemas.
2. Append the reply to history.
3. If the reply has no tool calls, compact the history and return the reply — done.
4. Otherwise run every tool call in the reply, appending each result as a `role: "tool"`
   message, and go back to step 1.

Exceeding the limit returns a `max turns exceeded` error. Unknown tool names and tool failures
are appended to history *as strings* rather than raised as Go errors: the model sees its own
mistakes and gets a chance to recover.

### Measuring the budget

After every reply metron prints what the turn cost:

```
[1240 prompt / 89 generated tokens · 3 tool calls]
```

The counts come from Ollama's own `prompt_eval_count` and `eval_count`. Once the prompt passes
80% of `num_ctx` a warning follows it, since that is the point at which the next turn starts
losing room rather than merely using it. `/history` shows what the session is carrying and
`/reset` clears it.

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

1. Implement it in `tools` as a method on `Env`, taking its budget from `Env.Budgets`.
2. Add its name to `tools.ToolNames`, and its JSON schema to the package-level `toolDefs` map in `agent/loop.go`.
3. Add a `case` for it in `Agent.dispatch`.

Then extend `TestDispatchRoutesToolsAndReportsErrors` and `TestStepAdvertisesEveryTool`.

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
  directory that becomes the entire `PATH` (see `tools/helpers_test.go`). An empty
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

## Benchmark

`make bench` measures metron over ten seeded repair tasks, across the models and edit formats
in [`bench/matrix.json`](bench/matrix.json), and reports a pass **rate** with median and p95
prompt tokens. Each task ships a `verify.sh` that judges the repository the model left behind
and never the answer text, so a confident sentence cannot pass a task; two of the ten
(`no-such-symbol`, `ambiguous-symbol`) pass only when the model declines to guess. It needs a
live Ollama server and is deliberately not part of `make check`.

See [bench/README.md](bench/README.md) for the task list, the prompt-token ceiling on
`large-file-edit`, and how to add a task.

## Why there is no MCP support

metron does not speak the Model Context Protocol, and this is a decision rather
than a gap.

Every budget in the table at the top of this file is enforced by metron itself:
`view_slice` refuses a wide range, `search_text` truncates and says so, the whole
turn is bounded by `max_prompt_tokens`. An MCP server arrives with a tool surface
metron did not write and cannot bound — schemas of arbitrary size paid for on
every request, and results of arbitrary size returned into the context window.
The one guarantee this program makes is the one thing MCP would take away.

That is not an argument that MCP is bad. It is an argument that "connect to
anything" and "the model never sees more than N tokens" are different products,
and metron is the second one. If you want the first, [several excellent tools
exist](https://modelcontextprotocol.io) and you should use one.

The conditions under which this could change: every server's schemas counted and
shown in `/config`, results clipped through the same byte budgets as native
tools, servers off by default, and a hard cap on advertised tools. If someone
wants to build that, the tool advertisement seam in `agent/loop.go` is where it
would go. Until then, the refusal is a better advertisement for what metron is
than the feature would be.

## Limitations

- No fallback if `rg` or a compatible `ctags` is missing; the affected tool reports the error
  to the model and the startup check warns you.
- `.tags` is built once and never invalidated automatically — use `/tags`.
- Trimming history drops the oldest exchanges silently; the model is not told that earlier
  context is gone.
- Conversation history is lost when you exit.
- The agent cannot run anything, so it cannot check whether its own edit worked.

These are being addressed. See [CHANGELOG.md](CHANGELOG.md) for what has landed.

## Contributing

Bug reports and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first:
metron is small on purpose, and the constraints there are what a PR is judged against.
Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

Security issues go through [SECURITY.md](SECURITY.md), not the public tracker. That file
also documents what metron actually does to your working tree, which is worth reading
before you point it at a repository.

## License

[Apache-2.0](LICENSE). Copyright 2026 Mike Dabydeen.
