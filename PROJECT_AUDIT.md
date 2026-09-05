# Project maturity audit

Reviewed against the repository at `main` (`9986268`) on 2026-09-04. This is a practical
roadmap, not a promise that every possible coding-agent feature belongs in metron. Changes
should continue to satisfy the project's defining constraint: source and tool output are
explicitly bounded.

## What is already mature

- Clear Apache-2.0 licensing, notice, contributing guide, code of conduct, private security
  reporting, issue forms, pull-request template, changelog, and automated dependency updates.
- Hermetic unit tests, integration and live-model tiers, race detection, linting, vetting,
  formatting checks, release-config checks, and a 100% statement-coverage gate.
- Reproducible multi-architecture macOS/Linux releases with checksums and Homebrew delivery.
- Strong default safety: project-root path confinement, symlink checks, patch approval,
  command allowlisting, time/output limits, dependency-aware tool advertisement, and no shell.
- Thoughtful user documentation covering architecture, configuration, operations, failure
  modes, and the token cost of the tool surface.

## Completed in isolated Codex branches

| Priority | Branch | Result |
| --- | --- | --- |
| P0 | `codex/project-config-discovery` | Loads project configuration from the repository root when launched in a subdirectory. |
| P0 | `codex/doctor-command` | Adds a side-effect-free `--doctor` readiness check for config, tools, endpoint, model, and tool capability. |
| P0 | `codex/output-token-budget` | Adds `max_output_tokens`, closing Ollama's otherwise unlimited generation default. |
| P0 | `codex/oss-supply-chain` | Adds CodeQL, vulnerability/dependency review, SPDX SBOMs, provenance attestations, and this audit. |
| P0 | `codex/live-test-project-root` | Makes the live tier target its throwaway repository instead of accidentally inspecting metron itself. |

Each branch is intentionally independent and should be merged (or cherry-picked) separately.
After merges, resolve the small README/changelog conflicts by retaining all entries.

## Valuable work already present on unmerged branches

These branches predate this audit. They are substantial prototypes, not duplicated here:

| Priority | Existing branch | Capability | Review note |
| --- | --- | --- | --- |
| P1 | `claude/search-replace-edit-format` | Anchored search/replace editing | Smaller diffs and fewer model formatting failures; review path and ambiguity handling carefully. |
| P1 | `claude/metron-phase4-openai-provider` | OpenAI-compatible local provider | Important for provider choice; keep credentials opt-in and never print them. |
| P1 | `claude/sessions-and-json-output` | Sessions, machine-readable output, benchmarks, public packages, repo maps, providers | High value but very large; split into reviewable PRs before merging. |
| P1 | `claude/go-symbol-index` | In-process Go symbol discovery | Removes a difficult dependency for Go projects; preserve a cross-language fallback. |
| P1 | `claude/repomap` | Bounded repository map | Good first-turn context if its token budget and invalidation are measurable. |

## Missing product capabilities

1. **Provider abstraction and authentication.** Merge a narrowly scoped OpenAI-compatible
   provider, then add API-key redaction tests and document which endpoints may receive source.
2. **Resumable sessions and structured output.** Essential for automation and long tasks, but
   session files need permissions, size limits, versioning, and explicit retention controls.
3. **A more reliable edit primitive.** Keep `git apply`, but add anchored replacement for small
   local edits so weaker models do not need to synthesize line-numbered unified diffs.
4. **Cross-language symbol discovery.** The Go-native index is useful, but a mature tool needs
   clear adapters or fallbacks for popular languages rather than Universal Ctags as the only path.
5. **Repository-aware initial context.** A bounded, ignore-aware repo map should reduce wasted
   discovery turns; benchmark it against the current minimal prompt before enabling by default.
6. **Windows support.** The code compiles there, but releases and CI currently cover only macOS
   and Linux. Add Windows archives, path/process tests, and installation guidance together.
7. **Non-interactive policy controls.** Add separate flags for permitting patches versus commands;
   `--yes` currently approves both effects when command execution is enabled.
8. **Stable public API boundary.** Most packages are internal. Decide whether embedding is a goal;
   if so, publish a small versioned API and compatibility policy rather than exposing internals.

## Missing project operations

- Cut the first signed/attested release; `v0.1.0` is documented but no tag exists locally.
- Enable branch protection/rulesets for `main`: required reviews, required CI/security checks,
  conversation resolution, no force pushes, and linear history or a documented merge strategy.
- Enable GitHub secret scanning with push protection and private vulnerability reporting.
- Pin remaining GitHub Actions to full commit SHAs and configure Dependabot to update those pins.
- Add macOS and Windows smoke-build jobs. Keep the expensive race/integration suite on Linux.
- Publish an explicit support window for Go/Ollama versions and a deprecation policy before 1.0.
- Define a lightweight maintainer/succession policy once a second regular contributor exists.

## Suggested merge order

1. Project-root configuration and `--doctor` (onboarding correctness).
2. Output-token budget (runtime safety; call out the new config key in release notes).
3. Supply-chain automation (requires GitHub security features to be enabled for best results).
4. Anchored edits and provider abstraction as separate reviewed PRs.
5. Split the sessions/JSON/benchmark branch by capability before reviewing it.
6. Go-native symbols and a measured repo map, followed by broader language adapters.
