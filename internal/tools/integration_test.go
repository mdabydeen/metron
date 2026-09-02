//go:build integration

// These tests exercise the tools against the *real* ripgrep, Universal Ctags
// and git binaries rather than shims. They are excluded from the default
// build because those binaries are not present on every developer machine;
// `make docker-test` provides an image where all three exist.
//
//	go test -tags=integration ./...
package tools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not installed", name)
	}
}

// sampleProject writes a small Go project into a scratch working directory.
func sampleProject(t *testing.T) string {
	t.Helper()
	dir := workdir(t)
	writeFile(t, "greeter.go", `package sample

type Greeter struct {
	Name string
}

func (g Greeter) Greet() string {
	return "hello, " + g.Name
}

func Helper() int { return 42 }
`)
	writeFile(t, "other.go", `package sample

func Unrelated() string { return "hello, world" }
`)
	return dir
}

func TestIntegrationPreflightPassesWithRealBinaries(t *testing.T) {
	requireBinary(t, "rg")
	requireBinary(t, "ctags")
	requireBinary(t, "git")
	initRepo(t)

	if got := Preflight(); len(got) != 0 {
		t.Fatalf("Preflight() = %v, want no warnings in the integration image", got)
	}
}

func TestIntegrationPreflightDetectsANonRepository(t *testing.T) {
	requireBinary(t, "rg")
	requireBinary(t, "ctags")
	requireBinary(t, "git")
	workdir(t)

	// Real git, real non-repository: the shim tests cannot prove that
	// `rev-parse --is-inside-work-tree` actually behaves this way.
	got := Preflight()
	if len(got) != 1 || !strings.Contains(got[0], "not a git repository") {
		t.Fatalf("Preflight() = %v, want the not-a-repository warning", got)
	}
}

// initRepo makes the test's working directory a real git repository, which is
// what apply_patch needs and what Preflight now checks for.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := workdir(t)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func TestIntegrationFindSymbolWithRealCtags(t *testing.T) {
	requireBinary(t, "ctags")
	sampleProject(t)

	got, err := FindSymbol("Greet")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	if !strings.Contains(got, "greeter.go") {
		t.Fatalf("FindSymbol() = %q, want the defining file", got)
	}
	// EnsureTags must have produced a real index with line numbers.
	if !strings.Contains(got, ":7") {
		t.Fatalf("FindSymbol() = %q, want the --fields=+nK line number", got)
	}
	if _, err := os.Stat(".tags"); err != nil {
		t.Fatalf("stat .tags = %v, want the index written", err)
	}
}

func TestIntegrationFindSymbolMissesCleanly(t *testing.T) {
	requireBinary(t, "ctags")
	sampleProject(t)

	got, err := FindSymbol("NoSuchSymbol")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	if !strings.Contains(got, "not found") {
		t.Fatalf("FindSymbol() = %q, want a clean miss", got)
	}
}

func TestIntegrationRebuildTagsWithRealCtags(t *testing.T) {
	requireBinary(t, "ctags")
	sampleProject(t)
	writeFile(t, ".tags", "stale\n")

	if err := RebuildTags(); err != nil {
		t.Fatalf("RebuildTags() error = %v", err)
	}
	b, err := os.ReadFile(".tags")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "stale") {
		t.Fatal("stale index survived RebuildTags")
	}
	if !strings.Contains(string(b), "Greeter") {
		t.Fatalf(".tags = %q, want the project's symbols", b)
	}
}

func TestIntegrationSearchTextWithRealRipgrep(t *testing.T) {
	requireBinary(t, "rg")
	sampleProject(t)

	got, err := SearchText("hello", 10, 2)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	if !strings.Contains(got, "greeter.go") || !strings.Contains(got, "other.go") {
		t.Fatalf("SearchText() = %q, want matches from both files", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, ":") {
			t.Fatalf("line %q is missing the -n line number", line)
		}
	}
}

func TestIntegrationSearchTextHonoursBudgets(t *testing.T) {
	requireBinary(t, "rg")
	workdir(t)
	var many strings.Builder
	for i := 0; i < 50; i++ {
		many.WriteString("needle\n")
	}
	writeFile(t, "a.txt", many.String())
	writeFile(t, "b.txt", many.String())

	got, err := SearchText("needle", 3, 1)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	seen := map[string]bool{}
	for _, line := range lines {
		if strings.HasPrefix(line, "[truncated") {
			continue
		}
		// --max-count=1 means at most one hit per file, so no file repeats.
		file := strings.SplitN(line, ":", 2)[0]
		if seen[file] {
			t.Fatalf("file %q matched more than once, want the per-file budget honoured", file)
		}
		seen[file] = true
	}
	if len(seen) > 3 {
		t.Fatalf("SearchText() returned %d matches, want at most the 3-match budget:\n%s", len(seen), got)
	}
}

func TestIntegrationSearchTextNoMatches(t *testing.T) {
	requireBinary(t, "rg")
	sampleProject(t)

	got, err := SearchText("zzz-not-present-zzz", 10, 2)
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	if got != "No matches found." {
		t.Fatalf("SearchText() = %q, want the no-match message", got)
	}
}

func TestIntegrationSearchTextInvalidRegex(t *testing.T) {
	requireBinary(t, "rg")
	sampleProject(t)

	if _, err := SearchText("(unclosed", 10, 2); err == nil {
		t.Fatal("SearchText() = nil error, want ripgrep to reject the pattern")
	}
}

func TestIntegrationListFilesWithRealRipgrep(t *testing.T) {
	requireBinary(t, "rg")
	sampleProject(t)

	got, err := ListFiles("", 50)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	for _, want := range []string{"greeter.go", "other.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("ListFiles() = %q, missing %q", got, want)
		}
	}
}

func TestIntegrationListFilesHonoursTheGlob(t *testing.T) {
	requireBinary(t, "rg")
	sampleProject(t)
	writeFile(t, "notes.md", "# notes\n")

	got, err := ListFiles("*.md", 50)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if !strings.Contains(got, "notes.md") || strings.Contains(got, "greeter.go") {
		t.Fatalf("ListFiles(*.md) = %q, want only the markdown file", got)
	}
}

func TestIntegrationListFilesRespectsGitignore(t *testing.T) {
	requireBinary(t, "rg")
	requireBinary(t, "git")
	initRepo(t)
	writeFile(t, ".gitignore", "ignored.go\n")
	writeFile(t, "kept.go", "package p\n")
	writeFile(t, "ignored.go", "package p\n")

	got, err := ListFiles("", 50)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	// The point of using `rg --files`: build output stays out of the context
	// window without metron maintaining an exclusion list of its own.
	if !strings.Contains(got, "kept.go") || strings.Contains(got, "ignored.go") {
		t.Fatalf("ListFiles() = %q, want the ignored file excluded", got)
	}
}

func TestIntegrationListFilesHonoursTheBudget(t *testing.T) {
	requireBinary(t, "rg")
	sampleProject(t)
	writeFile(t, "third.go", "package sample\n")

	got, err := ListFiles("", 2)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if !strings.Contains(got, "truncated to 2 entries") {
		t.Fatalf("ListFiles() = %q, want the budget enforced against real ripgrep", got)
	}
}
