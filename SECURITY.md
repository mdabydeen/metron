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
- **A project's config file is untrusted.** `<repo>/.metron.json` is data the repository ships, so it may tune budgets and the model but may **not** enable `auto_approve_patches` or grant `allowed_commands`. A project file that asks for either is refused with a warning on stderr at startup and in `--doctor`, and the operator can permit a project's `allowed_commands` for a run by setting `METRON_ALLOW_PROJECT_COMMANDS` to `1` (also accepted: `true`, `yes`). The operator's own config -- `~/.metron/config.json` or the file `METRON_CONFIG` names -- is trusted and may set either, as may the `OLLAMA_HOST` and `OLLAMA_MODEL` environment overrides.
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
broad entry is a broad grant: `"go"` permits `go run ./anything` and, through
`go generate`, code the repository itself supplies; `"make"` runs whatever the
project's Makefile says. An allowlisted command is code the repository chose,
so choose entries with the care you would give a sudoers file and prefer the
narrowest prefix that does the job.

**Remaining limitations** -- these are real:

- **Anything the model reads can reach the model's operator.** If you point
  metron at a repository containing secrets, and the model reads them, they are
  in the conversation. With a local Ollama server that conversation does not
  leave your machine; that property is a consequence of your configuration, not
  something metron enforces. metron does guard the path: the endpoint is checked
  to be an http(s) URL with a host before any request is built (so a config
  pointing at `file://`, `ftp://`, or a host-less endpoint cannot be turned into
  an exfiltration channel), and metron warns on stderr whenever the configured
  endpoint is not reached over loopback -- since that is the moment the
  conversation starts leaving the machine, over a transport an attacker may be
  able to read. Every redirect an endpoint issues is revalidated the same way: it
  must retain the original host and scheme, so a different host, an HTTPS-to-HTTP
  downgrade, or any non-http(s) scheme is refused and a misconfigured or hostile
  server cannot send the request somewhere else.
- **Confinement is the project directory, not a sandbox.** Everything inside the
  project is fair game, including files you would rather the model not read.
  There is no per-file policy and no allowlist.
- **An allowed command is not confined.** `run_command` sets the working
  directory to the project, but the command itself runs with your full user
  privileges and can reach anything you can. Path confinement bounds metron's
  own tools; it cannot bound a program you have permitted to run.
- **An allowed command runs with your full environment.** `run_command` inherits
  your process environment as-is, so credentials you export -- a `$GITHUB_TOKEN`,
  `AWS_*` variables, a login shell's `PATH` -- are visible to anything the model is
  permitted to run. Path confinement bounds metron's own tools; it does not scrub
  the environment of a program you allowed.
- **`list_files` hides gitignored paths, but `view_slice` will still read them.**
  `list_files` walks with `rg --files` and so skips what your `.gitignore` excludes;
  `view_slice` reads a named path directly and is not subject to `.gitignore`, so a
  path a project ignores -- a local `.env`, a scratch file -- is readable by name.
- **Path confinement is not atomic.** metron resolves a path and checks that it is
  inside the project, then hands it to `git apply`, and those are two separate steps.
  A symlink could in principle be swapped between the check and the apply -- the
  classic time-of-check-to-time-of-use gap. metron's own check and `git apply --check`
  together make this hard to exploit, but metron does not claim to close it completely.
- **Prompt injection is not mitigated.** Content in the files metron reads is
  data, but a sufficiently persuasive comment in a source file may influence what
  the model proposes. The approval prompt is the mitigation. Read the diffs.

**Not in scope:** the security of the model you point metron at, the security of your Ollama
server, or model output quality. metron does not sandbox model-proposed changes beyond the
approval prompt, and does not claim to.

## What metron does not do

- No telemetry, no analytics, no crash reporting, no network calls other than to the
  endpoint you configure.
- The endpoint a config names is checked to be an http(s) URL with a host before
  any request is built; a `file://`, `ftp://` or host-less endpoint cannot be
  turned into a network call, a non-loopback target is announced on stderr so a
  config that ships the conversation away is not a silent one, and any redirect the
  endpoint issues must retain the original host and scheme.
- No credentials of any kind are read or stored. There is no login and no account.
- No shell. External binaries (`rg`, `ctags`, `git`) are invoked with an explicit argument
  vector, never through `sh -c`, so a model-supplied string cannot become a shell command.
  Model-supplied values are additionally kept out of the flag namespace -- passed after `--`
  or as `--flag=value` -- so a pattern beginning with a dash is data rather than an option.
