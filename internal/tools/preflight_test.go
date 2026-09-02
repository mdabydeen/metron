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

	if got := Preflight(); len(got) != 0 {
		t.Fatalf("Preflight() = %v, want no warnings", got)
	}
}

func TestPreflightReportsMissingBinaries(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	got := Preflight()
	if len(got) != 3 {
		t.Fatalf("Preflight() = %v, want one warning per dependency", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"rg not found on PATH - search_text is unavailable",
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

	got := Preflight()
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

	got := Preflight()
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

	got := Preflight()
	if len(got) != 1 || !strings.Contains(got[0], "not a git repository") {
		t.Fatalf("Preflight() = %v, want the not-a-repository warning", got)
	}
	if !strings.Contains(got[0], "apply_patch will fail") {
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

	if got := Preflight(); len(got) != 1 || !strings.Contains(got[0], "not a git repository") {
		t.Fatalf("Preflight() = %v, want the not-a-repository warning", got)
	}
}

func TestRebuildTagsReplacesStaleIndex(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", "stale content\n")
	shimDir(t, map[string]string{"ctags": universalCtagsShim})

	if err := RebuildTags(); err != nil {
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

	if err := RebuildTags(); err != nil {
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

	err := RebuildTags()
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

	err := RebuildTags()
	if err == nil || !strings.Contains(err.Error(), "remove stale index") {
		t.Fatalf("RebuildTags() error = %v, want the removal failure reported", err)
	}
}
