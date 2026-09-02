package tools

import (
	"os"
	"strings"
	"testing"
)

func TestSearchTextReturnsTrimmedMatches(t *testing.T) {
	dir := workdir(t)
	shimDir(t, map[string]string{
		// Record the argv so the context-window budget flags can be asserted.
		"rg": `printf '%s\n' "$*" > ` + dir + `/argv
printf '\n./main.go:12:hit one\n./loop.go:4:hit two\n\n'
`,
	})

	got, err := SearchText("hit", 10, 2)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	want := "./main.go:12:hit one\n./loop.go:4:hit two"
	if got != want {
		t.Fatalf("SearchText() = %q, want %q", got, want)
	}

	argv, err := os.ReadFile(dir + "/argv")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-n", "--max-count=2", "hit", "."} {
		if !strings.Contains(string(argv), flag) {
			t.Errorf("ripgrep argv %q missing %q", argv, flag)
		}
	}
}

func TestSearchTextEnforcesTheOverallBudget(t *testing.T) {
	workdir(t)
	// ripgrep's own --max-count is per file, so five files each returning
	// their allowance can still blow the overall budget. SearchText trims.
	shimDir(t, map[string]string{"rg": `for i in 1 2 3 4 5 6; do echo "./f$i.go:1:hit"; done` + "\n"})

	got, err := SearchText("hit", 3, 2)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("SearchText() returned %d lines, want 3 matches plus a notice:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[3], "truncated to 3 matches") {
		t.Fatalf("last line = %q, want a truncation notice", lines[3])
	}
}

func TestSearchTextKeepsResultsWithinBudget(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "echo './a.go:1:hit'\necho './b.go:2:hit'\n"})

	got, err := SearchText("hit", 10, 2)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	if strings.Contains(got, "truncated") {
		t.Fatalf("SearchText() = %q, want no truncation notice", got)
	}
}

func TestSearchTextTreatsEmptyOutputAsNoMatches(t *testing.T) {
	workdir(t)
	// ripgrep can exit 0 with nothing to say (for example when every match is
	// filtered out); that is a miss, not an empty answer.
	shimDir(t, map[string]string{"rg": "exit 0\n"})

	got, err := SearchText("hit", 10, 2)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	if got != "No matches found." {
		t.Fatalf("SearchText() = %q, want the no-match message", got)
	}
}

func TestSearchTextNoMatches(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "exit 1\n"})

	got, err := SearchText("nothing", 10, 2)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	if got != "No matches found." {
		t.Fatalf("SearchText() = %q, want the no-match message", got)
	}
}

func TestSearchTextReportsRipgrepFailure(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "echo 'regex parse error' >&2\nexit 2\n"})

	_, err := SearchText("(", 10, 2)
	if err == nil || !strings.Contains(err.Error(), "ripgrep error") {
		t.Fatalf("SearchText() error = %v, want a ripgrep error", err)
	}
	if !strings.Contains(err.Error(), "regex parse error") {
		t.Fatalf("SearchText() error = %v, want it to carry ripgrep's own message", err)
	}
}

func TestSearchTextReportsMissingBinary(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	_, err := SearchText("anything", 10, 2)
	if err == nil {
		t.Fatal("SearchText() = nil error, want an error when rg is not on PATH")
	}
	// The message is aimed at the model: name the tool, and stop it retrying.
	for _, want := range []string{"ripgrep (rg) is not installed", "search_text is unavailable",
		"Do not retry", "find_symbol"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("SearchText() error = %v, want it to mention %q", err, want)
		}
	}
}
