package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo initialises a scratch git repository containing target.txt and makes
// it the working directory.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := workdir(t)
	writeFile(t, "target.txt", "alpha\nbeta\ngamma\n")
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "target.txt"},
		{"-c", "commit.gpgsign=false", "commit", "-qm", "init"},
	} {
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

const goodDiff = `--- a/target.txt
+++ b/target.txt
@@ -1,3 +1,3 @@
 alpha
-beta
+BETA
 gamma
`

func TestApplyPatchAppliesUnifiedDiff(t *testing.T) {
	gitRepo(t)

	got, err := defaultEnv(t).ApplyPatch(goodDiff)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if got != "Patch successfully applied to working tree." {
		t.Fatalf("ApplyPatch() = %q, want the success message", got)
	}
	b, err := os.ReadFile("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("target.txt = %q, want the patched content", b)
	}
}

func TestApplyPatchRejectsNonMatchingDiff(t *testing.T) {
	gitRepo(t)

	stale := strings.Replace(goodDiff, " alpha", " ALPHA", 1)
	got, err := defaultEnv(t).ApplyPatch(stale)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v, want the failure reported as content", err)
	}
	if !strings.Contains(got, "Patch dry-run failed") {
		t.Fatalf("ApplyPatch() = %q, want a dry-run failure report", got)
	}
	b, _ := os.ReadFile("target.txt")
	if string(b) != "alpha\nbeta\ngamma\n" {
		t.Fatalf("target.txt = %q, want it left untouched", b)
	}
}

func TestApplyPatchRejectsMalformedDiff(t *testing.T) {
	gitRepo(t)

	got, err := defaultEnv(t).ApplyPatch("this is not a diff at all\n")
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if !strings.Contains(got, "Patch dry-run failed") {
		t.Fatalf("ApplyPatch() = %q, want a dry-run failure report", got)
	}
}

func TestApplyPatchReportsApplyFailureAfterSuccessfulCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := gitRepo(t)

	// --check only reads, so it passes; writing the result then fails because
	// the containing directory is not writable.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	got, err := defaultEnv(t).ApplyPatch(goodDiff)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if !strings.Contains(got, "Patch application failed") {
		t.Fatalf("ApplyPatch() = %q, want an application failure report", got)
	}
}

func TestApplyPatchReportsMissingGit(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	_, err := defaultEnv(t).ApplyPatch(goodDiff)
	if err == nil {
		t.Fatal("ApplyPatch() = nil error, want a missing-git error")
	}
	for _, want := range []string{"git unavailable", "git is not installed",
		"apply_patch is unavailable", "Do not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ApplyPatch() error = %v, want it to mention %q", err, want)
		}
	}
}

func TestApplyPatchRefusesTargetsOutsideTheProject(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	// git is never reached: the path check runs first, so this needs no repo.
	shimDir(t, nil)

	// A target that is absolute and sits just outside the project, a
	// sibling of it, so no symlink resolution can pull it back inside.
	absOutside := filepath.Join(dir, "escaped.txt")

	// Each escape is written through a different header form so that no
	// single form can smuggle a path past the boundary check.
	escapes := []string{
		// --- / +++ with a relative traversal and with /dev/null.
		"--- /dev/null\n+++ b/../../escaped.txt\n@@ -0,0 +1 @@\n+pwned\n",
		"--- a/../outside.go\n+++ b/../outside.go\n@@ -1 +1 @@\n-a\n+b\n",
		// A diff --git plus rename header whose target escapes; and a
		// copy header whose destination escapes.
		"diff --git a/old.go b/../../escaped.go\nrename to ../../escaped.go\n@@ -1 +1 @@\n-a\n+b\n",
		"copy from old.go\ncopy to ../copied.go\n@@ -1 +1 @@\n-a\n+b\n",
		"rename from ../renamed-source.go\nrename to renamed-destination.go\n@@ -1 +1 @@\n-a\n+b\n",
		"copy from ../../copied-source.go\ncopy to copied-destination.go\n@@ -1 +1 @@\n-a\n+b\n",
		// An absolute target resolves as-is and lands outside the project.
		"--- a/x.go\n+++ " + absOutside + "\n@@ -1 +1 @@\n-a\n+b\n",
		"diff --git " + absOutside + " " + absOutside + "\n",
		// Git's quoted path form must be decoded before the boundary check.
		`diff --git "a/../../quoted-outside.go" "b/../../quoted-outside.go"
--- "a/../../quoted-outside.go"
+++ "b/../../quoted-outside.go"
@@ -1 +1 @@
-a
+b
`,
	}
	for _, diff := range escapes {
		got, err := defaultEnv(t).ApplyPatch(diff)
		if err != nil {
			t.Fatalf("ApplyPatch() error = %v, want the refusal reported as content", err)
		}
		if !strings.Contains(got, "Patch rejected") || !strings.Contains(got, "outside the project") {
			t.Fatalf("ApplyPatch() = %q, want a refusal naming the boundary", got)
		}
		if !strings.Contains(got, "Do not retry") {
			t.Fatalf("ApplyPatch() = %q, want the model told not to retry", got)
		}
	}
}

