package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTags = `!_TAG_FILE_FORMAT	2	/extended format/
Agent	agent/loop.go	/^type Agent struct$/;"	struct	line:22	end:30
Step	agent/loop.go	/^func (a *Agent) Step$/;"	kind:func	line:36
Bare	tools/bare.go	/^func Bare$/;"
Short	two-fields-only
Agent	other/dup.go	/^type Agent$/;"	kind:type	line:9
`

func TestEnsureTagsSkipsWhenTagsFileExists(t *testing.T) {
	dir := workdir(t)
	writeFile(t, ".tags", sampleTags)
	shimDir(t, map[string]string{
		"ctags": "echo ran > " + dir + "/ctags-ran\n",
	})

	if err := defaultEnv(t).EnsureTags(); err != nil {
		t.Fatalf("EnsureTags() = %v, want nil", err)
	}
	if _, err := os.Stat(dir + "/ctags-ran"); err == nil {
		t.Fatal("ctags was invoked even though .tags already existed")
	}
	got, err := os.ReadFile(".tags")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sampleTags {
		t.Fatal("existing .tags file was overwritten")
	}
}

func TestEnsureTagsGeneratesTagsFile(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{
		// Mimic ctags: honour "-f <file>" and write an index there.
		"ctags": `while [ $# -gt 0 ]; do
  if [ "$1" = "-f" ]; then shift; out="$1"; fi
  shift
done
printf 'Sym\tmain.go\t/^Sym$/;"\tkind:func\tline:3\n' > "$out"
`,
	})

	if err := defaultEnv(t).EnsureTags(); err != nil {
		t.Fatalf("EnsureTags() = %v, want nil", err)
	}
	b, err := os.ReadFile(".tags")
	if err != nil {
		t.Fatalf("expected .tags to be generated: %v", err)
	}
	if !strings.Contains(string(b), "Sym") {
		t.Fatalf(".tags content = %q, want it to contain the indexed symbol", b)
	}
}

func TestEnsureTagsReturnsErrorWhenCtagsUnavailable(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	err := defaultEnv(t).EnsureTags()
	if err == nil {
		t.Fatal("EnsureTags() = nil, want error when ctags is not on PATH")
	}
	for _, want := range []string{"ctags is not installed", "find_symbol is unavailable",
		"Do not retry", "search_text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("EnsureTags() error = %v, want it to mention %q", err, want)
		}
	}
}

func TestEnsureTagsRetriesWithoutTheEndField(t *testing.T) {
	workdir(t)
	// A Universal Ctags too old for --fields=+e rejects the whole invocation.
	// The retry without it is what keeps find_symbol working there.
	shimDir(t, map[string]string{
		"ctags": `for arg in "$@"; do
  case "$arg" in
    *+neK*) echo "Unknown field letter: e" >&2; exit 1;;
  esac
done
while [ $# -gt 0 ]; do
  if [ "$1" = "-f" ]; then shift; out="$1"; fi
  shift
done
printf 'Sym\tmain.go\t/^Sym$/;"\tkind:func\tline:3\n' > "$out"
`,
	})

	if err := defaultEnv(t).EnsureTags(); err != nil {
		t.Fatalf("EnsureTags() = %v, want the retry to succeed", err)
	}
	if !fileExists(".tags") {
		t.Fatal("no index was written by the retry")
	}
}

func TestEnsureTagsDiscardsAStubFromAFailedRun(t *testing.T) {
	dir := workdir(t)
	// A ctags that creates the index file and then fails leaves a stub behind,
	// which every later call would take for a usable index.
	shimDir(t, map[string]string{
		"ctags": "touch " + dir + "/.tags\nexit 1\n",
	})

	if err := defaultEnv(t).EnsureTags(); err == nil {
		t.Fatal("EnsureTags() = nil, want the failure reported")
	}
	if fileExists(".tags") {
		t.Fatal(".tags survived a failed run and would be trusted next time")
	}
}

func TestEnsureTagsExplainsIncompatibleCtags(t *testing.T) {
	workdir(t)
	// BSD ctags: present, but it rejects the flags EnsureTags relies on.
	shimDir(t, map[string]string{
		"ctags": "echo 'ctags: illegal option -- -' >&2\nexit 1\n",
	})

	err := defaultEnv(t).EnsureTags()
	if err == nil {
		t.Fatal("EnsureTags() = nil, want an error from an incompatible ctags")
	}
	if !strings.Contains(err.Error(), "Universal Ctags") {
		t.Errorf("EnsureTags() error = %v, want it to name the likely cause", err)
	}
	if !strings.Contains(err.Error(), "illegal option") {
		t.Errorf("EnsureTags() error = %v, want ctags's own message included", err)
	}
}

