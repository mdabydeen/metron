package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editEnv gives an Env rooted in a scratch tree holding one file.
func editEnv(t *testing.T, name, contents string) Env {
	t.Helper()
	workdir(t)
	env := NewEnv(DefaultBudgets())
	env.EditFormat = FormatSearchReplace
	writeFile(t, filepath.Join(env.Root, name), contents)
	return env
}

func read(t *testing.T, env Env, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(env.Root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEditFileReplacesAnExactMatch(t *testing.T) {
	env := editEnv(t, "a.go", "package main\n\nfunc Greet() string {\n\treturn \"hello\"\n}\n")

	got := env.EditFile("a.go", "\treturn \"hello\"", "\treturn \"hola\"")

	if !strings.Contains(got, "Edited a.go") {
		t.Fatalf("EditFile() = %q, want the edit confirmed", got)
	}
	if want := "\treturn \"hola\"\n"; !strings.Contains(read(t, env, "a.go"), want) {
		t.Fatalf("file = %q, want %q applied", read(t, env, "a.go"), want)
	}
}

func TestEditFileMatchesAcrossSeveralLines(t *testing.T) {
	env := editEnv(t, "a.go", "one\ntwo\nthree\nfour\n")

	env.EditFile("a.go", "two\nthree", "TWO\nTHREE")

	if got, want := read(t, env, "a.go"), "one\nTWO\nTHREE\nfour\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

// The ladder exists because a model retyping a line it has read commonly loses
// trailing whitespace or a level of indentation. Neither should sink the edit.
func TestEditFileToleratesTrailingWhitespaceDrift(t *testing.T) {
	env := editEnv(t, "a.go", "alpha   \nbeta\n")

	got := env.EditFile("a.go", "alpha", "ALPHA")

	if strings.Contains(got, "not found") {
		t.Fatalf("EditFile() = %q, want trailing whitespace tolerated", got)
	}
	if g, want := read(t, env, "a.go"), "ALPHA\nbeta\n"; g != want {
		t.Fatalf("file = %q, want %q", g, want)
	}
}

func TestEditFileToleratesIndentationDriftAndRestoresIt(t *testing.T) {
	env := editEnv(t, "a.go", "func f() {\n\t\tif x {\n\t\t\treturn 1\n\t\t}\n}\n")

	// The model quotes the block with one less level of indentation.
	got := env.EditFile("a.go", "\tif x {\n\t\treturn 1\n\t}", "\tif x {\n\t\treturn 2\n\t}")

	if strings.Contains(got, "not found") {
		t.Fatalf("EditFile() = %q, want indentation drift tolerated", got)
	}
	// The file's own indentation must survive: a matched edit that reindents
	// the surrounding code is worse than a refused one.
	if g, want := read(t, env, "a.go"), "func f() {\n\t\tif x {\n\t\t\treturn 2\n\t\t}\n}\n"; g != want {
		t.Fatalf("file = %q, want %q -- indentation was not restored", g, want)
	}
}

// A forgiving comparison finds more matches, so trying it first would call an
// exact match ambiguous. The exact match must win.
func TestEditFilePrefersTheStrictestMatch(t *testing.T) {
	env := editEnv(t, "a.go", "  target\ntarget\n")

	got := env.EditFile("a.go", "target", "changed")

	if strings.Contains(got, "matches 2 places") {
		t.Fatalf("EditFile() = %q, want the exact match to win over the loose one", got)
	}
	if g, want := read(t, env, "a.go"), "  target\nchanged\n"; g != want {
		t.Fatalf("file = %q, want %q", g, want)
	}
}

func TestEditFileRefusesAnAmbiguousSearch(t *testing.T) {
	env := editEnv(t, "a.go", "x := 1\ny := 2\nx := 1\n")
	before := read(t, env, "a.go")

	got := env.EditFile("a.go", "x := 1", "x := 3")

	// Guessing between two matches is how an agent silently edits the wrong
	// function. It has to come back and quote more.
	for _, want := range []string{"matches 2 places", "lines 1, 3", "Quote more"} {
		if !strings.Contains(got, want) {
			t.Errorf("EditFile() = %q, missing %q", got, want)
		}
	}
	if read(t, env, "a.go") != before {
		t.Fatal("an ambiguous edit modified the file")
	}
}

func TestEditFileReportsAMissWithTheNearestLine(t *testing.T) {
	env := editEnv(t, "a.go", "package main\n\nfunc Greet() string {\n\treturn \"hello\"\n}\n")

	got := env.EditFile("a.go", "\treturn \"goodbye\" // wrong", "x")

	if !strings.Contains(got, "not found") {
		t.Fatalf("EditFile() = %q, want the miss reported", got)
	}
	if !strings.Contains(got, "view_slice") {
		t.Fatalf("EditFile() = %q, want the model told how to recover", got)
	}
}

func TestEditFileDeletesWhenReplaceIsEmpty(t *testing.T) {
	env := editEnv(t, "a.go", "keep\ndrop\nkeep2\n")

	env.EditFile("a.go", "drop", "")

	if got, want := read(t, env, "a.go"), "keep\nkeep2\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestEditFileCreatesAFileFromAnEmptySearch(t *testing.T) {
	env := editEnv(t, "a.go", "existing\n")

	got := env.EditFile("new.go", "", "package main\n")

	if !strings.Contains(got, "Created new.go") {
		t.Fatalf("EditFile() = %q, want the creation confirmed", got)
	}
	if g, want := read(t, env, "new.go"), "package main\n"; g != want {
		t.Fatalf("file = %q, want %q", g, want)
	}
}

func TestEditFileRefusesAnEmptySearchOnAnExistingFile(t *testing.T) {
	env := editEnv(t, "a.go", "existing\n")

	got := env.EditFile("a.go", "", "clobbered\n")

	// Without this, an empty search would silently overwrite whole files.
	if !strings.Contains(got, "already exists") {
		t.Fatalf("EditFile() = %q, want the clobber refused", got)
	}
	if read(t, env, "a.go") != "existing\n" {
		t.Fatal("an empty search overwrote an existing file")
	}
}

func TestEditFileRefusesAMissingFile(t *testing.T) {
	env := editEnv(t, "a.go", "x\n")

	got := env.EditFile("gone.go", "x", "y")

	if !strings.Contains(got, "does not exist") {
		t.Fatalf("EditFile() = %q, want the missing file reported", got)
	}
}

func TestEditFileRefusesAPathOutsideTheProject(t *testing.T) {
	env := editEnv(t, "a.go", "x\n")

	for _, path := range []string{"../escape.go", "/etc/hosts"} {
		got := env.EditFile(path, "x", "y")
		if !strings.Contains(got, "Edit refused") || !strings.Contains(got, "outside the project") {
			t.Errorf("EditFile(%q) = %q, want it refused by the resolver", path, got)
		}
	}
}

func TestEditFilePreservesFileMode(t *testing.T) {
	env := editEnv(t, "run.sh", "#!/bin/sh\necho old\n")
	path := filepath.Join(env.Root, "run.sh")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}

	env.EditFile("run.sh", "echo old", "echo new")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rewriting a script as 0644 would break the thing it was editing.
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("mode = %v, want 0755 preserved", got)
	}
}

func TestEditFileRejectsAnEmptySearchBlockOfOnlyNewlines(t *testing.T) {
	env := editEnv(t, "a.go", "x\n")

	got := env.EditFile("a.go", "\n", "y")

	if !strings.Contains(got, "empty") {
		t.Fatalf("EditFile() = %q, want a whitespace-only search rejected", got)
	}
}

// The operator approves changes in unified diff, whatever notation the model
// used to express them.
func TestEditPreviewRendersAUnifiedDiff(t *testing.T) {
	env := editEnv(t, "a.go", "one\ntwo\nthree\nfour\nfive\nsix\nseven\n")

	got := env.EditPreview("a.go", "four", "FOUR")

	for _, want := range []string{"--- a/a.go", "+++ b/a.go", "@@ ", "-four", "+FOUR", " three", " five"} {
		if !strings.Contains(got, want) {
			t.Errorf("EditPreview() = %q, missing %q", got, want)
		}
	}
	// A preview must not touch the file.
	if read(t, env, "a.go") != "one\ntwo\nthree\nfour\nfive\nsix\nseven\n" {
		t.Fatal("EditPreview() modified the file")
	}
}

func TestEditPreviewExplainsAnEditThatWillNotApply(t *testing.T) {
	env := editEnv(t, "a.go", "one\ntwo\n")

	// The operator should not be asked to approve something doomed to fail.
	if got := env.EditPreview("a.go", "nope", "x"); !strings.Contains(got, "will fail") {
		t.Errorf("EditPreview() = %q, want the failure declared", got)
	}
	if got := env.EditPreview("../out.go", "x", "y"); !strings.Contains(got, "will be refused") {
		t.Errorf("EditPreview() = %q, want the refusal declared", got)
	}
	if got := env.EditPreview("new.go", "", "hello\n"); !strings.Contains(got, "create new.go") {
		t.Errorf("EditPreview() = %q, want creation described", got)
	}
}

func TestEditFileHandlesAFileWithoutATrailingNewline(t *testing.T) {
	env := editEnv(t, "a.go", "alpha\nbeta")

	env.EditFile("a.go", "beta", "gamma")

	// Round-tripping through split/join must not invent or drop a newline.
	if got, want := read(t, env, "a.go"), "alpha\ngamma"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestEditFileToleratesATrailingNewlineOnTheSearchBlock(t *testing.T) {
	env := editEnv(t, "a.go", "alpha\nbeta\n")

	// A model quoting a block will often include the newline that ended it.
	env.EditFile("a.go", "alpha\n", "ALPHA")

	if got, want := read(t, env, "a.go"), "ALPHA\nbeta\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestEditFileSaysWhenTheMatchWasNotExact(t *testing.T) {
	env := editEnv(t, "a.go", "\t\tvalue := 1\n")

	got := env.EditFile("a.go", "value := 1", "value := 2")

	// A loose match is usually right, but the operator should be told, because
	// this is the sentence they will want to have seen if it was wrong.
	if !strings.Contains(got, "matched ignoring indentation") {
		t.Fatalf("EditFile() = %q, want the looser match declared", got)
	}
}

func TestEditFileStaysQuietWhenTheMatchWasExact(t *testing.T) {
	env := editEnv(t, "a.go", "value := 1\n")

	if got := env.EditFile("a.go", "value := 1", "value := 2"); strings.Contains(got, "matched") {
		t.Fatalf("EditFile() = %q, want no note for an exact match", got)
	}
}

func TestEditFileReportsAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	env := editEnv(t, "secret.go", "x\n")
	if err := os.Chmod(filepath.Join(env.Root, "secret.go"), 0o000); err != nil {
		t.Fatal(err)
	}

	if got := env.EditFile("secret.go", "x", "y"); !strings.Contains(got, "Edit refused") {
		t.Fatalf("EditFile() = %q, want the read failure reported", got)
	}
	if got := env.EditPreview("secret.go", "x", "y"); !strings.Contains(got, "will be refused") {
		t.Fatalf("EditPreview() = %q, want the read failure declared", got)
	}
}

func TestEditFileReportsAnUnwritableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anything")
	}
	env := editEnv(t, "ro.go", "x\n")
	// Readable, so the match succeeds; the write is what fails.
	if err := os.Chmod(filepath.Join(env.Root, "ro.go"), 0o444); err != nil {
		t.Fatal(err)
	}

	if got := env.EditFile("ro.go", "x", "y"); !strings.Contains(got, "Edit failed") {
		t.Fatalf("EditFile() = %q, want the write failure reported", got)
	}
}

