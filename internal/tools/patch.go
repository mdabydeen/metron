package tools

import (
	"fmt"
	"os/exec"
	"strings"
)

// ApplyPatch applies diff via git apply. The applied bool tells the caller
// whether the patch actually changed the working tree -- distinct from err,
// which is reserved for environment faults (a missing git binary); a rejected
// or malformed diff is reported as text with applied=false so the model can
// see and correct its own mistake, matching the rest of this package's
// text-not-error convention for user-facing failures.
func ApplyPatch(diff string) (msg string, applied bool, err error) {
	// 1. Dry run verification
	checkCmd := exec.Command("git", "apply", "--check", "-")
	checkCmd.Stdin = strings.NewReader(diff)
	if out, cerr := checkCmd.CombinedOutput(); cerr != nil {
		// A missing git binary is an environment fault, not a bad patch.
		if missing := missingBinary(cerr, "git", "apply_patch",
			"report the change you would make instead"); missing != nil {
			return "", false, fmt.Errorf("git unavailable: %w", missing)
		}
		return fmt.Sprintf("Patch dry-run failed:\n%s\nVerify target paths and line numbers.", string(out)), false, nil
	}

	// 2. Real application
	applyCmd := exec.Command("git", "apply", "-")
	applyCmd.Stdin = strings.NewReader(diff)
	if out, aerr := applyCmd.CombinedOutput(); aerr != nil {
		return fmt.Sprintf("Patch application failed:\n%s", string(out)), false, nil
	}

	return "Patch successfully applied to working tree.", true, nil
}

// RevertPatch undoes a previously applied diff via `git apply -R`, mirroring
// ApplyPatch's dry-run-then-apply structure -- including the (msg, reverted,
// err) shape, so a caller can tell a real revert from a rejected one without
// string-matching msg, the same reason ApplyPatch reports applied explicitly.
// A diff that can't be cleanly reversed (e.g. the file has changed since) is
// reported as text with reverted=false rather than silently corrupting the
// tree.
func RevertPatch(diff string) (msg string, reverted bool, err error) {
	checkCmd := exec.Command("git", "apply", "-R", "--check", "-")
	checkCmd.Stdin = strings.NewReader(diff)
	if out, cerr := checkCmd.CombinedOutput(); cerr != nil {
		if missing := missingBinary(cerr, "git", "undo",
			"the working tree may have changed since the patch was applied"); missing != nil {
			return "", false, fmt.Errorf("git unavailable: %w", missing)
		}
		return fmt.Sprintf("Undo dry-run failed:\n%s\nThe file may have changed since the patch was applied.", string(out)), false, nil
	}

	revertCmd := exec.Command("git", "apply", "-R", "-")
	revertCmd.Stdin = strings.NewReader(diff)
	if out, rerr := revertCmd.CombinedOutput(); rerr != nil {
		return fmt.Sprintf("Undo failed:\n%s", string(out)), false, nil
	}

	return "Reverted the last applied patch.", true, nil
}
