package tools

import (
	"os"
	"os/exec"
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

	got, applied, err := ApplyPatch(goodDiff)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if !applied {
		t.Fatal("ApplyPatch() applied = false, want true on success")
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
	got, applied, err := ApplyPatch(stale)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v, want the failure reported as content", err)
	}
	if applied {
		t.Fatal("ApplyPatch() applied = true, want false on a rejected diff")
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

	got, applied, err := ApplyPatch("this is not a diff at all\n")
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if applied {
		t.Fatal("ApplyPatch() applied = true, want false on a malformed diff")
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

	got, applied, err := ApplyPatch(goodDiff)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if applied {
		t.Fatal("ApplyPatch() applied = true, want false when the write itself fails")
	}
	if !strings.Contains(got, "Patch application failed") {
		t.Fatalf("ApplyPatch() = %q, want an application failure report", got)
	}
}

func TestApplyPatchReportsMissingGit(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	_, _, err := ApplyPatch(goodDiff)
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

func TestRevertPatchReversesAnAppliedDiff(t *testing.T) {
	gitRepo(t)
	if _, applied, err := ApplyPatch(goodDiff); err != nil || !applied {
		t.Fatalf("setup: ApplyPatch() = (_, %v, %v), want it applied", applied, err)
	}

	got, reverted, err := RevertPatch(goodDiff)
	if err != nil {
		t.Fatalf("RevertPatch() error = %v", err)
	}
	if !reverted {
		t.Fatal("RevertPatch() reverted = false, want true")
	}
	if got != "Reverted the last applied patch." {
		t.Fatalf("RevertPatch() = %q, want the success message", got)
	}
	b, err := os.ReadFile("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "alpha\nbeta\ngamma\n" {
		t.Fatalf("target.txt = %q, want the original content restored", b)
	}
}

func TestRevertPatchReportsAFailedDryRun(t *testing.T) {
	gitRepo(t)
	// Never applied, so reversing it doesn't match the working tree.

	got, reverted, err := RevertPatch(goodDiff)
	if err != nil {
		t.Fatalf("RevertPatch() error = %v", err)
	}
	if reverted {
		t.Fatal("RevertPatch() reverted = true, want false when the diff was never applied")
	}
	if !strings.Contains(got, "Undo dry-run failed") {
		t.Fatalf("RevertPatch() = %q, want a dry-run failure report", got)
	}
}

func TestRevertPatchReportsApplyFailureAfterSuccessfulCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := gitRepo(t)
	if _, applied, err := ApplyPatch(goodDiff); err != nil || !applied {
		t.Fatalf("setup: ApplyPatch() = (_, %v, %v), want it applied", applied, err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	got, reverted, err := RevertPatch(goodDiff)
	if err != nil {
		t.Fatalf("RevertPatch() error = %v", err)
	}
	if reverted {
		t.Fatal("RevertPatch() reverted = true, want false when the write itself fails")
	}
	if !strings.Contains(got, "Undo failed") {
		t.Fatalf("RevertPatch() = %q, want an undo failure report", got)
	}
}

func TestRevertPatchReportsMissingGit(t *testing.T) {
	workdir(t)
	shimDir(t, nil)

	_, _, err := RevertPatch(goodDiff)
	if err == nil {
		t.Fatal("RevertPatch() = nil error, want a missing-git error")
	}
	if !strings.Contains(err.Error(), "git unavailable") {
		t.Errorf("RevertPatch() error = %v, want it to mention git unavailable", err)
	}
}
