# Metron evaluation brief

Status: public evaluation note; not a pricing page, support promise, or production offer
Last reviewed: 27 September 2026

## What it is

Metron is a local-first terminal coding agent for AI-assisted development. It runs against a local Ollama model, exposes bounded tools, asks before applying patches, and keeps the implementation and release artefacts inspectable.

The public v0.1.0 release includes Darwin and Linux archives for amd64 and arm64, checksums, and an SPDX SBOM. The repository and release notes are the authoritative source for the implementation.

## Who may want to evaluate it

- An engineer comparing local-first coding-agent workflows.
- A platform or engineering lead examining tool boundaries and patch approval.
- An educator teaching how local model capability, tool access, and application authority differ.

The current evidence does not establish adoption, buying demand, production readiness, or a performance advantage.

## What can be inspected now

- The release archive and checksum files.
- `metron --doctor`, which checks the resolved project, configuration, local binaries, Ollama connectivity, configured model, and model tool support without performing inference.
- Bounded file and repository tools.
- Explicit approval before patches are applied.
- Project configuration limits that cannot grant patch auto-approval or allowed commands.
- The public Q&A discussion for bounded reports containing model, hardware, task, and observed issue.

## Suggested evaluation path

1. Read the [README](https://github.com/mdabydeen/metron) and [v0.1.0 release](https://github.com/mdabydeen/metron/releases/tag/v0.1.0).
2. Install a release archive or use the documented source route.
3. Run `metron --doctor` inside a Git repository and inspect the checks before running a task.
4. Start with a no-write request and inspect the tool calls and output.
5. Use a small patch task and review the proposed diff before applying it.
6. Record the model, hardware, task, tool availability, and any patch-approval issue in the [public Q&A discussion](https://github.com/mdabydeen/metron/discussions/21).

## What a team discussion can cover

A facilitated discussion can compare local-first agent boundaries with a team's own review and approval practice, identify one bounded experiment, and list evidence that would be needed before widening access. The existing [workshop interest page](https://michaeldabydeen.com/workshops/ai-assisted-code-review) describes a separate proposed learning session.

No date, availability, booking, payment, customer outcome, or production integration is implied by this brief.

## Explicit boundaries

Metron does not currently claim:

- production readiness, unattended execution, or general compatibility;
- a hosted service, SLA, support contract, or response-time commitment;
- a performance advantage, cost saving, or return on investment;
- enterprise adoption, customer demand, or product-market fit;
- that `--doctor` or a local smoke check proves every model, machine, or workflow will work.

Any future paid software work would require a separate buyer problem, scope, evidence requirements, authorised access, price, and written terms.
