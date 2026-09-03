package tools

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// maxScanBuffer is the largest single line the scanner will accept. The default
// bufio.Scanner cap is 64KB, which one minified or generated line exceeds --
// turning a legitimate slice request into "token too long".
const maxScanBuffer = 1 << 20

// truncationMarker ends any line clipped by the per-line budget, so the model
// can tell a shortened line from a complete one.
const truncationMarker = " ...[line truncated]"

// ViewSlice returns lines [start, end] of a file, numbered. The span is bounded
// by MaxSliceLines and each individual line by MaxLineChars, so neither a wide
// range nor a single enormous line can flood the model's context window.
//
// The path is resolved against the project root and refused if it escapes it.
func (e Env) ViewSlice(path string, start, end int) (string, error) {
	// Bounds are checked before any arithmetic on them. end-start on
	// model-supplied numbers overflows: start=-5e18 with end=5e18 wraps to a
	// negative width, sails past the budget check, and reads the whole file --
	// defeating the one guarantee this program exists to make.
	if start < 1 {
		return "", fmt.Errorf("invalid line bounds: start (%d) must be 1 or greater", start)
	}
	if end < start {
		return "", fmt.Errorf("invalid line bounds: end (%d) < start (%d)", end, start)
	}
	if end-start > e.Budgets.MaxSliceLines || end-start < 0 {
		return "", fmt.Errorf("requested slice too large. Limit requests to <= %d lines",
			e.Budgets.MaxSliceLines)
	}

	resolved, err := e.resolve(path)
	if err != nil {
		return "", err
	}

	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	// A header naming the file and range. It costs a few tokens and buys two
	// things: the model can tell two slices apart without tracking which call
	// produced which, and compaction can say what it purged rather than leaving
	// an anonymous placeholder the model cannot act on.
	fmt.Fprintf(&sb, "%s:%d-%d\n", e.rel(resolved), start, end)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScanBuffer)
	lineIdx, shown := 1, 0
	for scanner.Scan() {
		if lineIdx >= start && lineIdx <= end {
			shown++
			fmt.Fprintf(&sb, "%5d | %s\n", lineIdx, clipLine(scanner.Text(), e.Budgets.MaxLineChars))
		}
		if lineIdx > end {
			break
		}
		lineIdx++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if shown == 0 {
		// Saying the file is shorter than the request ends the guess-another-
		// range loop that an empty result invites.
		return fmt.Sprintf("%s:%d-%d\n[no such lines; the file has %d]\n",
			e.rel(resolved), start, end, lineIdx-1), nil
	}
	return sb.String(), nil
}

// clipLine shortens one line to the per-line budget, counting runes rather than
// bytes so multi-byte source is not cut mid-character. A non-positive budget
// means unlimited.
func clipLine(line string, maxLineChars int) string {
	if maxLineChars <= 0 {
		return line
	}
	runes := []rune(line)
	if len(runes) <= maxLineChars {
		return line
	}
	return string(runes[:maxLineChars]) + truncationMarker
}
