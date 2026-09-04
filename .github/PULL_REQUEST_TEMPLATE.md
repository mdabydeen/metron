## What and why

<!-- The diff says what changed. Say why it needed to. -->

## Checklist

- [ ] `make check` passes (vet, test, race, coverage)
- [ ] Coverage is still 100% — new branches, including error paths, have tests
- [ ] `make test` still needs no Ollama, no ripgrep and no Universal Ctags
- [ ] Any new external binary is faked with a shim, not required from the host
- [ ] New tool output is bounded, and the bound is configurable
- [ ] Tool failure messages are written at the model: what broke, do not retry, what instead
- [ ] `README.md`, `CLAUDE.md` and `metron.example.json` updated if behaviour changed
- [ ] `CHANGELOG.md` updated under Unreleased

## Token cost

<!--
Does this change what a turn costs? Anything that adds to the system prompt or the tool
schemas is paid for on every single model call. Numbers if you have them, an honest
"unchanged" if not.
-->

## Related issues

<!-- Closes #123 -->
