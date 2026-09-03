# Security Policy

## Reporting a vulnerability

Report privately through GitHub's [Report a
vulnerability](https://github.com/mdabydeen/metron/security/advisories/new) form, or by
email to <kmike1337@gmail.com>. Please do not open a public issue.

Expect an acknowledgement within a week. metron is maintained by one person as a
side project, so please size your expectations accordingly — but a real vulnerability will
get a real fix.

## Supported versions

Only the latest release. metron is pre-1.0 and there are no maintenance branches.

## What metron actually does

Read this before deciding how much to trust it. metron is a program that **executes changes
proposed by a language model against your working tree**. That is the whole point of it, and
it means the threat model is unusual.

**By design:**

- `apply_patch` writes to files in your working directory via `git apply`.
- Every patch is shown to you as a diff and requires an explicit `y` before it is applied.
  End-of-input answers *no* — an operator who has walked away has not consented.
- `auto_approve_patches: true` in config, and the `--yes` flag, **disable that prompt
  entirely**. Both exist for scripted use. Do not set them on a repository whose contents
  you would mind losing, and prefer running against a clean git tree so that anything
  unwanted is one `git checkout` away.
- `-p/--prompt` without `--yes` fails closed: nobody is at the keyboard to approve, so
  patches are refused rather than applied unattended.

**Known limitations in the current release** — these are real, and are being addressed:

- **No filesystem confinement.** `view_slice` will read any path the process can read,
  including absolute paths outside the repository (`~/.ssh/id_rsa`, `/etc/passwd`), and
  `apply_patch` can be induced to write outside the working tree with a crafted diff. Today
  the approval prompt is the only guard on the write path, and there is no guard on the read
  path. Confinement to the repository root is planned.
- **Anything the model reads can reach the model's operator.** If you point metron at a
  repository containing secrets, and the model reads them, they are in the conversation.
  With a local Ollama server that conversation does not leave your machine; that property is
  a consequence of your configuration, not something metron enforces.
- **Prompt injection is not mitigated.** Content in the files metron reads is data, but a
  sufficiently persuasive comment in a source file may influence what the model proposes.
  The approval prompt is the mitigation. Read the diffs.

**Not in scope:** the security of the model you point metron at, the security of your Ollama
server, or model output quality. metron does not sandbox model-proposed changes beyond the
approval prompt, and does not claim to.

## What metron does not do

- No telemetry, no analytics, no crash reporting, no network calls other than to the
  endpoint you configure.
- No credentials of any kind are read or stored. There is no login and no account.
- No shell. External binaries (`rg`, `ctags`, `git`) are invoked with an explicit argument
  vector, never through `sh -c`, so a model-supplied string cannot become a shell command.
