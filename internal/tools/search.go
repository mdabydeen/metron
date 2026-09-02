package tools

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// SearchText greps the working directory, capped at maxPerFile matches per file
// and maxMatches results overall, to preserve the model's context window.
//
// Only the per-file cap can be delegated to ripgrep: -m and --max-count are the
// same flag, and it is per file, so the overall budget has to be applied here.
func SearchText(pattern string, maxMatches, maxPerFile int) (string, error) {
	cmd := exec.Command("rg", "-n", "--max-count="+strconv.Itoa(maxPerFile), pattern, ".")
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
	if len(lines) > maxMatches {
		lines = lines[:maxMatches]
		lines = append(lines, fmt.Sprintf("[truncated to %d matches; narrow the pattern]", maxMatches))
	}
	return strings.Join(lines, "\n"), nil
}