func TestEditFileKeepsBlankLinesUnindented(t *testing.T) {
	env := editEnv(t, "a.go", "\t\tfirst\n")

	// Matched loosely, so the replacement is re-indented -- but indenting a
	// blank line just leaves trailing whitespace behind.
	env.EditFile("a.go", "first", "first\n\nsecond")

	if got, want := read(t, env, "a.go"), "\t\tfirst\n\n\t\tsecond\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestEditFileHandlesAnEditAtTheStartAndEnd(t *testing.T) {
	// Exercises the diff hunk's context clamping at both boundaries.
	env := editEnv(t, "a.go", "first\nmiddle\nlast\n")

	if got := env.EditFile("a.go", "first", "FIRST"); !strings.Contains(got, "@@ -1,") {
		t.Fatalf("EditFile() = %q, want a hunk starting at line 1", got)
	}
	if got := env.EditFile("a.go", "last", "LAST"); !strings.Contains(got, "-last") {
		t.Fatalf("EditFile() = %q, want the final line edited", got)
	}
	if got, want := read(t, env, "a.go"), "FIRST\nmiddle\nLAST\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestEditFileMissWithNoNearbyLine(t *testing.T) {
	env := editEnv(t, "a.go", "alpha\n")

	// Nothing resembles the search, so there is no hint to give -- the message
	// still has to be useful.
	got := env.EditFile("a.go", "zzz", "y")

	if !strings.Contains(got, "not found") || !strings.Contains(got, "view_slice") {
		t.Fatalf("EditFile() = %q, want a usable miss message without a hint", got)
	}
}

