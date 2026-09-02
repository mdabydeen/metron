package tools

import (
	"strings"
	"testing"
)

func TestListFilesReturnsTrimmedPaths(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "printf 'main.go\\ninternal/a.go\\n\\n'\n"})

	got, err := ListFiles("", 10)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if got != "main.go\ninternal/a.go" {
		t.Fatalf("ListFiles() = %q, want the paths with trailing blank lines removed", got)
	}
}

func TestListFilesPassesTheGlobToRipgrep(t *testing.T) {
	workdir(t)
	// Echo the arguments so the flags metron actually sends are observable.
	shimDir(t, map[string]string{"rg": "echo \"$@\"\n"})

	got, err := ListFiles("internal/**/*.go", 10)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if !strings.Contains(got, "--files") || !strings.Contains(got, "-g internal/**/*.go") {
		t.Fatalf("ListFiles() invoked rg with %q, want --files and the glob", got)
	}
}

func TestListFilesOmitsTheGlobWhenBlank(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "echo \"$@\"\n"})

	got, err := ListFiles("   ", 10)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if strings.Contains(got, "-g") {
		t.Fatalf("ListFiles() invoked rg with %q, want no glob flag for a blank pattern", got)
	}
}

func TestListFilesEnforcesTheEntryBudget(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "for i in 1 2 3 4 5; do echo \"f$i.go\"; done\n"})

	got, err := ListFiles("", 3)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("ListFiles() = %q, want 3 entries plus the truncation notice", got)
	}
	if !strings.Contains(lines[3], "truncated to 3 entries") {
		t.Fatalf("ListFiles() = %q, want the truncation announced", got)
	}
}

func TestListFilesKeepsResultsWithinBudget(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "echo 'only.go'\n"})

	got, err := ListFiles("", 5)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if strings.Contains(got, "truncated") {
		t.Fatalf("ListFiles() = %q, want no truncation notice under budget", got)
	}
}

func TestListFilesNoMatches(t *testing.T) {
	workdir(t)
	// ripgrep exits 1 when nothing matches, which is not a failure.
	shimDir(t, map[string]string{"rg": "exit 1\n"})

	got, err := ListFiles("*.rs", 10)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if got != "No files found." {
		t.Fatalf("ListFiles() = %q, want the empty-result message", got)
	}
}

func TestListFilesTreatsEmptyOutputAsNoMatches(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "printf '   \\n'\nexit 0\n"})

	got, err := ListFiles("", 10)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if got != "No files found." {
		t.Fatalf("ListFiles() = %q, want the empty-result message", got)
	}
}

func TestListFilesReportsRipgrepFailure(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"rg": "echo 'bad glob' >&2\nexit 2\n"})

	_, err := ListFiles("[", 10)
	if err == nil || !strings.Contains(err.Error(), "ripgrep error") {
		t.Fatalf("ListFiles() error = %v, want the ripgrep failure surfaced", err)
	}
}

func TestListFilesReportsMissingBinary(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	_, err := ListFiles("", 10)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("ListFiles() error = %v, want the missing-binary guidance", err)
	}
	if !strings.Contains(err.Error(), "Do not retry") {
		t.Fatalf("ListFiles() error = %v, want the model told not to retry", err)
	}
}
