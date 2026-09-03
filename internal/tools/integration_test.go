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
	"path/filepath"
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

	got, err := defaultEnv(t).FindSymbol("Greet")
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

	got, err := defaultEnv(t).FindSymbol("NoSuchSymbol")
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

	if err := defaultEnv(t).RebuildTags(); err != nil {
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("hello")
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 3, SearchMaxPerFile: 1}).SearchText("needle")
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

	got, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("zzz-not-present-zzz")
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

	if _, err := testEnv(t, Budgets{SearchMaxMatches: 10, SearchMaxPerFile: 2}).SearchText("(unclosed"); err == nil {
		t.Fatal("SearchText() = nil error, want ripgrep to reject the pattern")
	}
}

func TestIntegrationListFilesWithRealRipgrep(t *testing.T) {
	requireBinary(t, "rg")
	sampleProject(t)

	got, err := testEnv(t, Budgets{ListMaxEntries: 50}).ListFiles("")
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

	got, err := testEnv(t, Budgets{ListMaxEntries: 50}).ListFiles("*.md")
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

	got, err := testEnv(t, Budgets{ListMaxEntries: 50}).ListFiles("")
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

	got, err := testEnv(t, Budgets{ListMaxEntries: 2}).ListFiles("")
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if !strings.Contains(got, "truncated to 2 entries") {
		t.Fatalf("ListFiles() = %q, want the budget enforced against real ripgrep", got)
	}
}

// TestSearchTextTreatsAFlagLikePatternAsText is the case the -- separator
// exists for. It needs real ripgrep: a shim cannot demonstrate how ripgrep's
// own argument parser reads a leading dash.
func TestSearchTextTreatsAFlagLikePatternAsText(t *testing.T) {
	workdir(t)
	writeFile(t, "notes.txt", "the --files flag\nunrelated line\n")
	writeFile(t, "other.txt", "nothing here\n")

	got, err := defaultEnv(t).SearchText("--files")
	if err != nil {
		t.Fatalf("SearchText() error = %v", err)
	}
	// Parsed as a flag, ripgrep would list every file and never mention the
	// line the pattern actually occurs on.
	if !strings.Contains(got, "notes.txt") || !strings.Contains(got, "--files flag") {
		t.Fatalf("SearchText(--files) = %q, want the literal match", got)
	}
	if strings.Contains(got, "other.txt") {
		t.Fatalf("SearchText(--files) = %q, want a search rather than a file listing", got)
	}
}

// TestListFilesTreatsAFlagLikeGlobAsAGlob is the same guarantee for the --glob=
// form: the value cannot be mistaken for a flag of its own.
func TestListFilesTreatsAFlagLikeGlobAsAGlob(t *testing.T) {
	workdir(t)
	writeFile(t, "keep.md", "x\n")
	writeFile(t, "skip.go", "package x\n")

	got, err := defaultEnv(t).ListFiles("*.md")
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if !strings.Contains(got, "keep.md") || strings.Contains(got, "skip.go") {
		t.Fatalf("ListFiles(*.md) = %q, want only the markdown file", got)
	}
}

// TestApplyPatchStillRefusesEscapesWithRealGit checks metron's own boundary
// against the real tool, so a change in git's behaviour cannot quietly widen it.
func TestApplyPatchStillRefusesEscapesWithRealGit(t *testing.T) {
	gitRepo(t)

	got, err := defaultEnv(t).ApplyPatch(
		"--- /dev/null\n+++ b/../../escaped.txt\n@@ -0,0 +1 @@\n+pwned\n")
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if !strings.Contains(got, "Patch rejected") {
		t.Fatalf("ApplyPatch() = %q, want metron's own refusal before git sees it", got)
	}
}

// TestToolsRunAtTheProjectRootFromASubdirectory is the behaviour the Env root
// buys: metron no longer has to be started from the top of the repository.
func TestToolsRunAtTheProjectRootFromASubdirectory(t *testing.T) {
	root := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "pkg", "marker.go"), "package pkg\n\nfunc Marker() {}\n")

	// Build the Env at the top, then descend, as running `metron` in a
	// subdirectory would.
	env := defaultEnv(t)
	t.Chdir(filepath.Join(root, "pkg", "deep"))

	got, err := env.ListFiles("")
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if !strings.Contains(got, "marker.go") {
		t.Fatalf("ListFiles() = %q, want files listed from the project root", got)
	}

	if err := env.RebuildTags(); err != nil {
		t.Fatalf("RebuildTags() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".tags")); err != nil {
		t.Fatalf(".tags not at the project root: %v", err)
	}
	if _, err := os.Stat(".tags"); err == nil {
		t.Fatal(".tags was written into the subdirectory")
	}
}