func TestPatchTargetsReadsRenameAndCopy(t *testing.T) {
	diff := "rename from old.go\n" +
		"rename to new.go\n" +
		"copy from src.go\n" +
		"copy to dst.go\n"
	got := patchTargets(diff)
	want := []string{"old.go", "new.go", "src.go", "dst.go"}
	if len(got) != len(want) {
		t.Fatalf("patchTargets() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("patchTargets() = %q, want %q", got, want)
		}
	}
}

func TestPatchTargetsReadsQuotedPaths(t *testing.T) {
	diff := `diff --git "a/dir with spaces/old\"name.txt" "b/dir with spaces/new\"name.txt"
--- "a/dir with spaces/old\"name.txt"
+++ "b/dir with spaces/new\"name.txt"
rename from "old\tname.txt"
rename to "new\tname.txt"
`
	got := patchTargets(diff)
	want := []string{
		`dir with spaces/old"name.txt`,
		`dir with spaces/new"name.txt`,
		`dir with spaces/old"name.txt`,
		`dir with spaces/new"name.txt`,
		"old\tname.txt",
		"new\tname.txt",
	}
	if len(got) != len(want) {
		t.Fatalf("patchTargets() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("patchTargets() = %q, want %q", got, want)
		}
	}
}

func TestPatchTargetsKeepsSpacesInUnquotedDiffPaths(t *testing.T) {
	diff := "diff --git a/foo .. bar b/foo .. bar\n" +
		"--- a/foo .. bar\n" +
		"+++ b/foo .. bar\n"
	got := patchTargets(diff)
	want := []string{"foo .. bar", "foo .. bar", "foo .. bar", "foo .. bar"}
	if len(got) != len(want) {
		t.Fatalf("patchTargets() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("patchTargets() = %q, want %q", got, want)
		}
	}
}

func TestGitDiffPathsRefusesAnUnterminatedQuotedPath(t *testing.T) {
	got := gitDiffPaths(`"unterminated`)
	if len(got) != 1 || got[0] != `"unterminated` {
		t.Fatalf("gitDiffPaths() = %q, want the malformed header preserved as one candidate", got)
	}
}

func TestUnquoteGitPathReportsInvalidEscapesAndMissingQuotes(t *testing.T) {
	for _, input := range []string{`"bad\q"`, `"unterminated`} {
		if _, _, ok := unquoteGitPath(input); ok {
			t.Fatalf("unquoteGitPath(%q) reported success, want malformed input rejected", input)
		}
	}
}

func TestApplyPatchRefusesSymlinkedTargetOutsideTheProject(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the symlink boundary")
	}
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	outside := filepath.Join(dir, "outside")
	for _, d := range []string{project, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A symlink inside the project that points at a sibling outside it. A
	// lexical prefix test would call link/escaped.txt "inside"; only
	// resolving the symlink and checking the boundary reveals the escape.
	if err := os.Symlink(outside, filepath.Join(project, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	t.Chdir(project)
	// git is never reached: the path check runs first, so this needs no repo.
	shimDir(t, nil)

	diff := "--- a/link/escaped.txt\n+++ b/link/escaped.txt\n@@ -0,0 +1 @@\n+pwned\n"
	got, err := defaultEnv(t).ApplyPatch(diff)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v, want the refusal reported as content", err)
	}
	if !strings.Contains(got, "Patch rejected") || !strings.Contains(got, "outside the project") {
		t.Fatalf("ApplyPatch() = %q, want a refusal naming the boundary", got)
	}
	if _, serr := os.Stat(filepath.Join(outside, "escaped.txt")); serr == nil {
		t.Fatal("escaped.txt was created outside the project")
	}
}

func TestPatchTargetsReadsBothHeaderForms(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n" +
		"--- a/x.go\t2026-01-01 00:00:00\n" +
		"+++ b/x.go\n" +
		"@@ -1 +1 @@\n-a\n+b\n" +
		"--- /dev/null\n" +
		"+++ b/new.go\n" +
		"--- plain.go\n" +
		"+++ \n"

	got := patchTargets(diff)
	// The diff --git line contributes the path twice (a/ and b/),
	// and the --- / +++ lines contribute it again; then the create
	// target and the plain-form one. /dev/null and the empty +++ are
	// dropped.
	want := []string{"x.go", "x.go", "x.go", "x.go", "new.go", "plain.go"}
	if len(got) != len(want) {
		t.Fatalf("patchTargets() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("patchTargets() = %q, want %q", got, want)
		}
	}
}

func TestPatchTargetsIgnoresANonDiff(t *testing.T) {
	if got := patchTargets("this is prose, not a diff\n"); got != nil {
		t.Fatalf("patchTargets() = %q, want nothing recognised", got)
	}
}
