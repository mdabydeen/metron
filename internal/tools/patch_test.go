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

	escapes := []string{
		"--- /dev/null\n+++ b/../../escaped.txt\n@@ -0,0 +1 @@\n+pwned\n",
		"--- a/../outside.go\n+++ b/../outside.go\n@@ -1 +1 @@\n-a\n+b\n",
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
	want := []string{"x.go", "x.go", "new.go", "plain.go"}
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
