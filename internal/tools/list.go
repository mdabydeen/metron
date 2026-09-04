package tools

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ListFiles lists the files ripgrep can see, optionally narrowed by a glob, and
// capped at ListMaxEntries. It is the only tool that answers "what is here?" --
// the other tools all require the model to already know a symbol, a pattern or a
// path, which is how it ends up guessing filenames.
//
// `rg --files` honours .gitignore, so build output and vendored trees stay out
// of the model's context for free.
//
// The glob is passed as --glob=<pattern> rather than as a separate argument, so
// a pattern beginning with a dash is unambiguously a value and can never be
// read as a flag.
func (e Env) ListFiles(pattern string) (string, error) {
	args := []string{"--files"}
	if strings.TrimSpace(pattern) != "" {
		args = append(args, "--glob="+pattern)
	}
	cmd := exec.Command("rg", args...)
	cmd.Dir = e.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "No files found.", nil
		}
		if missing := missingBinary(err, "ripgrep (rg)", "list_files",
			"ask for a specific path with view_slice instead"); missing != nil {
			return "", missing
		}
		return "", fmt.Errorf("ripgrep error: %s", string(out))
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "No files found.", nil
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) > e.Budgets.ListMaxEntries {
		lines = lines[:e.Budgets.ListMaxEntries]
		lines = append(lines, fmt.Sprintf("[truncated to %d entries; narrow with a glob]",
			e.Budgets.ListMaxEntries))
	}
	return strings.Join(lines, "\n"), nil
}
