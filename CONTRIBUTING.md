# Contributing to metron

Thanks for looking. metron is small on purpose, and the rules below exist to keep it that
way — most of them are about *not* adding things.

## The one idea

metron's defining constraint is **token discipline**: the model never reads a whole file.
Every look at your code goes through a narrow, budgeted tool, and large tool output is
purged from the conversation once the turn that needed it is over.

A change that widens what the model can see, without a budget on it, is off-thesis even if
it makes some task easier. That is the main reason a PR gets turned down here.

## Ground rules

These hold for every change:

1. **The default test suite stays hermetic.** `make test` must pass with no Ollama server,
   no ripgrep and no Universal Ctags. If your change shells out to a new binary, fake it
   with a shim (see below) — never add a host dependency to the default tier.
2. **Statement coverage stays at 100%.** It is currently 100%, CI enforces it, and it is
   only ever 100% or abandoned. New code needs new tests, including the error branches.
3. **Every tool is output-bounded and configurable.** A tool that can return unbounded text
   is a bug in this codebase. Add the budget to `internal/config`, thread it through
   `agent.Options`, and document it in the README's budget table.
4. **Tool failures are written at the model, not at the user.** See
   `internal/tools/missing.go`: say what broke, say not to retry, and name the alternative.
   This exists because a vague error once made a live model retry `search_text` eight times
   and burn the whole turn budget.
5. **No runtime dependencies without a good reason.** `go.mod` has no `require` block, and
   that is deliberate — it keeps the binary statically linked and cross-compilable. Test-only
   dependencies are held to the same bar.
6. **Docs move with the code.** `README.md`, `CLAUDE.md` and `metron.example.json` are part
   of the PR that changes behaviour, not a cleanup pass afterwards.

## Getting set up

```bash
git clone https://github.com/mdabydeen/metron
cd metron
make check          # vet + test + race + coverage
make vuln           # known-vulnerability scan (requires govulncheck)
```

Runtime (not testing) needs `rg`, Universal Ctags and `git` on PATH, plus a reachable
Ollama server with a tool-capable model. `metron` warns at startup about anything missing.

Install the optional security scanner with
`go install golang.org/x/vuln/cmd/govulncheck@latest`. CI runs the official pinned action,
so contributors do not need it for ordinary local tests.

On macOS, note that the system `ctags` is BSD ctags and does **not** work —
`brew install universal-ctags`, and make sure it comes first on `PATH`.

## The three test tiers

| Tier | Command | Needs |
| --- | --- | --- |
| Hermetic (default) | `make test` | nothing but Go and git |
| Integration | `make docker-test` | real `rg`, Universal Ctags, `git` — supplied by `Dockerfile.test` |
| Live | `make test-live` | a real Ollama server with a tool-capable model |

**Hermetic** is what CI gates on and what you should be running constantly. External
binaries are faked by writing executable shell scripts into a temp directory that becomes
the *entire* `PATH` — see `shimDir` in `internal/tools/helpers_test.go`. An empty shim
directory is how the "binary not installed" branches get covered. Ollama is faked with
`httptest` servers that assert on the request body and return scripted replies.

**Integration** (`-tags=integration`) runs the tools against the real binaries. The Docker
image runs unprivileged on purpose, so the permission-failure cases are exercised rather
than skipped as root.

**Live** (`-tags=live`) drives an actual model, discovering a tool-capable one from
`/api/tags` rather than assuming the default model is installed. Nondeterministic model
behaviour is a **skip**, not a failure; a broken agent loop is a failure. Keep it that way,
or the tier becomes noise everyone ignores.

Filesystem-touching tests run in `t.TempDir()` via `t.Chdir`, since the tools use relative
paths.

## Adding a tool

Tools are the part of metron most likely to attract contributions, and the part where the
constraint matters most. The mechanical steps:

1. Implement it in `internal/tools`, taking its budget as a parameter.
2. Add the budget to `config.Config`, `config.Defaults()` and `Config.Validate()`.
3. Add its JSON schema to the package-level `toolDefs` in `internal/agent/loop.go`.
4. Add a case to `dispatch`.
5. Extend `TestDispatchRoutesToolsAndReportsErrors` and the tool-advertisement test.
6. Update the README budget table, the tool reference section, and `CLAUDE.md`.

Then the part that actually decides whether it lands: **what bounds its output, and what
happens when it fails?** A tool whose answer to the first question is "the file is usually
small" will not be merged.

## Pull requests

- One logical change per PR. The commit body should say *why*, since the diff already says
  what.
- Run `make check` before pushing. `gofmt` is enforced in CI.
- If the change affects how many tokens a turn costs, say so in the description, with
  numbers if you have them.
- Draft PRs are welcome for direction checks before you write the tests.

## Copyright

metron is licensed under Apache-2.0. There is **no CLA**, and there are deliberately **no
per-file copyright headers** — the `LICENSE` and `NOTICE` files at the repository root
cover the whole work. Do not add headers to individual files.

By contributing, you agree that your contribution is licensed under Apache-2.0, per section
5 of the license.

## Reporting bugs

Use the issue template. The two things that make a report actionable are the output of
`/config` and any startup warnings — a surprising number of "`find_symbol` does not work"
reports turn out to be BSD ctags.

Security issues go to [SECURITY.md](SECURITY.md) instead, not the public tracker.
