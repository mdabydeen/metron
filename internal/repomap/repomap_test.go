package repomap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers --

// noGit empties PATH for the duration of the test, so `git` cannot be found and
// discovery takes its non-repository path. This is the same trick the tools
// package uses for a missing ripgrep: an empty PATH is the only way to be sure
// the fallback is exercised on a machine that does have git installed.
func noGit(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// requireGit skips a test that needs a real repository. git is used for real
// rather than shimmed, because what is under test is metron's reading of git's
// actual output -- the format of `git log --name-only`, and which paths
// `git ls-files` does and does not report.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// gitInit makes dir a repository with an identity, so commits do not depend on
// the machine's global git configuration.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	requireGit(t)
	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "user.email", "test@example.com")
	run(t, dir, "git", "config", "user.name", "test")
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// withinBudget asserts the invariant the whole package exists to hold, and
// reports the measurement either way so a shrinking margin is visible before it
// becomes an overrun.
func withinBudget(t *testing.T, shape, out string, budgetTokens int) {
	t.Helper()
	limit := budgetTokens * bytesPerToken
	t.Logf("%s: %d bytes of %d (%d tokens at %d bytes/token, %d lines)",
		shape, len(out), limit, budgetTokens, bytesPerToken, strings.Count(out, "\n"))
	if len(out) > limit {
		t.Errorf("%s: %d bytes exceeds the %d-byte budget", shape, len(out), limit)
	}
}

// ------------------------------------------------------------------ Build --

func TestBuildRefusesNonPositiveBudgets(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package a\n\nfunc A() {}\n")
	for _, budget := range []int{0, -1} {
		if out := Build(dir, budget); out != "" {
			t.Errorf("budget %d: want empty, got %q", budget, out)
		}
	}
}

func TestBuildOnMissingRoot(t *testing.T) {
	if out := Build(filepath.Join(t.TempDir(), "absent"), 500); out != "" {
		t.Errorf("want empty for a root that does not exist, got %q", out)
	}
}

func TestBuildOnEmptyDirectory(t *testing.T) {
	noGit(t)
	withinBudget(t, "empty directory", Build(t.TempDir(), 500), 500)
	if out := Build(t.TempDir(), 500); out != "" {
		t.Errorf("want empty map for an empty directory, got %q", out)
	}
}

func TestBuildOnBinaryOnlyTree(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	write(t, dir, "logo.png", "\x89PNG\r\n\x1a\n\x00\x00binary")
	write(t, dir, "data/blob.bin", "\x00\x01\x02\x03")
	write(t, dir, "notes.txt", "plain text")

	out := Build(dir, 500)
	withinBudget(t, "binary-only tree", out, 500)
	for _, want := range []string{"logo.png", "data/blob.bin", "notes.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s listed, got:\n%s", want, out)
		}
	}
	// Non-Go files contribute a path and nothing else: there is no parser for
	// them and guessing at one is how a repo map stops being cheap.
	if strings.Contains(out, "  ") {
		t.Errorf("want bare paths for non-Go files, got:\n%s", out)
	}
}

func TestBuildListsSymbolsAndSaysHowItRanked(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	write(t, dir, "big.go", "package big\n\ntype T struct{}\n\nfunc New() *T { return nil }\n\nfunc helper() {}\n")
	write(t, dir, "small.go", "package small\n\nfunc Only() {}\n")

	out := Build(dir, 500)
	withinBudget(t, "no git history", out, 500)
	if !strings.HasPrefix(out, headerFlat) {
		t.Errorf("want the no-git header, got:\n%s", out)
	}
	if !strings.Contains(out, "big.go  T, New, helper") {
		t.Errorf("want big.go's symbols exported-first, got:\n%s", out)
	}
	// Density is the tie-break when there is no churn to rank by.
	if strings.Index(out, "big.go") > strings.Index(out, "small.go") {
		t.Errorf("want the denser file first, got:\n%s", out)
	}
}

func TestBuildListsTestFilesWithoutExpandingThem(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	write(t, dir, "a.go", "package p\n\nfunc A() {}\n")
	write(t, dir, "a_test.go", "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n")

	out := Build(dir, 500)
	if !strings.Contains(out, "a_test.go") {
		t.Errorf("want the test file listed, got:\n%s", out)
	}
	if strings.Contains(out, "TestA") {
		t.Errorf("want the test file unexpanded, got:\n%s", out)
	}
	// No symbols also means no density, so the source it tests ranks above it.
	if strings.Index(out, "\na.go") > strings.Index(out, "\na_test.go") {
		t.Errorf("want the source file first, got:\n%s", out)
	}
}

func TestBuildRanksByChurn(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	write(t, dir, "cold.go", "package p\n\nfunc Cold() {}\n\ntype Big struct{}\n\ntype Bigger struct{}\n")
	write(t, dir, "hot.go", "package p\n\nfunc Hot() {}\n")
	commit(t, dir, "one")
	for i := range 3 {
		write(t, dir, "hot.go", fmt.Sprintf("package p\n\nfunc Hot() {}\n\n// rev %d\n", i))
		commit(t, dir, fmt.Sprintf("hot %d", i))
	}

	out := Build(dir, 500)
	withinBudget(t, "git repository", out, 500)
	if !strings.HasPrefix(out, headerChurn) {
		t.Errorf("want the churn header, got:\n%s", out)
	}
	// cold.go has more symbols, so only churn can put hot.go first.
	if strings.Index(out, "hot.go") > strings.Index(out, "cold.go") {
		t.Errorf("want the churned file first, got:\n%s", out)
	}
}

