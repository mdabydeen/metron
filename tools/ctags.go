package tools

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// maxSymbolMatches bounds what find_symbol will print. A name like Err or New
// can have dozens of definitions, and a tool that answers with all of them
// spends the context the rest of the turn needs.
const maxSymbolMatches = 20

// tagsFile is the symbol index, written at the project root rather than in the
// working directory, so running metron from a subdirectory does not scatter
// index files around the tree.
func (e Env) tagsFile() string {
	return filepath.Join(e.Root, ".tags")
}

// tagsExist reports whether a ctags index is already on disk. An index from an
// earlier run keeps working after ctags itself is gone, so this is part of
// deciding whether find_symbol has anything to answer from.
func (e Env) tagsExist() bool {
	_, err := os.Stat(e.tagsFile())
	return err == nil
}

// EnsureTags builds the ctags index if it is not already there. It is built
// once per project and never invalidated automatically; /tags rebuilds it.
func (e Env) EnsureTags() error {
	if e.tagsExist() {
		return nil
	}
	// +e records where each definition ends, which is what lets find_symbol
	// report a span instead of a start line -- the difference between the model
	// slicing the right range and guessing one. It is a recent Universal Ctags
	// field, and an older one rejects the whole invocation over it, so a
	// failure is retried without it before it is called a failure.
	out, err := e.runCtags("+neK")
	if err == nil {
		return nil
	}
	if missing := missingBinary(err, "ctags", "find_symbol",
		"use search_text to locate definitions instead"); missing != nil {
		return missing
	}
	if _, retry := e.runCtags("+nK"); retry == nil {
		return nil
	}
	// A run that failed part way can still have left a stub behind, and a stub
	// on disk would be taken for a usable index by every later call.
	_ = os.Remove(e.tagsFile())
	// The usual cause is BSD ctags, which rejects the field flags outright.
	return fmt.Errorf("ctags failed (is this Universal Ctags?): %v: %s", err, strings.TrimSpace(string(out)))
}

// runCtags indexes the project with the given field set.
//
// --links=no matters: Universal Ctags follows symlinks by default, so a
// vendor -> /somewhere/private symlink would put paths from outside the project
// into the index, and FindSymbol reports index paths verbatim.
func (e Env) runCtags(fields string) ([]byte, error) {
	cmd := exec.Command("ctags", "-R", "--links=no", "--fields="+fields,
		"--exclude=.git", "--exclude=vendor", "-f", e.tagsFile(), ".")
	cmd.Dir = e.Root
	return cmd.CombinedOutput()
}

// FindSymbol reports where a symbol is defined, by exact name.
//
// Two indexes can answer that, and this is where the choice is made: Universal
// Ctags if it can run, metron's built-in Go parser otherwise. The output shape
// is identical either way, so nothing downstream -- and nothing in the model's
// prompt -- has to know which one replied.
func (e Env) FindSymbol(symbol string) (string, error) {
	matches, err := e.symbolMatches(symbol)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return fmt.Sprintf("Symbol '%s' not found.", symbol), nil
	}
	if len(matches) > maxSymbolMatches {
		matches = append(matches[:maxSymbolMatches],
			fmt.Sprintf("... more definitions of '%s' not shown; search_text a longer pattern to narrow it", symbol))
	}
	return strings.Join(matches, "\n"), nil
}

// symbolMatches picks an index and queries it. A ctags index that exists, or a
// ctags that can build one, wins: it covers every language, where the built-in
// index covers Go only. A ctags failure with Go source present is a fallback
// rather than an error, since the fallback answers the same question.
func (e Env) symbolMatches(symbol string) ([]string, error) {
	if e.tagsExist() || ctagsFault() == "" {
		matches, err := e.findInTags(symbol)
		if err == nil {
			return matches, nil
		}
		if !e.hasGoSources() {
			return nil, fmt.Errorf("failed generating ctags: %w", err)
		}
		return e.findGoSymbol(symbol), nil
	}
	if e.hasGoSources() {
		return e.findGoSymbol(symbol), nil
	}
	// Phrased at the model, like every other tool failure: what is wrong, that
	// retrying will not help, and what to do instead.
	return nil, errors.New("no symbol index is available: Universal Ctags is not installed " +
		"and the project has no Go source, so find_symbol cannot answer. Do not retry it; " +
		"use search_text to locate definitions instead")
}

// findInTags greps the ctags index for exact matches on the tag name.
func (e Env) findInTags(symbol string) ([]string, error) {
	if err := e.EnsureTags(); err != nil {
		return nil, err
	}

	f, err := os.Open(e.tagsFile())
	if err != nil {
		return nil, err
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
			lineNo, end, kind := "unknown", "", "sym"
			// Extension fields start after the name, the file and the search
			// pattern, so the scan begins at index 3. Starting at 2 would let
			// the pattern itself be mistaken for a bare kind field.
			var fields []string
			if len(parts) > 3 {
				fields = parts[3:]
			}
			for _, p := range fields {
				lineNo = fieldValue(p, "line:", lineNo)
				end = fieldValue(p, "end:", end)
				kind = fieldValue(p, "kind:", kind)
				// Universal Ctags writes the kind as a bare field -- "func",
				// not "kind:func" -- unless asked otherwise. Only the prefixed
				// form was read, so every real lookup reported "sym" and threw
				// away the one field that tells the model what it found. The
				// test shims emitted the prefixed form, which is exactly why
				// nothing noticed.
				kind = bareKind(p, kind)
			}
			matches = append(matches, formatSymbol(parts[0], kind, parts[1], lineNo, end))
		}
	}
	return matches, nil
}

// fieldValue returns the value of an extended ctags field, or the value already
// held if this is not that field.
func fieldValue(field, prefix, current string) string {
	if strings.HasPrefix(field, prefix) {
		return strings.TrimPrefix(field, prefix)
	}
	return current
}

// formatSymbol renders one match. The span is collapsed to a single number when
// it is one line, or when the index did not record an end -- an older Universal
// Ctags, or a hand-written index. Nothing downstream parses this, but the model
// reads it on every lookup, so it stays as short as it can be.
// bareKind reads an unprefixed kind field. Every other extension field carries
// a "name:" prefix, so an extension field with no colon in it is the kind.
func bareKind(field, current string) string {
	if current != "sym" || field == "" || strings.Contains(field, ":") {
		return current
	}
	return field
}

func formatSymbol(name, kind, path, start, end string) string {
	loc := path + ":" + start
	if end != "" && end != start {
		loc += "-" + end
	}
	return fmt.Sprintf("%s [%s] -> %s", name, kind, loc)
}
