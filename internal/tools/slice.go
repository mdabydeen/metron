package tools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ViewSlice returns lines [start, end] of a file, numbered. maxLines bounds
// the span so a single call cannot flood the model's context window.
func ViewSlice(path string, start, end, maxLines int) (string, error) {
	if end < start {
		return "", fmt.Errorf("invalid line bounds: end (%d) < start (%d)", end, start)
	}
	if end-start > maxLines {
		return "", fmt.Errorf("requested slice too large (%d lines). Limit requests to <= %d lines", end-start, maxLines)
	}

	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	lineIdx := 1
	for scanner.Scan() {
		if lineIdx >= start && lineIdx <= end {
			sb.WriteString(fmt.Sprintf("%5d | %s\n", lineIdx, scanner.Text()))
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
