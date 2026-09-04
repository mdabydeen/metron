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
- `save_sessions` (off by default) writes the conversation to
  `.metron/sessions/`, which includes every tool result the model saw -- that
  is, the contents of every file it read. Transcripts are written 0600 and the
  directory ignores itself in git, but they persist indefinitely and are not
  encrypted. Turn it on knowing that.
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

metron's own patch check reads the paths out of `---`/`+++`, `rename from`/`to`
and `copy from`/`to` headers, and unquotes C-escaped paths first, so an escape
cannot hide inside an octal one. It is still a parser looking at a format git
defines: **treat `git apply` as the real boundary for patches and metron's check
as a second opinion**, not the other way round.

Two directories inside the project are refused outright, to every tool:
`.git` and `.metron`. Being inside the root is not the same as being safe to
write. `.git/config` holds settings git itself executes -- `core.pager`,
`core.sshCommand`, `[alias]` -- so a single edit there runs a command the next
time you type `git log`, and it appears in no diff and in no `git status`.
`git apply` refuses `.git` paths on its own, so `apply_patch` was always
covered; `edit_file` writes files directly and now refuses them too. The check
is case-insensitive, because macOS's default filesystem is.

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
  group, and that group is killed at `command_timeout_seconds` -- so a
  `go test` that spawns a test binary does not outlive its deadline. A process
  that deliberately *leaves* the group (`setsid`) is not reached by the signal;
  what bounds metron in that case is `WaitDelay`, which forces the output pipes
  shut so the call returns rather than blocking on a survivor holding them open.
  The result says "its process group was killed" rather than claiming the
  command is gone, because that is what actually happened. Output is clipped to
  `max_command_output_bytes`.

Choose allowlist entries with the same care you would give a sudoers file, and
be clear-eyed about what narrowing buys you. For any compiler, test runner or
task runner, **every** prefix is arbitrary code execution: metron also writes
the tree, so `"go test"` runs whatever `_test.go` the model just created, and
`"make"` runs whatever target it just added. `"go test"` is not meaningfully
safer than `"go"`.

What actually bounds this is the approval prompt and starting from a clean git
tree, not the narrowness of the prefix. Narrow entries still help -- they make
an unexpected command visible rather than routine -- but do not mistake them for
a sandbox.

**A project cannot grant itself any of this.** (See "Where the model is" for the
rest of what a project file may not set.) `.metron.json` travels with a
repository, and it is the highest-priority config file, so a cloned repository
could otherwise set `auto_approve_patches` and `allowed_commands` and turn
`git clone && metron` into arbitrary code execution before the first turn. Those
settings -- and `save_sessions` -- are honoured only from your user-level config
or from a file you name with `METRON_CONFIG`. A project file that sets them is
reported on stderr and ignored.

**Where the model is.** metron was written for a local Ollama, and most of this
document reasons that way. It also speaks the OpenAI chat-completions format,
which means the endpoint can be anywhere -- and then the model is no longer
something you control.

- **The endpoint is not something a repository may choose.** `provider`,
  `endpoint`, `api_key_env` and `system_prompt_extra` are privileged, alongside
  `auto_approve_patches`, `allowed_commands` and `save_sessions`. A cloned
  repository could otherwise point metron at a server it ran, name an
  environment variable for metron to send as a bearer token, and append its own
  instructions to the system prompt -- so `git clone && metron` would exfiltrate
  a secret and hand the attacker a read primitive over your project through
  tools that never prompt. A project file that sets any of them is reported on
  stderr and ignored. `model` is *not* privileged: with the endpoint pinned to
  your own server, naming a model can only choose among what you installed.
- **A non-local endpoint is announced at startup**, on stderr, with a note when
  it is plain HTTP. If you see that line and did not expect it, stop.
- **An API key is named, not held.** `api_key_env` names an environment variable;
  the key is read at use, never written to a config file, a transcript or
  `--json`. `/config` prints the variable's name.
- **A remote endpoint sees everything metron reads.** That is inherent, not a
  flaw: the tool results are the conversation. Do not point metron at a server
  you do not trust and then at a repository you care about.
- **Replies are bounded.** Response size, assembled reply text, tool-call count
  and tool-call arguments all have ceilings, because the idle watchdog resets on
  every chunk -- a server that streams steadily is never idle, and without limits
  that is an out-of-memory condition it can trigger at will.

**What reaches your terminal.** Everything the model writes is escaped before it
is printed: the streamed reply, the final answer, tool output and the text of
errors carrying a server's own message. Printed raw, that text can clear the
screen, overpaint a diff you have just approved, retitle the window, or write
your clipboard with OSC 52 -- and a comment in a file metron reads is enough to
steer it. Control characters are rendered visibly rather than executed.

**Remaining limitations** -- these are real:

- **Anything the model reads can reach the model's operator.** If you point
  metron at a repository containing secrets, and the model reads them, they are
  in the conversation. With a local Ollama server that conversation does not
  leave your machine; that property is a consequence of your configuration, not
  something metron enforces.
- **Confinement is the project directory, not a sandbox.** Everything inside the
  project is fair game, including files you would rather the model not read.
  There is no per-file policy and no allowlist.
- **Hard links are not detected.** Path confinement resolves symlinks, so a
  symlinked file or directory cannot be used to step outside the project. A
  *hard* link inside the project to a file outside it is invisible to that
  check, and writing through it writes the shared inode. Creating one requires
  `run_command` with something like `ln` permitted, or a link that was already
  there, so it is narrow -- but "every path is confined to the project" should
  be read with this exception in mind.
- **An allowed command is not confined.** `run_command` sets the working
  directory to the project, but the command itself runs with your full user
  privileges and can reach anything you can. Path confinement bounds metron's
  own tools; it cannot bound a program you have permitted to run.
- **What you approve is what applies, but read it carefully.** The diff shown at
  the prompt is written by the model. Control characters in it are escaped
  rather than executed, so it cannot clear your screen and redraw a different
  patch, and an over-long preview is truncated with a count rather than
  scrolling the real hunk out of view. It can still be *misleading* in ways no
  escaping fixes: a plausible-looking change to an unexpected file, a mode
  change that renders as an almost-empty hunk.
- **Prompt injection is not mitigated.** Content in the files metron reads is
  data, but a sufficiently persuasive comment in a source file may influence what
  the model proposes. The approval prompt is the mitigation. Read the diffs.

**Not in scope:** the security of the model you point metron at, the security of your Ollama
server, or model output quality. metron does not sandbox model-proposed changes beyond the
approval prompt, and does not claim to.

## What metron does not do

- No telemetry, no analytics, no crash reporting, no network calls other than to the
  configured model endpoint. Note the wording: metron calls *an endpoint*, and where
  that endpoint is depends on configuration. See "Where the model is" below.
- metron has no account, no login, and stores no credentials of its own. It does
  not follow that nothing sensitive is stored: with `save_sessions` on, whatever
  the model read is written to `.metron/sessions/`.
- No shell. External binaries (`rg`, `ctags`, `git`) are invoked with an explicit argument
  vector, never through `sh -c`, so a model-supplied string cannot become a shell command.
  Model-supplied values are additionally kept out of the flag namespace -- passed after `--`
  or as `--flag=value` -- so a pattern beginning with a dash is data rather than an option.
