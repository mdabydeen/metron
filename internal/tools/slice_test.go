package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func numberedFile(t *testing.T, lines int) string {
	t.Helper()
	dir := workdir(t)
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

	got, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 3, 5)
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

	got, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 2, 100)
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

	got, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 50, 60)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	if got != "" {
		t.Fatalf("ViewSlice() = %q, want empty output", got)
	}
}

func TestViewSliceRejectsInvertedBounds(t *testing.T) {
	path := numberedFile(t, 10)

	_, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 9, 2)
	if err == nil || !strings.Contains(err.Error(), "invalid line bounds") {
		t.Fatalf("ViewSlice() error = %v, want an invalid-bounds error", err)
	}
}

func TestViewSliceEnforcesLineBudget(t *testing.T) {
	path := numberedFile(t, 500)

	// end-start == 120 is the largest accepted span (121 numbered lines).
	if _, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 1, 121); err != nil {
		t.Fatalf("ViewSlice(1, 121) error = %v, want it accepted", err)
	}
	_, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 1, 122)
	if err == nil || !strings.Contains(err.Error(), "slice too large") {
		t.Fatalf("ViewSlice(1, 122) error = %v, want a budget error", err)
	}
}

func TestViewSliceMissingFile(t *testing.T) {
	workdir(t)

	if _, err := defaultEnv(t).ViewSlice("nope.go", 1, 5); err == nil {
		t.Fatal("ViewSlice() = nil error, want a file-open error")
	}
}

func TestViewSliceCleansPath(t *testing.T) {
	path := numberedFile(t, 3)
	dirty := strings.Replace(path, "/sample.go", "/./sub/../sample.go", 1)

	if _, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(dirty, 1, 3); err != nil {
		t.Fatalf("ViewSlice() error = %v, want the path to be cleaned before opening", err)
	}
}

func TestViewSliceSurfacesScannerErrors(t *testing.T) {
	dir := workdir(t)
	path := dir + "/huge.txt"
	// A single line past the enlarged scan buffer, not merely past the 64KiB
	// default -- the default is what generated files routinely exceed.
	writeFile(t, path, strings.Repeat("x", maxScanBuffer+1)+"\n")

	_, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 1, 2)
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("ViewSlice() error = %v, want the scanner error surfaced", err)
	}
}

func TestViewSliceReadsLinesPastTheDefaultScannerLimit(t *testing.T) {
	dir := workdir(t)
	path := dir + "/generated.js"
	// The case that used to fail outright: one minified line over 64KiB.
	writeFile(t, path, strings.Repeat("x", 128*1024)+"\n")

	got, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 500}).ViewSlice(path, 1, 1)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v, want a long line read rather than refused", err)
	}
	if !strings.Contains(got, truncationMarker) {
		t.Fatalf("ViewSlice() = %q, want the line clipped and marked", got)
	}
}

func TestViewSliceClipsLongLinesToTheBudget(t *testing.T) {
	dir := workdir(t)
	path := dir + "/wide.go"
	writeFile(t, path, strings.Repeat("ab", 40)+"\n")

	got, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 10}).ViewSlice(path, 1, 1)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	want := "    1 | " + strings.Repeat("ab", 5) + truncationMarker + "\n"
	if got != want {
		t.Fatalf("ViewSlice() = %q, want %q", got, want)
	}
}

func TestViewSliceCountsRunesNotBytes(t *testing.T) {
	dir := workdir(t)
	path := dir + "/utf8.go"
	writeFile(t, path, "ααααα\n")

	got, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 3}).ViewSlice(path, 1, 1)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	if !strings.Contains(got, "ααα"+truncationMarker) {
		t.Fatalf("ViewSlice() = %q, want the clip on a rune boundary", got)
	}
}

func TestViewSliceTreatsANonPositiveBudgetAsUnlimited(t *testing.T) {
	dir := workdir(t)
	path := dir + "/wide.go"
	line := strings.Repeat("z", 5000)
	writeFile(t, path, line+"\n")

	got, err := testEnv(t, Budgets{MaxSliceLines: 120, MaxLineChars: 0}).ViewSlice(path, 1, 1)
	if err != nil {
		t.Fatalf("ViewSlice() error = %v", err)
	}
	if !strings.Contains(got, line) || strings.Contains(got, truncationMarker) {
		t.Fatal("ViewSlice() clipped the line, want an unlimited budget to pass it through")
	}
}

func TestViewSliceRefusesPathsOutsideTheProject(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	writeFile(t, secret, "PRIVATE KEY\n")

	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	_, err := defaultEnv(t).ViewSlice(secret, 1, 1)
	if err == nil {
		t.Fatal("ViewSlice() read a file outside the project")
	}
	if !strings.Contains(err.Error(), "outside the project") {
		t.Fatalf("ViewSlice() error = %v, want it to say the path is outside the project", err)
	}
}

func TestViewSliceRefusesRelativeEscapes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "secret.txt"), "shh\n")
	project := filepath.Join(dir, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	if _, err := defaultEnv(t).ViewSlice("../secret.txt", 1, 1); err == nil {
		t.Fatal("ViewSlice() followed ../ out of the project")
	}
}

// TestViewSliceRejectsOverflowingBounds is the regression test for a hole that
// defeated the program's entire premise: end-start on model-supplied numbers
// overflowed, wrapping to a negative width that sailed past the budget check
// and read the whole file in one call.
func TestViewSliceRejectsOverflowingBounds(t *testing.T) {
	dir := workdir(t)
	var big strings.Builder
	for i := 0; i < 5000; i++ {
		big.WriteString("line\n")
	}
	writeFile(t, filepath.Join(dir, "big.go"), big.String())
	env := NewEnv(DefaultBudgets())

	for _, tc := range []struct {
		name       string
		start, end int
	}{
		{"overflowing width", -5_000_000_000_000_000_000, 5_000_000_000_000_000_000},
		{"negative start", -10, 5},
		{"zero start", 0, 5},
	} {
		got, err := env.ViewSlice("big.go", tc.start, tc.end)
		if err == nil {
			t.Errorf("%s: ViewSlice() returned %d bytes, want the bounds refused", tc.name, len(got))
		}
	}
}
