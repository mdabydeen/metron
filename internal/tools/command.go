package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// elisionMarker replaces the middle of an over-budget output. Both ends are
// kept because that is where the information is: a compiler names the file it
// choked on at the top and summarises at the bottom, and a test runner puts the
// first failure at the top and the count at the bottom.
const elisionMarker = "\n...[%d bytes elided]...\n"

// RunCommand executes one allowed command in the project and reports what it
// did. It is the only tool that can cause an effect metron cannot describe in
// advance, so it is bounded three ways: the operator's allowlist decides what
// may run at all, Approve is asked before it runs, and the output is clipped to
// the configured budget.
//
// There is no shell. The command is split on whitespace and executed directly,
// so ;, &&, |, globs and redirection are inert -- they arrive as literal
// arguments and the program rejects them. That is the whole security model: not
// a blocklist of dangerous characters, but never handing the string to anything
// that would interpret them.
//
// A non-zero exit is data, not an error: "the tests still fail" is exactly what
// the model asked to find out. Nothing here is a Go error either -- a refusal, a
// timeout, a missing binary and a failed start are all things the model should
// read and respond to, so the signature says so by having no error to return.
func (e Env) RunCommand(ctx context.Context, command string) string {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return "No command given. Pass the command to run, for example 'go test ./...'."
	}
	if !e.Allows(argv) {
		return fmt.Sprintf("run_command refused: %q is not permitted here. %s "+
			"Do not retry this command.", command, e.AllowedPhrase())
	}

	timeout := e.Budgets.CommandTimeout
	if timeout < commandTimeoutFloor {
		timeout = commandTimeoutFloor
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = e.Root
	// A command that spawns children -- `go test` builds and runs a test binary
	// -- must die with them. Killing only the process metron started leaves the
	// real work running after the timeout it was supposed to enforce.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }

	out, err := cmd.CombinedOutput()
	body := clipOutput(string(out), e.Budgets.MaxCommandOutputBytes)

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("$ %s\ntimed out after %s and was killed.\n--- output so far ---\n%s",
			command, timeout, body)
	case err != nil:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Sprintf("$ %s\nexit status %d\n--- output ---\n%s",
				command, exitErr.ExitCode(), body)
		}
		if missing := missingBinary(err, argv[0], "run_command",
			"try a command that is installed, or report what you would run"); missing != nil {
			return missing.Error()
		}
		return fmt.Sprintf("$ %s\nfailed to start: %v", command, err)
	default:
		return fmt.Sprintf("$ %s\nexit status 0\n--- output ---\n%s", command, body)
	}
}

// Allows reports whether an argv begins with one of the permitted prefixes.
// Matching whole tokens is what makes the check hard to talk around: "go test"
// admits `go test ./...` but not `go tool ...`, and never `gotcha`, because the
// comparison is per element rather than over the joined string.
func (e Env) Allows(argv []string) bool {
	for _, prefix := range e.Allowed {
		if len(argv) < len(prefix) {
			continue
		}
		matched := true
		for i, want := range prefix {
			if argv[i] != want {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// AllowedPhrase names what the model may run. It appears both in the tool's
// description, so the model does not have to guess, and in a refusal, so a
// rejected call is a redirection rather than a dead end.
func (e Env) AllowedPhrase() string {
	if len(e.Allowed) == 0 {
		return "No commands are permitted in this project."
	}
	quoted := make([]string, 0, len(e.Allowed))
	for _, prefix := range e.Allowed {
		quoted = append(quoted, fmt.Sprintf("%q", strings.Join(prefix, " ")))
	}
	return "Permitted commands start with: " + strings.Join(quoted, ", ") + "."
}

// clipOutput bounds command output, keeping the head and the tail and eliding
// the middle. Cuts are moved back to a rune boundary so a multi-byte character
// is never split, which would otherwise put invalid UTF-8 into the history.
func clipOutput(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	half := max / 2
	head := backToBoundary(s[:half])
	tail := forwardToBoundary(s[len(s)-half:])
	return head + fmt.Sprintf(elisionMarker, len(s)-len(head)-len(tail)) + tail
}

// backToBoundary trims any partial rune from the end of a prefix.
func backToBoundary(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// forwardToBoundary trims any partial rune from the start of a suffix.
func forwardToBoundary(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[1:]
	}
	return s
}

// commandTimeoutFloor stops a zero-valued Budgets from meaning "no time at
// all", which would make every command report a timeout it never had a chance
// to avoid. Configured values are validated separately and are always larger.
const commandTimeoutFloor = time.Second
