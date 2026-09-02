package tools

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func EnsureTags() error {
	if _, err := os.Stat(".tags"); err == nil {
		return nil
	}
	cmd := exec.Command("ctags", "-R", "--fields=+nK", "--exclude=.git", "--exclude=vendor", "-f", ".tags", ".")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if missing := missingBinary(err, "ctags", "find_symbol",
		"use search_text to locate definitions instead"); missing != nil {
		return missing
	}
	// The usual cause is BSD ctags, which rejects --fields=+nK outright.
	return fmt.Errorf("ctags failed (is this Universal Ctags?): %v: %s", err, strings.TrimSpace(string(out)))
}

func FindSymbol(symbol string) (string, error) {
	if err := EnsureTags(); err != nil {
		return "", fmt.Errorf("failed generating ctags: %w", err)
	}

	f, err := os.Open(".tags")
	if err != nil {
		return "", err
	}
	defer f.Close()

	var matches []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "!") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) >= 3 && parts[0] == symbol {
			lineNo, kind := "unknown", "sym"
			for _, p := range parts[2:] {
				if strings.HasPrefix(p, "line:") {
					lineNo = strings.TrimPrefix(p, "line:")
				}
				if strings.HasPrefix(p, "kind:") {
					kind = strings.TrimPrefix(p, "kind:")
				}
			}
			matches = append(matches, fmt.Sprintf("%s [%s] -> %s:%s", parts[0], kind, parts[1], lineNo))
		}
	}

	if len(matches) == 0 {
		return fmt.Sprintf("Symbol '%s' not found.", symbol), nil
	}
	return strings.Join(matches, "\n"), nil
}
