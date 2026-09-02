package tools

import (
	"os"
	"strings"
	"testing"
)

const sampleTags = `!_TAG_FILE_FORMAT	2	/extended format/
Agent	internal/agent/loop.go	/^type Agent struct$/;"	kind:struct	line:22
Step	internal/agent/loop.go	/^func (a *Agent) Step$/;"	kind:func	line:36
Bare	internal/tools/bare.go	/^func Bare$/;"
Short	two-fields-only
Agent	internal/other/dup.go	/^type Agent$/;"	kind:type	line:9
`

func TestEnsureTagsSkipsWhenTagsFileExists(t *testing.T) {
	dir := workdir(t)
	writeFile(t, ".tags", sampleTags)
	shimDir(t, map[string]string{
		"ctags": "echo ran > " + dir + "/ctags-ran\n",
	})

	if err := EnsureTags(); err != nil {
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

	if err := EnsureTags(); err != nil {
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

	err := EnsureTags()
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

func TestEnsureTagsExplainsIncompatibleCtags(t *testing.T) {
	workdir(t)
	// BSD ctags: present, but it rejects the flags EnsureTags relies on.
	shimDir(t, map[string]string{
		"ctags": "echo 'ctags: illegal option -- -' >&2\nexit 1\n",
	})

	err := EnsureTags()
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

func TestFindSymbolReportsCtagsFailure(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	_, err := FindSymbol("Agent")
	if err == nil || !strings.Contains(err.Error(), "failed generating ctags") {
		t.Fatalf("FindSymbol() error = %v, want it to wrap the ctags failure", err)
	}
}

func TestFindSymbolMatchesExactSymbolAcrossFiles(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)

	got, err := FindSymbol("Agent")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	want := "Agent [struct] -> internal/agent/loop.go:22\n" +
		"Agent [type] -> internal/other/dup.go:9"
	if got != want {
		t.Fatalf("FindSymbol() =\n%q\nwant\n%q", got, want)
	}
}

func TestFindSymbolDefaultsMissingKindAndLineFields(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)

	got, err := FindSymbol("Bare")
	if err != nil {
		t.Fatalf("FindSymbol() error = %v", err)
	}
	if got != "Bare [sym] -> internal/tools/bare.go:unknown" {
		t.Fatalf("FindSymbol() = %q, want the sym/unknown fallbacks", got)
	}
}

func TestFindSymbolIgnoresHeaderAndMalformedLines(t *testing.T) {
	workdir(t)
	writeFile(t, ".tags", sampleTags)

	for _, sym := range []string{"!_TAG_FILE_FORMAT", "Short"} {
		got, err := FindSymbol(sym)
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

	got, err := FindSymbol("Nonexistent")
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

	if _, err := FindSymbol("Agent"); err == nil {
		t.Fatal("FindSymbol() = nil error, want a read failure")
	}
}
