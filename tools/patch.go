package tools

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ApplyPatch applies a unified diff to the project, dry-running it first so a
// bad patch leaves the tree untouched.
//
// Patch failures come back as *text* rather than as a Go error, so the model
// can read git's complaint and correct the diff itself. Only a missing git
// binary is a real error, since that is an environment fault.
func (e Env) ApplyPatch(diff string) (string, error) {
	// git apply already refuses the obvious escapes -- it rejects paths
	// containing "..", refuses to follow a symlinked directory out of the tree,
	// and treats a leading "/" as relative to the tree rather than to the
	// filesystem. This check is not making up for a hole in git; it makes the
	// boundary metron's own, stated in metron's own terms, so it still holds if
	// a future flag (--unsafe-paths, say) or a different backend loosens git's.
	for _, target := range patchTargets(diff) {
		if _, err := e.resolve(target); err != nil {
			return fmt.Sprintf("Patch rejected: %v. Do not retry this path; "+
				"patch a file inside the project instead.", err), nil
		}
	}

	// 1. Dry run verification
	checkCmd := exec.Command("git", "apply", "--check", "-")
	checkCmd.Dir = e.Root
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
	applyCmd.Dir = e.Root
	applyCmd.Stdin = strings.NewReader(diff)
	if out, err := applyCmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("Patch application failed:\n%s", string(out)), nil
	}

	return "Patch successfully applied to working tree.", nil
}

// patchHeaders are the ways a unified diff names a file it will touch. Reading
// only ---/+++ was not enough: a pure rename carries neither, so a rename diff
// slipped past this check entirely and was bounded by git alone.
var patchHeaders = []string{"--- ", "+++ ", "rename from ", "rename to ", "copy from ", "copy to "}

// patchTargets extracts the files a unified diff claims to touch. Anything it
// does not recognise is left for git to reject, which it does with a better
// message than this could produce.
func patchTargets(diff string) []string {
	var targets []string
	for _, line := range strings.Split(diff, "\n") {
		var prefix string
		for _, h := range patchHeaders {
			if strings.HasPrefix(line, h) {
				prefix = h
				break
			}
		}
		if prefix == "" {
			continue
		}
		field := strings.TrimPrefix(line, prefix)
		// A header may carry a timestamp after a tab; the path stops there.
		path := strings.TrimSpace(strings.SplitN(field, "\t", 2)[0])
		if path == "" || path == "/dev/null" {
			continue
		}
		// git C-quotes a path containing unusual bytes, so "b/\\056\\056/x" is
		// really "b/../x". Unquoting before the check is what stops an escape
		// from being hidden inside an octal one.
		if strings.HasPrefix(path, `"`) {
			if unquoted, err := strconv.Unquote(path); err == nil {
				path = unquoted
			}
		}
		// Strip git's a/ and b/ working-tree prefixes. --no-prefix diffs have
		// neither, which is why this is a trim rather than a requirement.
		for _, p := range []string{"a/", "b/"} {
			if strings.HasPrefix(path, p) {
				path = strings.TrimPrefix(path, p)
				break
			}
		}
		if path != "" {
			targets = append(targets, path)
		}
	}
	return targets
}
