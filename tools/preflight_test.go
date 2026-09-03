package tools

import (
	"os"
	"strings"
	"testing"
)

const universalCtagsShim = `case "$1" in
  --version) echo "Universal Ctags 6.1.0, Copyright (C) 2015-2023"; exit 0;;
esac
while [ $# -gt 0 ]; do
  if [ "$1" = "-f" ]; then shift; out="$1"; fi
  shift
done
printf 'Sym\tmain.go\t/^Sym$/;"\tkind:func\tline:3\n' > "$out"
`

// gitRepoShim stands in for a git that reports the working directory is inside
// a repository, which is what apply_patch needs to be usable.
const gitRepoShim = `case "$1 $2" in
  "rev-parse --is-inside-work-tree") echo true; exit 0;;
esac
exit 0
`

func TestPreflightAllDependenciesPresent(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{
		"rg":    "exit 0\n",
		"git":   gitRepoShim,
		"ctags": universalCtagsShim,
	})

	if got := defaultEnv(t).Preflight(); len(got) != 0 {
		t.Fatalf("Preflight() = %v, want no warnings", got)
	}
}

func TestPreflightReportsMissingBinaries(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	got := defaultEnv(t).Preflight()
	if len(got) != 3 {
		t.Fatalf("Preflight() = %v, want one warning per dependency", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"rg not found on PATH - list_files, search_text are unavailable",
		"ctags not found on PATH - find_symbol is unavailable",
		"git not found on PATH - apply_patch is unavailable",
		"install ripgrep",
		"universal-ctags",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Preflight() = %q, missing %q", joined, want)
		}
	}
}

func TestPreflightRejectsBSDCtags(t *testing.T) {
	workdir(t)
	// macOS ctags exits non-zero on --version and never says "Universal".
	shimDir(t, map[string]string{
		"rg":    "exit 0\n",
		"git":   gitRepoShim,
		"ctags": "echo 'ctags: illegal option -- -' >&2\nexit 1\n",
	})

	got := defaultEnv(t).Preflight()
	if len(got) != 1 || !strings.Contains(got[0], "not Universal Ctags") {
		t.Fatalf("Preflight() = %v, want exactly the BSD ctags warning", got)
	}
}

func TestPreflightRejectsCtagsThatIsNotUniversal(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{
		"rg":    "exit 0\n",
		"git":   gitRepoShim,
		"ctags": "echo 'Exuberant Ctags 5.8'\nexit 0\n",
	})

	got := defaultEnv(t).Preflight()
	if len(got) != 1 || !strings.Contains(got[0], "not Universal Ctags") {
		t.Fatalf("Preflight() = %v, want the non-Universal warning", got)
	}
}

func TestPreflightWarnsOutsideAGitRepository(t *testing.T) {
	workdir(t)
	// git is installed but reports we are not inside a work tree, which is the
	// state of any directory the operator started metron in by mistake.
	shimDir(t, map[string]string{
		"rg":    "exit 0\n",
		"git":   "exit 1\n",
		"ctags": universalCtagsShim,
	})

	got := defaultEnv(t).Preflight()
	if len(got) != 1 || !strings.Contains(got[0], "not a git repository") {
		t.Fatalf("Preflight() = %v, want the not-a-repository warning", got)
	}
	if !strings.Contains(got[0], "apply_patch is unavailable") {
		t.Fatalf("Preflight() = %v, want the warning to name the affected tool", got)
	}
}

func TestPreflightWarnsWhenGitDeniesTheWorkTree(t *testing.T) {
	workdir(t)
	// git succeeds but answers "false" -- e.g. a bare repo or $GIT_DIR games.
	shimDir(t, map[string]string{
		"rg":    "exit 0\n",
		"git":   "echo false\nexit 0\n",
		"ctags": universalCtagsShim,
	})

	if got := defaultEnv(t).Preflight(); len(got) != 1 || !strings.Contains(got[0], "not a git repository") {
		t.Fatalf("Preflight() = %v, want the not-a-repository warning", got)
	}
}

