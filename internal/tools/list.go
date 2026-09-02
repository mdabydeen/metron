package tools

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ListFiles lists the files ripgrep can see, optionally narrowed by a glob, and
// capped at maxEntries. It is the only tool that answers "what is here?" -- the
// other three all require the model to already know a symbol, a pattern or a
// path, which is how it ends up guessing filenames.
//
// `rg --files` honours .gitignore, so build output and vendored trees stay out
// of the model's context for free.
func ListFiles(pattern string, maxEntries int) (string, error) {
	args := []string{"--files"}
	if strings.TrimSpace(pattern) != "" {
		args = append(args, "-g", pattern)
	}
	out, err := exec.Command("rg", args...).CombinedOutput()
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
	if len(lines) > maxEntries {
		lines = lines[:maxEntries]
		lines = append(lines, fmt.Sprintf("[truncated to %d entries; narrow with a glob]", maxEntries))
	}
	return strings.Join(lines, "\n"), nil
}
