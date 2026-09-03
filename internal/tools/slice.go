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
	if end < start {
		return "", fmt.Errorf("invalid line bounds: end (%d) < start (%d)", end, start)
	}
	if end-start > e.Budgets.MaxSliceLines {
		return "", fmt.Errorf("requested slice too large (%d lines). Limit requests to <= %d lines",
			end-start, e.Budgets.MaxSliceLines)
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
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScanBuffer)
	lineIdx := 1
	for scanner.Scan() {
		if lineIdx >= start && lineIdx <= end {
			sb.WriteString(fmt.Sprintf("%5d | %s\n", lineIdx, clipLine(scanner.Text(), e.Budgets.MaxLineChars)))
		}
		if lineIdx > end {
			break
		}
		lineIdx++
	}
	if err := scanner.Err(); err != nil {
		return "", err
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