func TestRebuildTagsReplacesStaleIndex(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", "stale content\n")
	shimDir(t, map[string]string{"ctags": universalCtagsShim})

	if err := defaultEnv(t).RebuildTags(); err != nil {
		t.Fatalf("RebuildTags() error = %v", err)
	}
	b, err := os.ReadFile(".tags")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "stale") || !strings.Contains(string(b), "Sym") {
		t.Fatalf(".tags = %q, want a freshly generated index", b)
	}
}

func TestRebuildTagsWithNoExistingIndex(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"ctags": universalCtagsShim})

	if err := defaultEnv(t).RebuildTags(); err != nil {
		t.Fatalf("RebuildTags() error = %v, want a missing index to be fine", err)
	}
	if _, err := os.Stat(".tags"); err != nil {
		t.Fatalf("stat .tags = %v, want it created", err)
	}
}

func TestRebuildTagsReportsGenerationFailure(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", "stale\n")
	shimDir(t, nil)

	err := defaultEnv(t).RebuildTags()
	if err == nil || !strings.Contains(err.Error(), "rebuild ctags index") {
		t.Fatalf("RebuildTags() error = %v, want a rebuild failure", err)
	}
}

func TestRebuildTagsReportsUndeletableIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := workdir(t)
	writeFile(t, ".tags", "stale\n")
	// A read-only directory blocks the unlink but not the stat.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := defaultEnv(t).RebuildTags()
	if err == nil || !strings.Contains(err.Error(), "remove stale index") {
		t.Fatalf("RebuildTags() error = %v, want the removal failure reported", err)
	}
}

func TestUnavailableToolsNamesEveryAffectedTool(t *testing.T) {
	workdir(t)
	shimDir(t, nil) // nothing on PATH at all

	got := defaultEnv(t).UnavailableTools()

	// ripgrep backs two tools, so a single missing binary must disable both.
	for _, name := range []string{ToolListFiles, ToolSearchText, ToolFindSymbol, ToolApplyPatch} {
		if _, ok := got[name]; !ok {
			t.Errorf("UnavailableTools() = %v, want %q reported unavailable", got, name)
		}
	}
	// view_slice reads files directly and has no external dependency.
	if _, ok := got[ToolViewSlice]; ok {
		t.Errorf("UnavailableTools() = %v, want view_slice always available", got)
	}
	if !strings.Contains(got[ToolListFiles], "install ripgrep") {
		t.Errorf("UnavailableTools()[list_files] = %q, want the operator hint included", got[ToolListFiles])
	}
}

func TestUnavailableToolsIsEmptyWhenEverythingIsPresent(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{
		"rg":    "exit 0\n",
		"git":   gitRepoShim,
		"ctags": universalCtagsShim,
	})

	env := defaultEnv(t)
	// run_command needs permission rather than a binary, so it is only
	// available once the operator has allowed something.
	env.Allowed = ParseAllowlist([]string{"go test"})

	// The two edit tools are alternatives, so one of them is always withheld;
	// everything else should be available.
	got := env.UnavailableTools()
	delete(got, ToolEditFile)
	if len(got) != 0 {
		t.Fatalf("UnavailableTools() = %v, want nothing else unavailable", got)
	}
}

func TestUnavailableToolsWithholdsTheUnselectedEditTool(t *testing.T) {
	workdir(t)

	for _, tc := range []struct{ format, withheld, offered string }{
		{FormatDiff, ToolEditFile, "apply_patch"},
		{FormatSearchReplace, ToolApplyPatch, "edit_file"},
	} {
		env := NewEnv(DefaultBudgets())
		env.EditFormat = tc.format

		got := env.UnavailableTools()
		reason, withheld := got[tc.withheld]
		if !withheld {
			t.Errorf("edit_format %q: %s was not withheld", tc.format, tc.withheld)
			continue
		}
		// The refusal has to name the tool that does work, or the model has
		// nowhere to go.
		if !strings.Contains(reason, tc.offered) {
			t.Errorf("edit_format %q: reason %q does not name %s", tc.format, reason, tc.offered)
		}
	}
}

