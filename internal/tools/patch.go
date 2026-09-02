package tools

import (
	"fmt"
	"os/exec"
	"strings"
)

func ApplyPatch(diff string) (string, error) {
	// 1. Dry run verification
	checkCmd := exec.Command("git", "apply", "--check", "-")
	checkCmd.Stdin = strings.NewReader(diff)
	if out, err := checkCmd.CombinedOutput(); err != nil {
		// A missing git binary is an environment fault, not a bad patch.
		if missing := missingBinary(err, "git", "apply_patch",
			"report the change you would make instead"); missing != nil {
			return "", fmt.Errorf("git unavailable: %w", missing)
		}
		return fmt.Sprintf("Patch dry-run failed:\n%s\nVerify target paths and line numbers.", string(out)), nil
	}

	// 2. Real application
	applyCmd := exec.Command("git", "apply", "-")
	applyCmd.Stdin = strings.NewReader(diff)
	if out, err := applyCmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("Patch application failed:\n%s", string(out)), nil
	}

	return "Patch successfully applied to working tree.", nil
}
