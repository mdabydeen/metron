package tools

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tagsFile is the symbol index, written at the project root rather than in the
// working directory, so running metron from a subdirectory does not scatter
// index files around the tree.
func (e Env) tagsFile() string {
	return filepath.Join(e.Root, ".tags")
}

// EnsureTags builds the ctags index if it is not already there. It is built
// once per project and never invalidated automatically; /tags rebuilds it.
func (e Env) EnsureTags() error {
	if _, err := os.Stat(e.tagsFile()); err == nil {
		return nil
	}
	// --links=no matters: Universal Ctags follows symlinks by default, so a
	// vendor -> /somewhere/private symlink would put paths from outside the
	// project into the index, and FindSymbol reports index paths verbatim.
	cmd := exec.Command("ctags", "-R", "--links=no", "--fields=+nK",
		"--exclude=.git", "--exclude=vendor", "-f", e.tagsFile(), ".")
	cmd.Dir = e.Root
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

// FindSymbol reports where a symbol is defined, by exact name.
func (e Env) FindSymbol(symbol string) (string, error) {
	if err := e.EnsureTags(); err != nil {
		return "", fmt.Errorf("failed generating ctags: %w", err)
	}

	f, err := os.Open(e.tagsFile())
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
			// An index built before --links=no, or one edited by hand, can name
			// a path outside the project. Report only what the other tools
			// would actually let the model read.
			if _, err := e.resolve(parts[1]); err != nil {
				continue
			}
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
