# Using metron as a library

metron's agent loop is importable. The packages outside `internal/` are the
public surface:

| Package | What it is |
| --- | --- |
| `agent` | the loop: `New`, `Step`, `Reset`, budgets, tool advertisement |
| `tools` | the tool implementations and the `Env` that bounds them |
| `llm` | the provider-neutral vocabulary the loop speaks |
| `ollama`, `openai` | the two providers |

Everything under `internal/` — configuration loading, session transcripts, the
repo map — is metron's own plumbing and is not part of the contract.

## The smallest useful program

```go
env := tools.NewEnv(tools.DefaultBudgets())

opts := agent.DefaultOptions()
opts.Env = env
opts.MaxPromptTokens = 8000 // bound the whole turn, not just each tool

a := agent.New(ollama.NewClient("http://localhost:11434/api/chat", "qwen2.5-coder:7b", llm.DefaultOptions()), opts)

answer, err := a.Step(context.Background(), "make Greet return hola")
```

`agent.New` takes an `agent.Chatter`, which is one method. A stub implementing
it is how metron's own tests drive the loop without a model, and it is how you
should test yours.

## What you are responsible for

- **Approval.** `Options.Approve` is consulted before anything that causes an
  effect. A `nil` approver proceeds without asking — which is what a scripted
  caller wants and what an interactive one must not do.
- **The root.** `tools.NewEnv` resolves the project root from git, or the
  working directory. Every path a tool touches is confined to it. Set
  `Env.Root` explicitly if you want it somewhere else, and read `SECURITY.md`
  before you do.
- **Execution.** `Env.Allowed` is empty by default, which withdraws
  `run_command` entirely. Populating it grants arbitrary code execution to
  whatever the model proposes; the approval prompt is the only thing between
  the two.

## Stability

**The API is not stable, and will not be before v1.0.0.** Pin an exact version.

This is deliberate rather than an oversight. metron ships a benchmark
(`make bench`) precisely so that its design choices can be checked against
measurements rather than argued about, and several of the shapes here — the
edit-format seam, how budgets degrade, whether the repo map earns its tokens —
are questions that benchmark has not yet been run to answer against a real
model. Freezing the API before those answers exist would mean committing to
whichever guess happened to be written first.

What has to be true before v1.0.0:

1. `make bench` has been run against at least two model sizes, and the numbers
   are published in the README.
2. The defaults in `config.Defaults()` each trace to a benchmark run rather than
   to an intuition.
3. `Options` has stopped growing every release.

Until then, treat this as what it is: a working library whose shape is still
being decided by evidence.