func TestUnavailableToolsDefaultsToDiffForAZeroEnv(t *testing.T) {
	workdir(t)

	got := (Env{}).UnavailableTools()

	// An empty EditFormat must mean diff, so edit_file is the one withheld.
	// apply_patch may still be withheld here for wanting a git repository --
	// a different reason, and not the one under test.
	if _, withheld := got[ToolEditFile]; !withheld {
		t.Fatalf("UnavailableTools() = %v, want edit_file withheld for a zero Env", got)
	}
	if reason := got[ToolApplyPatch]; strings.Contains(reason, "edit_format") {
		t.Fatalf("apply_patch withheld as %q; an empty EditFormat should mean diff", reason)
	}
}

// bsdCtagsShim stands in for the ctags Apple ships: on PATH, failing the
// version check, and rejecting the field flags EnsureTags needs.
const bsdCtagsShim = "echo 'ctags: illegal option -- -' >&2\nexit 1\n"

func TestPreflightKeepsFindSymbolWhenTheGoIndexCanCover(t *testing.T) {
	workdir(t)
	writeFile(t, "main.go", "package main\n\nfunc main() {}\n")
	shimDir(t, map[string]string{
		"rg":    "exit 0\n",
		"git":   gitRepoShim,
		"ctags": bsdCtagsShim,
	})

	got := defaultEnv(t).Preflight()
	if len(got) != 1 {
		t.Fatalf("Preflight() = %v, want exactly the degraded-index note", got)
	}
	// The operator is told the index narrowed, not that a tool is gone -- and
	// the note must not claim find_symbol is unavailable, because it is not.
	for _, want := range []string{"not Universal Ctags", "built-in index", "Go only"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("Preflight() = %q, want it to mention %q", got[0], want)
		}
	}
	if strings.Contains(got[0], "find_symbol is unavailable") {
		t.Errorf("Preflight() = %q, want find_symbol reported as working", got[0])
	}
}

func TestUnavailableToolsOffersFindSymbolOnAStockMac(t *testing.T) {
	workdir(t)
	writeFile(t, "main.go", "package main\n\nfunc main() {}\n")
	// The stock macOS state: BSD ctags, and a Go project to index.
	shimDir(t, map[string]string{"ctags": bsdCtagsShim})

	if _, withheld := defaultEnv(t).UnavailableTools()[ToolFindSymbol]; withheld {
		t.Fatal("find_symbol was withheld even though the built-in Go index can answer")
	}
}

func TestUnavailableToolsOffersFindSymbolWithAnIndexOnDisk(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)
	shimDir(t, nil) // ctags is gone, its index is not

	if _, withheld := defaultEnv(t).UnavailableTools()[ToolFindSymbol]; withheld {
		t.Fatal("find_symbol was withheld even though an index is already on disk")
	}
}

func TestRebuildTagsDropsTheIndexWhenTheGoFallbackIsInUse(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", "stale content\n")
	writeFile(t, "main.go", "package main\n\nfunc main() {}\n")
	shimDir(t, map[string]string{"ctags": bsdCtagsShim})

	// There is no ctags index to rebuild, and the built-in one is rebuilt on
	// every lookup. Dropping the stale file is the whole job, and reporting the
	// missing ctags as a failure would call a working tool broken.
	if err := defaultEnv(t).RebuildTags(); err != nil {
		t.Fatalf("RebuildTags() error = %v, want the Go fallback to make this a no-op", err)
	}
	if fileExists(".tags") {
		t.Fatal(".tags survived RebuildTags")
	}
}
