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
// The path boundary is enforced before git ever runs: every file the diff
// names -- across diff --git, rename, copy, and --- / +++ headers -- is
// checked to land inside the project. git apply --check remains the backstop.
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

// patchTargets extracts the files a unified diff claims to touch. It reads every
// header form git produces -- diff --git, rename, and copy, in addition to the
// --- / +++ lines -- so a file can no longer slip past metron's check by using a
// form it did not parse. Anything it does not recognise is left for git to
// reject, which it does with a better message than this could produce.
func patchTargets(diff string) []string {
	var targets []string

	// add resolves one path the diff names down to a project-relative target and
	// records it. git's a/ and b/ working-tree prefixes are stripped (a
	// --no-prefix diff has neither, so this is a trim, not a requirement); an
	// empty path or /dev/null -- the old side of a creation or deletion -- is
	// dropped.
	add := func(raw string) {
		path := strings.TrimSpace(raw)
		for _, p := range []string{"a/", "b/"} {
			if strings.HasPrefix(path, p) {
				path = strings.TrimPrefix(path, p)
				break
			}
		}
		if path != "" && path != "/dev/null" {
			targets = append(targets, path)
		}
	}

	for _, line := range strings.Split(diff, "\n") {
		switch {
		// diff --git a/x b/y names both sides after its own two-word prefix.
		case strings.HasPrefix(line, "diff --git "):
			for _, path := range gitDiffPaths(strings.TrimPrefix(line, "diff --git ")) {
				add(path)
			}
		// git spells a rename out with a named source and target line.
		case strings.HasPrefix(line, "rename from "):
			add(gitPath(strings.TrimPrefix(line, "rename from ")))
		case strings.HasPrefix(line, "rename to "):
			add(gitPath(strings.TrimPrefix(line, "rename to ")))
		case strings.HasPrefix(line, "copy from "):
			add(gitPath(strings.TrimPrefix(line, "copy from ")))
		case strings.HasPrefix(line, "copy to "):
			add(gitPath(strings.TrimPrefix(line, "copy to ")))
		case strings.HasPrefix(line, "--- "):
			// A header may carry a timestamp after a tab; the path stops there.
			add(gitPath(strings.TrimPrefix(line, "--- ")))
		case strings.HasPrefix(line, "+++ "):
			add(gitPath(strings.TrimPrefix(line, "+++ ")))
		}
	}
	return targets
}

// gitDiffPaths splits the two paths in a diff --git header. Git quotes paths
// containing control characters, quotes, or backslashes with C-style escapes;
// parse those as one path instead of letting a whitespace split hide a path
// component. Ordinary unquoted paths may contain spaces too, so use the
// a/-to-b/ boundary rather than splitting every word. If a path itself contains
// that boundary, retain every possible pair: the extra candidates are harmless
// for in-project paths and keep an unsafe component from being hidden.
func gitDiffPaths(header string) []string {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(header, "a/") {
		var paths []string
		for i := 0; i+3 <= len(header); i++ {
			if header[i] != ' ' || header[i+1:i+3] != "b/" {
				continue
			}
			paths = append(paths, header[:i], header[i+1:])
		}
		if len(paths) > 0 {
			return paths
		}
	}

	// This also handles --no-prefix headers and unusual hand-written mixed
	// quoting. Standard Git output with quoted paths is parsed without loss.
	var paths []string
	for {
		header = strings.TrimLeft(header, " \t")
		if header == "" {
			return paths
		}
		if header[0] == '"' {
			path, rest, ok := unquoteGitPath(header)
			if !ok {
				// The malformed header is left for git apply to reject. Keep the
				// raw value as a candidate so a visibly unsafe path is not hidden.
				return append(paths, header)
			}
			paths = append(paths, path)
			header = rest
			continue
		}

		end := strings.IndexAny(header, " \t")
		if end < 0 {
			return append(paths, header)
		}
		paths = append(paths, header[:end])
		header = header[end:]
	}
}

// gitPath extracts one path-bearing header value. A quoted path ends at its
// closing quote; any following tab and timestamp are metadata, not part of the
// filename. For an unquoted header, Git uses a tab before optional metadata and
// the remainder is the path (including ordinary spaces).
func gitPath(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	if header[0] == '"' {
		if path, _, ok := unquoteGitPath(header); ok {
			return path
		}
	}
	return strings.TrimSpace(strings.SplitN(header, "\t", 2)[0])
}

// unquoteGitPath decodes the C-style quoted path format emitted by Git and
// returns the unconsumed suffix after the closing quote.
func unquoteGitPath(value string) (path, rest string, ok bool) {
	end := 1
	escaped := false
	for end < len(value) {
		switch {
		case escaped:
			escaped = false
		case value[end] == '\\':
			escaped = true
		case value[end] == '"':
			path, err := strconv.Unquote(value[:end+1])
			if err != nil {
				return "", "", false
			}
			return path, value[end+1:], true
		}
		end++
	}
	return "", "", false
}