func TestEditFileCreatesAnEmptyFile(t *testing.T) {
	env := editEnv(t, "a.go", "x\n")

	if got := env.EditFile("empty.go", "", ""); !strings.Contains(got, "Created empty.go (0 lines)") {
		t.Fatalf("EditFile() = %q, want an empty file created", got)
	}
	if got := read(t, env, "empty.go"); got != "" {
		t.Fatalf("file = %q, want it empty", got)
	}
}

func TestIndentDeltaIgnoresIncompatibleIndentation(t *testing.T) {
	// Spaces in the file, a tab in the quote: there is no delta that makes
	// sense, so re-indenting must not guess.
	if got := indentDelta("    x", "\tx"); got != "" {
		t.Fatalf("indentDelta() = %q, want no adjustment for mismatched indent styles", got)
	}
}

func TestEditFileMissNamesAPartiallyMatchingLine(t *testing.T) {
	env := editEnv(t, "a.go", "alpha beta gamma\ndelta\n")

	// The first quoted line occurs inside a file line, but the block as a whole
	// does not match. Pointing at the near miss is what turns a dead end into a
	// next step.
	got := env.EditFile("a.go", "alpha\nzzz", "x")

	if !strings.Contains(got, "closest line is 1") {
		t.Fatalf("EditFile() = %q, want the near miss named", got)
	}
	if !strings.Contains(got, "alpha beta gamma") {
		t.Fatalf("EditFile() = %q, want the near line quoted", got)
	}
}

func TestEditFileMissWithABlankFirstQuotedLine(t *testing.T) {
	env := editEnv(t, "a.go", "alpha\n")

	// A leading blank line gives nothing to search for, so there is no hint --
	// but the message must still be well-formed.
	got := env.EditFile("a.go", "\nzzz", "x")

	if !strings.Contains(got, "not found") {
		t.Fatalf("EditFile() = %q, want the miss reported", got)
	}
	if strings.Contains(got, "closest line") {
		t.Fatalf("EditFile() = %q, want no hint when there is nothing to hint at", got)
	}
}

func TestEditFileOnAnEmptyFile(t *testing.T) {
	env := editEnv(t, "empty.go", "")

	got := env.EditFile("empty.go", "anything", "x")

	if !strings.Contains(got, "not found") {
		t.Fatalf("EditFile() = %q, want a clean miss on an empty file", got)
	}
}
