package tools

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Dependency describes one external binary a tool depends on.
type Dependency struct {
	Binary string // executable looked up on PATH
	Tool   string // the metron tool that stops working without it
	Hint   string // what the operator should do about it
}

// dependencies is the full set of external programs metron shells out to.
var dependencies = []Dependency{
	{Binary: "rg", Tool: "search_text", Hint: "install ripgrep (brew install ripgrep)"},
	{Binary: "ctags", Tool: "find_symbol", Hint: "install Universal Ctags (brew install universal-ctags)"},
	{Binary: "git", Tool: "apply_patch", Hint: "install git"},
}

// Preflight checks that every external dependency is present and usable, and
// returns one human-readable warning per problem found. An empty result means
// all four tools are ready. Nothing here is fatal: a missing binary only
// disables the tool that needs it, and the model is told so at the point of
// failure.
func Preflight() []string {
	var warnings []string
	for _, dep := range dependencies {
		if _, err := exec.LookPath(dep.Binary); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"%s not found on PATH - %s is unavailable (%s)", dep.Binary, dep.Tool, dep.Hint))
			continue
		}
		if dep.Binary == "ctags" && !isUniversalCtags() {
			warnings = append(warnings, fmt.Sprintf(
				"ctags on PATH is not Universal Ctags - find_symbol is unavailable (%s)", dep.Hint))
		}
		if dep.Binary == "git" && !insideWorkTree() {
			warnings = append(warnings,
				"the working directory is not a git repository - apply_patch will fail "+
					"(run metron from a repo root)")
		}
	}
	return warnings
}

// isUniversalCtags reports whether the ctags on PATH is Universal Ctags. The
// BSD ctags shipped with macOS does not understand the --fields=+nK flag
// EnsureTags relies on, and fails this check.
func isUniversalCtags() bool {
	out, err := exec.Command("ctags", "--version").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Universal Ctags")
}

// insideWorkTree reports whether the working directory is inside a git
// repository. Having the git binary is not enough: apply_patch shells out to
// `git apply`, which outside a repository fails with a message the model then
// wastes turns trying to fix as if it were a bad diff.
func insideWorkTree() bool {
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// RebuildTags discards any existing ctags index and builds a fresh one. The
// index is otherwise generated once and never invalidated, so this is how an
// operator picks up renames and new files mid-session.
func RebuildTags() error {
	if err := os.Remove(".tags"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale index: %w", err)
	}
	if err := EnsureTags(); err != nil {
		return fmt.Errorf("rebuild ctags index: %w", err)
	}
	return nil
}
