package tools

import (
	"fmt"
	"os"
	"strings"
)

// EditFile replaces one span of a file, located by quoting it rather than by
// numbering it.
//
// This exists because unified diffs ask a model for the two things it is worst
// at: exact line numbers and correct hunk headers. A small model reads a slice,
// counts wrong, and produces a diff git rejects -- then spends the rest of the
// turn budget adjusting the numbers. Quoting a few lines it has already read is
// something the same model does reliably.
//
// The cost of dropping line numbers is that a quote can match in more than one
// place, so ambiguity has to be an error rather than a guess. See locate.
func (e Env) EditFile(path, search, replace string) string {
	resolved, err := e.resolve(path)
	if err != nil {
		return fmt.Sprintf("Edit refused: %v", err)
	}

	original, err := os.ReadFile(resolved)
	switch {
	case os.IsNotExist(err) && search == "":
		// Creating a file is the one case where an empty search is meaningful:
		// there is nothing to anchor to, and nothing to be ambiguous about.
		return e.writeFile(resolved, path, replace, "", true)
	case os.IsNotExist(err):
		return fmt.Sprintf("Edit refused: %s does not exist. To create it, "+
			"send an empty search with the full file contents as replace.", path)
	case err != nil:
		return fmt.Sprintf("Edit refused: %v", err)
	case search == "":
		return fmt.Sprintf("Edit refused: %s already exists, and an empty search "+
			"has nothing to anchor to. Quote the lines you want to replace.", path)
	}

	span, err := locate(string(original), search)
	if err != nil {
		return fmt.Sprintf("Edit failed in %s: %v", path, err)
	}

	updated := span.apply(string(original), replace)
	preview := diffHunk(path, string(original), span, replace)
	if span.strategy != ladder[0].name {
		// Say when the quote did not match verbatim. A loose match is usually
		// right, but "it matched ignoring indentation" is exactly the sentence
		// someone wants to have seen if it turns out to have been wrong.
		preview = fmt.Sprintf("(matched %s)\n%s", span.strategy, preview)
	}
	return e.writeFile(resolved, path, updated, preview, false)
}

// writeFile commits the new contents, preserving the mode of an existing file.
func (e Env) writeFile(resolved, path, contents, preview string, created bool) string {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(resolved); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(resolved, []byte(contents), mode); err != nil {
		return fmt.Sprintf("Edit failed: %v", err)
	}
	if created {
		return fmt.Sprintf("Created %s (%d lines).", path, countLines(contents))
	}
	return fmt.Sprintf("Edited %s.\n%s", path, preview)
}

// span is the half-open line range [start, end) that a search matched, plus how
// it had to be matched to get there.
type span struct {
	start, end int
	strategy   string
	// indent is the leading whitespace the file used where the search block did
	// not, so the replacement can be re-indented to sit correctly.
	indent string
}

// apply splices the replacement into the original at the matched span.
func (s span) apply(original, replace string) string {
	lines := splitLines(original)
	out := make([]string, 0, len(lines))
	out = append(out, lines[:s.start]...)
	if replace != "" {
		for _, line := range splitLines(replace) {
			if line == "" {
				out = append(out, line) // never indent a blank line
				continue
			}
			out = append(out, s.indent+line)
		}
	}
	out = append(out, lines[s.end:]...)
	return strings.Join(out, "\n")
}

// matcher is one rung of the ladder: a way of deciding two lines are "the same"
// that is more forgiving than the rung above it.
type matcher struct {
	name string
	norm func(string) string
}

// ladder runs from strictest to most forgiving. The order is the whole point: a
// forgiving comparison finds more matches, so trying it first would call an
// exact match ambiguous. Each rung is tried in full, and the first one that
// finds exactly one match wins.
var ladder = []matcher{
	{"exact", func(s string) string { return s }},
	{"ignoring trailing whitespace", func(s string) string { return strings.TrimRight(s, " \t") }},
	{"ignoring indentation", strings.TrimSpace},
}

