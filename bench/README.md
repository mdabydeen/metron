# The metron benchmark

metron's claim is that a budgeted tool surface beats pasting files into a
context window. Nobody in this space publishes numbers, so until now that has
been an assertion. This directory turns it into a measurement.

```bash
make bench                                            # the whole matrix
make bench BENCH_FLAGS="-only large-file-edit -reps 1"
make bench BENCH_FLAGS="-min-pass-rate 0.66"          # use it as a gate
```

`make bench` builds `bin/metron` and runs `cmd/metron-bench`. It needs a live
Ollama server, so it is **not** part of `make check` or `make test`.

## What a run does

For every `(task × model × edit_format)` cell, repeated `repetitions` times:

1. make a scratch directory, copy `tasks/<name>/seed/` into it, `git init`,
   commit;
2. write a `.metron.json` for the cell — model, edit format, and whatever
   `allowed_commands` the task asks for. Everything else stays at metron's
   shipped defaults, because a benchmark that quietly retunes the budgets is
   not measuring the shipped product;
3. run `metron -p "<prompt>" --yes --json` in that directory and parse the
   JSON report from stdout;
4. run the task's `verify.sh` in that directory. **Its exit code is the
   verdict.**

Results go to stdout as a table and to `results/<timestamp>.json`.

## Two rules that shape everything

**The model never judges itself.** `verify.sh` looks at the repository — file
contents, `git diff`, a test it writes itself after the run — and never at the
answer text. A model that says "I have renamed Greet everywhere" and renamed
nothing fails. This is why `find-the-caller` asks for the answer to be written
into the source as a comment: it turns a question into something a script can
check without reading prose.

**A missing model is a skip, not a failure.** Which models are pulled is a
fact about the machine, not about metron — the same posture the `-tags=live`
tests take. The runner asks `/api/tags` once and marks those cells skipped,
with the reason, so an empty row says why instead of leaving a hole.

## The tasks

| Task | What it is really testing |
| --- | --- |
| `rename-symbol` | the basic loop: find, slice, edit, across two files |
| `add-nil-guard` | inserting code rather than replacing a line |
| `fix-off-by-one` | a one-character edit that a whole-file rewrite would pass by accident |
| `add-config-field` | two coordinated edits in one file, struct tag included |
| `find-the-caller` | navigation with decoys that mention the symbol without calling it |
| `make-test-pass` | `run_command`, and not cheating by editing the test |
| `large-file-edit` | the thesis: one line in 3000, under a prompt-token ceiling |
| `ambiguous-symbol` | precision: three `Handle`s, only `beta` may change |
| `no-such-symbol` | honesty: the symbol does not exist, so any edit is a failure |
| `unicode-source` | multi-byte source, compared byte for byte |

The last two matter more than they look. A benchmark that only scores success
on solvable tasks rewards a model that guesses confidently. `no-such-symbol`
passes only when the tree is untouched; `ambiguous-symbol` passes only when
the two decoys are byte-identical to the seed commit.

### The token ceiling

`large-file-edit` sets `max_prompt_tokens` in its `task.json`. `big.go` is
3000 lines and about 100 KB — roughly 25k tokens. The ceiling is 20,000, so a
cell breaches it if its **median** prompt-token count ever approached the cost
of pulling the file into context whole. Passing the edit while breaching the
ceiling is reported as a failure by `-min-pass-rate`, because succeeding that
way is precisely the thing metron exists to prevent.

Median rather than mean: one run that blows the turn budget would otherwise
dominate the number this whole exercise exists to publish. p95 is reported
alongside it by nearest rank, so it names a run that actually happened.

## Adding a task

Create `tasks/<name>/` with `seed/`, `prompt.txt`, `verify.sh` and
`task.json`. `task.json` accepts:

| Field | Meaning |
| --- | --- |
| `name`, `tags` | identity; tags are for slicing the report |
| `timeout_seconds` | bounds the metron invocation |
| `allowed_commands` | argv prefixes written into the cell's `.metron.json` |
| `max_prompt_tokens` | ceiling on the cell's median prompt tokens |
| `expect_no_changes` | cross-check that the run reported no modified files |

Write `verify.sh` so that it **fails on the untouched seed**. A verifier that
passes before the model has done anything measures nothing, and
`TestTaskSuiteIsWellFormed` in `cmd/metron-bench` enforces exactly that (with
`no-such-symbol` as the deliberate exception).

Prefer writing the assertion inside `verify.sh` — a `zz_verify_test.go` heredoc
that runs after the model is done — over shipping it in `seed/`. The model
cannot read a check it never sees, and cannot pass by editing it.