func TestBuildInRepositoryWithNoCommits(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	write(t, dir, "new.go", "package p\n\nfunc New() {}\n")

	out := Build(dir, 500)
	withinBudget(t, "repository with no history", out, 500)
	// `git log` fails in a repository with no commits, which must degrade the
	// ranking rather than the map: the untracked file is still listed, via
	// `git ls-files --others`.
	if !strings.HasPrefix(out, headerFlat) || !strings.Contains(out, "new.go  New") {
		t.Errorf("want an unranked map naming the untracked file, got:\n%s", out)
	}
}

func TestBuildHonoursGitignore(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	write(t, dir, ".gitignore", "ignored.go\n")
	write(t, dir, "kept.go", "package p\n\nfunc Kept() {}\n")
	write(t, dir, "ignored.go", "package p\n\nfunc Ignored() {}\n")
	commit(t, dir, "one")

	out := Build(dir, 500)
	if !strings.Contains(out, "kept.go") || strings.Contains(out, "ignored.go") {
		t.Errorf("want the ignored file excluded, got:\n%s", out)
	}
}

func TestBuildSkipsUnwantedDirectoriesAndSymlinks(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	write(t, dir, "keep.go", "package p\n\nfunc Keep() {}\n")
	write(t, dir, "vendor/dep/dep.go", "package dep\n\nfunc Dep() {}\n")
	write(t, dir, "testdata/fixture.go", "package p\n\nfunc Fixture() {}\n")
	write(t, dir, ".metron/session.json", "{}")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	write(t, dir, ".git/config", "[core]\n")

	if runtime.GOOS != "windows" {
		if err := os.Symlink("/etc", filepath.Join(dir, "escape")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	out := Build(dir, 500)
	if !strings.Contains(out, "keep.go") {
		t.Errorf("want keep.go, got:\n%s", out)
	}
	for _, unwanted := range []string{"vendor", "testdata", ".metron", ".git", "escape", "passwd"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("want %s excluded, got:\n%s", unwanted, out)
		}
	}
}

func TestBuildSurvivesUnparseableAndUnreadableFiles(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	write(t, dir, "broken.go", "package p\n\nfunc Broken() { this is not go ((( \n")
	write(t, dir, "fine.go", "package p\n\nfunc Fine() {}\n")

	out := Build(dir, 500)
	if !strings.Contains(out, "fine.go  Fine") || !strings.Contains(out, "broken.go") {
		t.Errorf("want both files present, got:\n%s", out)
	}
}

func TestBuildTruncatesToBudget(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	for i := range 40 {
		write(t, dir, fmt.Sprintf("pkg%02d/file.go", i),
			fmt.Sprintf("package p%d\n\nfunc Alpha%d() {}\n\nfunc Beta%d() {}\n", i, i, i))
	}

	full := Build(dir, 4000)
	tight := Build(dir, 60)
	withinBudget(t, "40 files, generous budget", full, 4000)
	withinBudget(t, "40 files, tight budget", tight, 60)
	if len(tight) >= len(full) {
		t.Errorf("a tighter budget must produce a smaller map: %d vs %d", len(tight), len(full))
	}
	if tight == "" {
		t.Error("want at least one entry to fit in 60 tokens")
	}
}

func TestBuildReturnsNothingWhenNothingFits(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	write(t, dir, "a.go", "package a\n")
	// One token buys three bytes, which does not even cover the header.
	if out := Build(dir, 1); out != "" {
		t.Errorf("want empty when the budget cannot hold a line, got %q", out)
	}
}

func TestBuildOnThisRepository(t *testing.T) {
	requireGit(t)
	root := filepath.Join("..", "..")
	for _, budget := range []int{200, 800, 2000} {
		out := Build(root, budget)
		withinBudget(t, fmt.Sprintf("metron itself at %d tokens", budget), out, budget)
		if out == "" {
			t.Fatalf("want a map of metron at %d tokens", budget)
		}
	}
	t.Logf("metron at 800 tokens:\n%s", Build(root, 800))
}

func TestBuildOnALargeTree(t *testing.T) {
	noGit(t)
	dir := t.TempDir()
	for d := range 30 {
		for f := range 100 {
			write(t, dir, fmt.Sprintf("d%02d/f%03d.go", d, f),
				fmt.Sprintf("package p%d\n\nfunc F%d%d() {}\n\ntype T%d%d struct{}\n", d, d, f, d, f))
		}
	}
	start := time.Now()
	out := Build(dir, 1000)
	elapsed := time.Since(start)

	withinBudget(t, "3,000 files", out, 1000)
	t.Logf("3,000 files mapped in %s", elapsed)
	if elapsed > 10*time.Second {
		t.Errorf("mapping 3,000 files took %s; the work is meant to be bounded", elapsed)
	}
}

// ------------------------------------------------------------- discovery --

func TestDiscoverStopsAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		write(t, dir, fmt.Sprintf("f%d.txt", i), "x")
	}

	t.Run("git", func(t *testing.T) {
		gitInit(t, dir)
		if got := discover(dir, 2); len(got) != 2 {
			t.Errorf("want 2 paths, got %v", got)
		}
	})
	t.Run("walk", func(t *testing.T) {
		noGit(t)
		if got := walkFiles(dir, 2); len(got) != 2 {
			t.Errorf("want 2 paths, got %v", got)
		}
	})
}

func TestWalkSurvivesAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 directory, so there is no error to survive")
	}
	dir := t.TempDir()
	write(t, dir, "keep.txt", "x")
	write(t, dir, "locked/hidden.txt", "x")
	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	got := walkFiles(dir, 100)
	if len(got) != 1 || got[0] != "keep.txt" {
		t.Errorf("want just keep.txt, got %v", got)
	}
}

func TestUsableRejectsWhatItShould(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "real.txt", "x")
	if err := os.MkdirAll(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{"real.txt", true},
		{"", false},
		{filepath.Join(dir, "real.txt"), false}, // absolute
		{"../outside.txt", false},
		{"a/../../outside.txt", false},
		{"vendor/dep.go", false},
		{".GIT/config", false}, // case-insensitive, as macOS filesystems are
		{"absent.txt", false},  // in the index, gone from disk
		{"adir", false},        // not a regular file
	}
	for _, c := range cases {
		if got := usable(dir, c.path); got != c.want {
			t.Errorf("usable(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestChurnCountsOutsideARepository(t *testing.T) {
	noGit(t)
	if counts, ok := churnCounts(t.TempDir()); ok || counts != nil {
		t.Errorf("want no churn without git, got %v, %v", counts, ok)
	}
}

// --------------------------------------------------------------- symbols --

func TestSymbols(t *testing.T) {
	dir := t.TempDir()
	src := `package p

import "fmt"

const Version = "1"

var buf, _ = fmt.Println, 0

type Exported struct{}

type unexported struct{}

func New() *Exported { return nil }

func (e *Exported) String() string { return "" }

func (u unexported) String() string { return "" }

func helper() {}
`
	write(t, dir, "a.go", src)
	got := symbols(filepath.Join(dir, "a.go"))
	want := []string{"Version", "Exported", "New", "String", "buf", "unexported", "helper"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("symbols = %v, want %v", got, want)
	}
}

func TestSymbolsOfABrokenFileAreWhatParsed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package p\n\nfunc Good() {}\n\nfunc Bad( { ]]]\n")
	if got := symbols(filepath.Join(dir, "a.go")); len(got) == 0 || got[0] != "Good" {
		t.Errorf("want the declarations that did parse, got %v", got)
	}
}

func TestSymbolsOfAnUnreadableFile(t *testing.T) {
	// A directory named like a Go file is the portable way to make the parser's
	// own read fail, and it stays a failure when the tests run as root.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notafile.go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := symbols(filepath.Join(dir, "notafile.go")); got != nil {
		t.Errorf("want no symbols, got %v", got)
	}
}

// -------------------------------------------------------------- rendering --

func TestRenderEntryElidesAndRefuses(t *testing.T) {
	e := entry{path: "internal/agent/loop.go", syms: []string{"Agent", "Options", "New", "Step", "dispatch"}}
	cases := []struct {
		width int
		want  string
	}{
		{200, "internal/agent/loop.go  Agent, Options, New, Step, dispatch"},
		{45, "internal/agent/loop.go  Agent (+4 more)"},
		{33, "internal/agent/loop.go (+5 more)"},
		{22, "internal/agent/loop.go"}, // no room even for the marker
		{10, ""},
	}
	for _, c := range cases {
		if got := renderEntry(e, c.width); got != c.want {
			t.Errorf("renderEntry(width %d) = %q, want %q", c.width, got, c.want)
		}
	}
}

func TestRenderSkipsAnOversizedEntryAndKeepsGoing(t *testing.T) {
	long := entry{path: strings.Repeat("x", 300) + ".go"}
	short := entry{path: "b.go"}
	out := render([]entry{long, short}, len(headerFlat)+16, false)
	if out != headerFlat+"\nb.go" {
		t.Errorf("want the entry that fits, got %q", out)
	}
}

func TestRenderStopsWhenTheBudgetIsSpent(t *testing.T) {
	entries := []entry{{path: "a.go"}, {path: "b.go"}, {path: "c.go"}}
	out := render(entries, len(headerFlat)+13, false)
	if out != headerFlat+"\na.go\nb.go" {
		t.Errorf("want two entries, got %q", out)
	}
}

func TestRenderWithoutRoomForAnything(t *testing.T) {
	if out := render([]entry{{path: "a.go"}}, 4, true); out != "" {
		t.Errorf("want empty rather than a lone header, got %q", out)
	}
}
