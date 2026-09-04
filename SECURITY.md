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
- `edit_file` writes to files in your working directory directly, when
  `edit_format` is `search_replace`. It goes through the same path confinement
  and the same approval prompt, and shows you a unified diff.
- Every patch is shown to you as a diff and requires an explicit `y` before it is applied.
  End-of-input answers *no* — an operator who has walked away has not consented.
- `auto_approve_patches: true` in config, and the `--yes` flag, **disable that prompt
  entirely**. Both exist for scripted use. Do not set them on a repository whose contents
  you would mind losing, and prefer running against a clean git tree so that anything
  unwanted is one `git checkout` away.
- `-p/--prompt` without `--yes` fails closed: nobody is at the keyboard to approve, so
  patches are refused rather than applied unattended.
- `run_command` executes commands in your project. It is **off by default**: with no
  `allowed_commands` set, the tool is not offered to the model at all and its schema is
  not even sent. Turning it on is a deliberate act.

**Confinement.** Every path a tool touches is resolved against the project root
-- the enclosing git work tree, or the working directory if there is none -- and
refused if it lands outside. Symlinks are followed before the check, so a
symlinked directory cannot be used to step out of the tree, and that holds for
files being *created* as well as read, since the check resolves the ancestors of
a path that does not exist yet. `view_slice` will not read `~/.ssh/id_rsa`, and
`apply_patch` will not write to `../..`.

For patches this is belt and braces rather than the only guard: `git apply`
already rejects paths containing `..`, refuses to follow a symlinked directory
out of the tree, and treats a leading `/` as relative to the tree rather than to
the filesystem. metron checks anyway, so the boundary is stated in metron's own
terms and survives a future flag or backend that loosens git's.

**Running commands.** `run_command` is the only tool that can cause an effect
metron cannot describe in advance, so it is bounded four ways:

- **There is no shell.** The command is split on whitespace and executed
  directly. `;`, `&&`, `|`, redirection and globs are never interpreted -- they
  arrive as literal arguments and the program rejects them. This is the security
  property the design rests on: not a blocklist of dangerous characters, but
  never handing the string to anything that would interpret them.
- **An allowlist decides what may run at all**, matched on whole argv tokens.
  `"go test"` permits `go test ./...` and refuses `go tool`, `gotcha test`,
  `go --work test` and `env go test`. Matching per element rather than over the
  joined string is what makes it hard to talk around.
- **You are asked before it runs**, with the same prompt apply_patch uses, and
  the same fail-closed behaviour on end-of-input.
- **It is bounded in time and output.** The command runs in its own process
  group and the whole group is killed at `command_timeout_seconds`, so a
  `go test` that spawns a test binary does not outlive its deadline. Output is
  clipped to `max_command_output_bytes`.

Choose allowlist entries with the same care you would give a sudoers file. A
broad entry is a broad grant: `"go"` permits `go run ./anything`, and `"make"`
permits whatever the project's Makefile does. Prefer the narrowest prefix that
does the job.

**Remaining limitations** -- these are real:

- **Anything the model reads can reach the model's operator.** If you point
  metron at a repository containing secrets, and the model reads them, they are
  in the conversation. With a local Ollama server that conversation does not
  leave your machine; that property is a consequence of your configuration, not
  something metron enforces.
- **Confinement is the project directory, not a sandbox.** Everything inside the
  project is fair game, including files you would rather the model not read.
  There is no per-file policy and no allowlist.
- **An allowed command is not confined.** `run_command` sets the working
  directory to the project, but the command itself runs with your full user
  privileges and can reach anything you can. Path confinement bounds metron's
  own tools; it cannot bound a program you have permitted to run.
- **Prompt injection is not mitigated.** Content in the files metron reads is
  data, but a sufficiently persuasive comment in a source file may influence what
  the model proposes. The approval prompt is the mitigation. Read the diffs.

**Not in scope:** the security of the model you point metron at, the security of your Ollama
server, or model output quality. metron does not sandbox model-proposed changes beyond the
approval prompt, and does not claim to.

## What metron does not do

- No telemetry, no analytics, no crash reporting, no network calls other than to the
  endpoint you configure.
- No credentials of any kind are read or stored. There is no login and no account.
- No shell. External binaries (`rg`, `ctags`, `git`) are invoked with an explicit argument
  vector, never through `sh -c`, so a model-supplied string cannot become a shell command.
  Model-supplied values are additionally kept out of the flag namespace -- passed after `--`
  or as `--flag=value` -- so a pattern beginning with a dash is data rather than an option.
