package tools

import (
	"fmt"
	"strings"
	"testing"
)

func numberedFile(t *testing.T, lines int) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/sample.go"
	var sb strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	writeFile(t, path, sb.String())
	return path
}

func TestViewSliceReturnsNumberedRange(t *testing.T) {
	path := numberedFile(t, 10)

	got, err := ViewSlice(path, 3, 5, 120, 500)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	want := "    3 | line 3\n    4 | line 4\n    5 | line 5\n"
	if got != want {
		t.Fatalf("ViewSlice() = %q, want %q", got, want)
	}
}

func TestViewSliceClampsToEndOfFile(t *testing.T) {
	path := numberedFile(t, 3)

	got, err := ViewSlice(path, 2, 100, 120, 500)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	want := "    2 | line 2\n    3 | line 3\n"
	if got != want {
		t.Fatalf("ViewSlice() = %q, want %q", got, want)
	}
}

func TestViewSliceStartPastEndOfFileIsEmpty(t *testing.T) {
	path := numberedFile(t, 3)

	got, err := ViewSlice(path, 50, 60, 120, 500)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	if got != "" {
		t.Fatalf("ViewSlice() = %q, want empty output", got)
	}
}

func TestViewSliceRejectsInvertedBounds(t *testing.T) {
	path := numberedFile(t, 10)

	_, err := ViewSlice(path, 9, 2, 120, 500)
	if err == nil || !strings.Contains(err.Error(), "invalid line bounds") {
		t.Fatalf("ViewSlice() error = %v, want an invalid-bounds error", err)
	}
}

func TestViewSliceEnforcesLineBudget(t *testing.T) {
	path := numberedFile(t, 500)

	// end-start == 120 is the largest accepted span (121 numbered lines).
	if _, err := ViewSlice(path, 1, 121, 120, 500); err != nil {
		t.Fatalf("ViewSlice(1, 121) error = %v, want it accepted", err)
	}
	_, err := ViewSlice(path, 1, 122, 120, 500)
	if err == nil || !strings.Contains(err.Error(), "slice too large") {
		t.Fatalf("ViewSlice(1, 122) error = %v, want a budget error", err)
	}
}

func TestViewSliceMissingFile(t *testing.T) {
	if _, err := ViewSlice(t.TempDir()+"/nope.go", 1, 5, 120, 500); err == nil {
		t.Fatal("ViewSlice() = nil error, want a file-open error")
	}
}

func TestViewSliceCleansPath(t *testing.T) {
	path := numberedFile(t, 3)
	dirty := strings.Replace(path, "/sample.go", "/./sub/../sample.go", 1)

	if _, err := ViewSlice(dirty, 1, 3, 120, 500); err != nil {
		t.Fatalf("ViewSlice() error = %v, want the path to be cleaned before opening", err)
	}
}

func TestViewSliceSurfacesScannerErrors(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/huge.txt"
	// A single line past the enlarged scan buffer, not merely past the 64KiB
	// default -- the default is what generated files routinely exceed.
	writeFile(t, path, strings.Repeat("x", maxScanBuffer+1)+"\n")

	_, err := ViewSlice(path, 1, 2, 120, 500)
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("ViewSlice() error = %v, want the scanner error surfaced", err)
	}
}

func TestViewSliceReadsLinesPastTheDefaultScannerLimit(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/generated.js"
	// The case that used to fail outright: one minified line over 64KiB.
	writeFile(t, path, strings.Repeat("x", 128*1024)+"\n")

	got, err := ViewSlice(path, 1, 1, 120, 500)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v, want a long line read rather than refused", err)
	}
	if !strings.Contains(got, truncationMarker) {
		t.Fatalf("ViewSlice() = %q, want the line clipped and marked", got)
	}
}

func TestViewSliceClipsLongLinesToTheBudget(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/wide.go"
	writeFile(t, path, strings.Repeat("ab", 40)+"\n")

	got, err := ViewSlice(path, 1, 1, 120, 10)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	want := "    1 | " + strings.Repeat("ab", 5) + truncationMarker + "\n"
	if got != want {
		t.Fatalf("ViewSlice() = %q, want %q", got, want)
	}
}

func TestViewSliceCountsRunesNotBytes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/utf8.go"
	writeFile(t, path, "ααααα\n")

	got, err := ViewSlice(path, 1, 1, 120, 3)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	if !strings.Contains(got, "ααα"+truncationMarker) {
		t.Fatalf("ViewSlice() = %q, want the clip on a rune boundary", got)
	}
}

func TestViewSliceTreatsANonPositiveBudgetAsUnlimited(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/wide.go"
	line := strings.Repeat("z", 5000)
	writeFile(t, path, line+"\n")

	got, err := ViewSlice(path, 1, 1, 120, 0)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	if !strings.Contains(got, line) || strings.Contains(got, truncationMarker) {
		t.Fatal("ViewSlice() clipped the line, want an unlimited budget to pass it through")
	}
}
