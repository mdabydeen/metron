package tools

import (
	"os"
	"path/filepath"
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("hit")
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 3, SearchMaxPerFile: 2}).SearchText("hit")
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("hit")
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("hit")
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("nothing")
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

	_, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("(")
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

	_, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("anything")
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

func TestSearchTextPassesADashPatternAsAPattern(t *testing.T) {
	workdir(t)
	// Echo the arguments so the argv metron builds is observable.
	shimDir(t, map[string]string{"rg": "echo \"$@\"\n"})

	// Without the -- separator, ripgrep parses this as the --files flag: it
	// would list every file in the repository and silently ignore the match
	// budget, which is the opposite of what search_text is for.
	got, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("--files")
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	if !strings.Contains(got, "-- --files") {
		t.Fatalf("SearchText() invoked rg with %q, want the pattern after a -- separator", got)
	}
}

func TestSearchTextRunsAtTheProjectRoot(t *testing.T) {
	dir := workdir(t)
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	shimDir(t, map[string]string{"rg": "pwd\n"})

	env := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2})
	t.Chdir(sub)

	// The process working directory has moved, but the tool still searches the
	// project it was rooted at.
	got, err := env.SearchText("anything")
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	if strings.TrimSpace(got) != env.Root {
		t.Fatalf("SearchText() ran in %q, want the project root %q", strings.TrimSpace(got), env.Root)
	}
}
