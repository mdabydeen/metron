package tools

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// SearchText greps the project, capped at SearchMaxPerFile matches per file and
// SearchMaxMatches results overall, to preserve the model's context window.
//
// Only the per-file cap can be delegated to ripgrep: -m and --max-count are the
// same flag, and it is per file, so the overall budget has to be applied here.
//
// The pattern is passed after a `--` separator. Without it a model-supplied
// pattern beginning with a dash -- "--files", say -- is parsed by ripgrep as a
// flag, which silently changes what the tool does and bypasses the match
// budget entirely.
func (e Env) SearchText(pattern string) (string, error) {
	cmd := exec.Command("rg", "-n", "--max-count="+strconv.Itoa(e.Budgets.SearchMaxPerFile),
		"--", pattern, ".")
	cmd.Dir = e.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "No matches found.", nil
		}
		if missing := missingBinary(err, "ripgrep (rg)", "search_text",
			"use find_symbol to locate definitions instead"); missing != nil {
			return "", missing
		}
		return "", fmt.Errorf("ripgrep error: %s", string(out))
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "No matches found.", nil
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) > e.Budgets.SearchMaxMatches {
		lines = lines[:e.Budgets.SearchMaxMatches]
		lines = append(lines, fmt.Sprintf("[truncated to %d matches; narrow the pattern]",
			e.Budgets.SearchMaxMatches))
	}
	return strings.Join(lines, "\n"), nil
}