func TestFindSymbolReportsThatNoIndexCanBeBuilt(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	// No ctags, no Go source: nothing can answer, and the message has to tell
	// the model so rather than let it retry the tool for the rest of the turn.
	_, err := defaultEnv(t).FindSymbol("Agent")
	if err == nil {
		t.Fatal("FindSymbol() = nil error, want a refusal when no index is possible")
	}
	for _, want := range []string{"no symbol index is available", "Do not retry", "search_text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("FindSymbol() error = %v, want it to mention %q", err, want)
		}
	}
}

func TestFindSymbolReportsCtagsFailureWhenNoFallbackExists(t *testing.T) {
	workdir(t)
	// ctags is present and claims to be Universal, so it is chosen -- and then
	// fails. With no Go source to fall back on, the failure is the answer.
	shimDir(t, map[string]string{
		"ctags": `case "$1" in
  --version) echo "Universal Ctags 6.1.0"; exit 0;;
esac
echo "boom" >&2
exit 2
`,
	})

	_, err := defaultEnv(t).FindSymbol("Agent")
	if err == nil || !strings.Contains(err.Error(), "failed generating ctags") {
		t.Fatalf("FindSymbol() error = %v, want it to wrap the ctags failure", err)
	}
}

func TestFindSymbolFallsBackToTheGoIndex(t *testing.T) {
	workdir(t)
	writeFile(t, "greet.go", "package greet\n\nfunc Greet() string {\n\treturn \"hi\"\n}\n")
	// BSD ctags: on PATH, and unusable. This is the stock macOS state, and the
	// one the built-in index exists for.
	shimDir(t, map[string]string{
		"ctags": "echo 'ctags: illegal option -- -' >&2\nexit 1\n",
	})

	got, err := defaultEnv(t).FindSymbol("Greet")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v, want the Go index to answer", err)
	}
	if got != "Greet [func] -> greet.go:3-5" {
		t.Fatalf("FindSymbol() = %q, want the built-in index result", got)
	}
	if fileExists(".tags") {
		t.Error("a .tags file was written by the fallback path")
	}
}

func TestFindSymbolFallsBackWhenCtagsBreaksMidRun(t *testing.T) {
	workdir(t)
	writeFile(t, "greet.go", "package greet\n\nfunc Greet() {}\n")
	// A ctags that passes the version check and then fails is not a refusal:
	// the Go index answers the same question.
	shimDir(t, map[string]string{
		"ctags": `case "$1" in
  --version) echo "Universal Ctags 6.1.0"; exit 0;;
esac
exit 2
`,
	})

	got, err := defaultEnv(t).FindSymbol("Greet")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	if !strings.Contains(got, "greet.go:3") {
		t.Fatalf("FindSymbol() = %q, want the Go index to have answered", got)
	}
}

func TestFindSymbolPrefersAnExistingIndexOverTheGoFallback(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)
	writeFile(t, "other.go", "package other\n\nfunc Step() {}\n")
	shimDir(t, nil) // ctags is gone; the index it left behind is not

	got, err := defaultEnv(t).FindSymbol("Step")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	if !strings.Contains(got, "loop.go:36") {
		t.Fatalf("FindSymbol() = %q, want the existing index used", got)
	}
}

func TestFindSymbolReportsSpansFromTheEndField(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags",
		"Wide	wide.go	/^func Wide$/;\"	kind:func	line:10	end:24\n"+
			"Thin	wide.go	/^var Thin$/;\"	kind:var	line:30	end:30\n")
	writeFile(t, "wide.go", "x\n")

	got, err := defaultEnv(t).FindSymbol("Wide")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Wide [func] -> wide.go:10-24" {
		t.Fatalf("FindSymbol() = %q, want the span from the end: field", got)
	}
	// end == line is one line, and a range would be noise.
	if got, _ := defaultEnv(t).FindSymbol("Thin"); got != "Thin [var] -> wide.go:30" {
		t.Fatalf("FindSymbol() = %q, want a single-line symbol collapsed", got)
	}
}