// locate finds the single place the search block occurs. Zero matches and more
// than one match are both failures, reported so the model can fix them: a miss
// gets the nearest thing found, an ambiguity gets a count and the instruction
// to quote more.
func locate(original, search string) (span, error) {
	haystack := splitLines(original)
	needle := splitLines(search)
	// A trailing newline on the search block yields an empty final line that
	// would never match; it is punctuation, not content.
	if n := len(needle); n > 1 && needle[n-1] == "" {
		needle = needle[:n-1]
	}
	if len(needle) == 0 || allBlank(needle) {
		// Whitespace alone is not an anchor: it would match the first blank
		// line in the file, which is never what was meant.
		return span{}, fmt.Errorf("the search block is empty. Quote the actual lines to replace")
	}

	for _, m := range ladder {
		found := matchesUnder(haystack, needle, m)
		switch len(found) {
		case 1:
			return found[0], nil
		case 0:
			continue
		default:
			lines := make([]string, 0, len(found))
			for _, s := range found {
				lines = append(lines, fmt.Sprintf("%d", s.start+1))
			}
			return span{}, fmt.Errorf(
				"the search block matches %d places (lines %s). Quote more surrounding "+
					"lines so it matches exactly one, then retry",
				len(found), strings.Join(lines, ", "))
		}
	}

	return span{}, fmt.Errorf("the search block was not found. %s "+
		"Read the lines again with view_slice and quote them exactly", nearestHint(haystack, needle))
}

// matchesUnder returns every span where the needle occurs under one comparison.
func matchesUnder(haystack, needle []string, m matcher) []span {
	var found []span
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j, want := range needle {
			if m.norm(haystack[i+j]) != m.norm(want) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		found = append(found, span{
			start:    i,
			end:      i + len(needle),
			strategy: m.name,
			indent:   indentDelta(haystack[i], needle[0]),
		})
	}
	return found
}

// indentDelta reports the leading whitespace the file has that the quoted line
// lacks. When a model re-types a block it has read, it commonly loses a level of
// indentation; putting that back is what stops a matched edit from mangling the
// file it just matched in.
func indentDelta(fileLine, searchLine string) string {
	fileIndent := leadingSpace(fileLine)
	searchIndent := leadingSpace(searchLine)
	if strings.HasSuffix(fileIndent, searchIndent) {
		return strings.TrimSuffix(fileIndent, searchIndent)
	}
	return ""
}

// allBlank reports whether every line is empty or whitespace.
func allBlank(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return true
}

func leadingSpace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// nearestHint names the closest line to the start of the search block, so a
// failed match points somewhere instead of just saying no. It is a hint, not a
// diagnosis: the model still has to go and read.
func nearestHint(haystack, needle []string) string {
	want := strings.TrimSpace(needle[0])
	if want == "" {
		return ""
	}
	for i, line := range haystack {
		if trimmed := strings.TrimSpace(line); trimmed != "" && strings.Contains(trimmed, want) {
			return fmt.Sprintf("The closest line is %d: %q.", i+1, trimmed)
		}
	}
	return ""
}

// diffContext is how many unchanged lines to show either side of an edit. Three
// is what unified diffs use, and the operator is reading this to decide whether
// to allow the change.
const diffContext = 3

// diffHunk renders the edit as a unified diff for the approval prompt. No diff
// algorithm is needed: the replaced span is known exactly, so the hunk can be
// written directly. An operator should never be asked to approve a change in a
// notation invented for the model's convenience.
func diffHunk(path, original string, s span, replace string) string {
	lines := splitLines(original)
	var replaced []string
	if replace != "" {
		for _, line := range splitLines(replace) {
			if line == "" {
				replaced = append(replaced, line)
				continue
			}
			replaced = append(replaced, s.indent+line)
		}
	}

	from := max(0, s.start-diffContext)
	to := min(len(lines), s.end+diffContext)

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n",
		from+1, to-from,
		from+1, to-from-(s.end-s.start)+len(replaced))
	for _, line := range lines[from:s.start] {
		fmt.Fprintf(&b, " %s\n", line)
	}
	for _, line := range lines[s.start:s.end] {
		fmt.Fprintf(&b, "-%s\n", line)
	}
	for _, line := range replaced {
		fmt.Fprintf(&b, "+%s\n", line)
	}
	for _, line := range lines[s.end:to] {
		fmt.Fprintf(&b, " %s\n", line)
	}
	return b.String()
}

// EditPreview renders what an edit would do, for the approval prompt, without
// touching the file. It returns the reason instead when the edit could not be
// located -- the operator is not asked to approve something that will not apply.
func (e Env) EditPreview(path, search, replace string) string {
	resolved, err := e.resolve(path)
	if err != nil {
		return fmt.Sprintf("%s (this will be refused: %v)", path, err)
	}
	original, err := os.ReadFile(resolved)
	if os.IsNotExist(err) && search == "" {
		return fmt.Sprintf("create %s (%d lines)", path, countLines(replace))
	}
	if err != nil {
		return fmt.Sprintf("%s (this will be refused: %v)", path, err)
	}
	s, err := locate(string(original), search)
	if err != nil {
		return fmt.Sprintf("%s (this will fail: %v)", path, err)
	}
	return diffHunk(path, string(original), s, replace)
}

// splitLines splits on newlines without inventing a trailing empty line for
// content that ends in one, so a round trip through Join is lossless.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}