func TestFindSymbolMatchesExactSymbolAcrossFiles(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)

	got, err := defaultEnv(t).FindSymbol("Agent")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	// The first entry uses the bare kind field real Universal Ctags emits, and
	// carries an end line; the second uses the prefixed form. Both must parse.
	want := "Agent [struct] -> agent/loop.go:22-30\n" +
		"Agent [type] -> other/dup.go:9"
	if got != want {
		t.Fatalf("FindSymbol() =\n%q\nwant\n%q", got, want)
	}
}

func TestFindSymbolDefaultsMissingKindAndLineFields(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)

	got, err := defaultEnv(t).FindSymbol("Bare")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	if got != "Bare [sym] -> tools/bare.go:unknown" {
		t.Fatalf("FindSymbol() = %q, want the sym/unknown fallbacks", got)
	}
}

func TestFindSymbolIgnoresHeaderAndMalformedLines(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)

	for _, sym := range []string{"!_TAG_FILE_FORMAT", "Short"} {
		got, err := defaultEnv(t).FindSymbol(sym)
		if err != nil {
			t.Fatalf("FindSymbol(%q) error = %v", sym, err)
		}
		if !strings.Contains(got, "not found") {
			t.Fatalf("FindSymbol(%q) = %q, want a not-found result", sym, got)
		}
	}
}

func TestFindSymbolNotFound(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)

	got, err := defaultEnv(t).FindSymbol("Nonexistent")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	if got != "Symbol 'Nonexistent' not found." {
		t.Fatalf("FindSymbol() = %q, want the not-found message", got)
	}
}

func TestFindSymbolReturnsErrorWhenTagsFileUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	workdir(t)
	writeFile(t, ".tags", sampleTags)
	if err := os.Chmod(".tags", 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(".tags", 0o644) })

	if _, err := defaultEnv(t).FindSymbol("Agent"); err == nil {
		t.Fatal("FindSymbol() = nil error, want a read failure")
	}
}

func TestFindSymbolSkipsPathsOutsideTheProject(t *testing.T) {
	dir := workdir(t)
	env := NewEnv(DefaultBudgets())
	// An index built before --links=no, or edited by hand, can name a path
	// outside the project. FindSymbol reports index paths verbatim, so it must
	// not report what the other tools would refuse to open.
	writeFile(t, filepath.Join(dir, ".tags"),
		"Escaped\t../../outside.go\t/^x$/;\"\tkind:func\tline:1\n"+
			"Inside\tinside.go\t/^x$/;\"\tkind:func\tline:2\n")
	writeFile(t, filepath.Join(dir, "inside.go"), "x\n")

	got, err := env.FindSymbol("Escaped")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "not found") {
		t.Fatalf("FindSymbol() = %q, want an out-of-project path withheld", got)
	}
	if got, _ := env.FindSymbol("Inside"); !strings.Contains(got, "inside.go") {
		t.Fatalf("FindSymbol() = %q, want in-project symbols still reported", got)
	}
}

// TestFindSymbolReadsTheKindFormatRealCtagsEmits is the regression test for a
// bug the shims hid. Universal Ctags writes the kind as a bare field -- "func",
// not "kind:func" -- so every real lookup reported "sym" and discarded the one
// field that tells the model what it found. The hermetic shims emitted the
// prefixed form, which is exactly why nothing noticed until the integration
// image was asked what ctags actually produces.
func TestFindSymbolReadsTheKindFormatRealCtagsEmits(t *testing.T) {
	workdir(t)
	writeFile(t, "greet.go", "x\n")
	writeFile(t, ".tags",
		"Greet\tgreet.go\t/^func Greet() string {$/;\"\tfunc\tline:3\tpackage:main\tend:5\n")

	got, err := defaultEnv(t).FindSymbol("Greet")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Greet [func] -> greet.go:3-5" {
		t.Fatalf("FindSymbol() = %q, want the bare kind field read", got)
	}
}

func TestFindSymbolDoesNotMistakeThePatternForAKind(t *testing.T) {
	workdir(t)
	writeFile(t, "bare.go", "x\n")
	// The search pattern sits at index 2 and contains no colon, so a scan that
	// began there would report it as the kind.
	writeFile(t, ".tags", "Bare\tbare.go\t/^func Bare$/;\"\n")

	got, err := defaultEnv(t).FindSymbol("Bare")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[sym]") {
		t.Fatalf("FindSymbol() = %q, want the pattern not read as a kind", got)
	}
}
