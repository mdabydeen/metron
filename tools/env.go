package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Budgets bounds what each tool is allowed to return. Every field was once a
// magic number; none of them may be zero at runtime, since an unbounded tool
// defeats the point of the whole program.
type Budgets struct {
	MaxSliceLines    int // widest span view_slice will read
	MaxLineChars     int // longest single line view_slice will emit
	SearchMaxMatches int // total ripgrep matches
	SearchMaxPerFile int // ripgrep matches per file
	ListMaxEntries   int // paths list_files will return

	CommandTimeout        time.Duration // wall clock a single run_command gets
	MaxCommandOutputBytes int           // combined output run_command will return
}

// DefaultBudgets matches metron's built-in configuration.
func DefaultBudgets() Budgets {
	return Budgets{
		MaxSliceLines:    120,
		MaxLineChars:     500,
		SearchMaxMatches: 10,
		SearchMaxPerFile: 2,
		ListMaxEntries:   60,

		CommandTimeout:        120 * time.Second,
		MaxCommandOutputBytes: 4000,
	}
}

// Env is the environment the tools operate in: the directory they are confined
// to, and the budgets they enforce. It is passed in rather than held globally,
// so a caller can drive the tools against a scratch tree without touching
// process state.
//
// Root is the boundary. Every path a tool touches is resolved against it and
// rejected if it lands outside -- a model asking for ~/.ssh/id_rsa or writing a
// patch to ../../etc gets a refusal rather than a file. That matters less with
// a local model reading a local repo, and a great deal the moment metron talks
// to an endpoint someone else controls.
type Env struct {
	Root    string
	Budgets Budgets

	// Allowed is the set of command prefixes run_command may execute, already
	// split into argv. An empty list -- the default -- means the tool is not
	// offered at all, so granting execution is something an operator does on
	// purpose rather than something they forget to switch off.
	Allowed [][]string

	// EditFormat selects how the model is asked to express a change: a unified
	// diff (apply_patch) or an anchored search/replace (edit_file). Empty means
	// FormatDiff, so a zero Env keeps the original behaviour.
	EditFormat string
}

// NewEnv builds an Env rooted at the current repository.
func NewEnv(b Budgets) Env {
	return Env{Budgets: b}.Rooted()
}

// Rooted fills in Root if it is not already set, leaving every other field
// alone. Callers that build an Env field by field -- setting Allowed, say --
// use this to resolve the root without having their other choices replaced.
func (e Env) Rooted() Env {
	if e.Root == "" {
		e.Root = repoRoot()
	}
	return e
}

// ParseAllowlist splits each configured command into the argv prefix a call has
// to begin with. Splitting once, here, keeps the matching in Allows a plain
// comparison rather than a parse repeated on every call.
func ParseAllowlist(commands []string) [][]string {
	var out [][]string
	for _, c := range commands {
		if fields := strings.Fields(c); len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// repoRoot returns the directory tools resolve paths against: the enclosing
// git work tree if there is one, else the working directory. Preferring the
// git root is what lets metron be run from a subdirectory of a project rather
// than only from its top level.
//
// Symlinks are resolved once, here, so that comparisons against Root are made
// between two real paths. On macOS this is not a corner case: /tmp is a symlink
// to /private/tmp, and t.TempDir() hands out paths under it.
func repoRoot() string {
	root := "."
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			root = top
		}
	} else if wd, _ := os.Getwd(); wd != "" {
		root = wd
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		return real
	}
	return root
}

// errOutsideRoot is returned for any path that escapes Root. Like the rest of
// metron's tool failures it is phrased at the model: it says what is allowed,
// so the reply is a corrected path rather than another attempt at the same one.
func (e Env) errOutsideRoot(path string) error {
	return fmt.Errorf("path %q is outside the project (%s). Only files within the project "+
		"can be read or written; use a path relative to the project root", path, e.Root)
}

// resolve turns a tool-supplied path into an absolute one inside Root, or
// refuses it. Relative paths are taken against Root, not the process working
// directory, so the answer does not depend on where metron happens to be run.
//
// The boundary is checked after symlinks are resolved, never before: a path can
// be lexically inside the project and still point out of it, and -- just as
// importantly -- one that looks outside can resolve back in. On macOS the
// second case is routine, since /var is a symlink to /private/var.
func (e Env) resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("no path given")
	}
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Root, p)
	}

	real, err := evalExisting(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	if !within(e.Root, real) {
		return "", e.errOutsideRoot(path)
	}
	if seg := forbiddenSegment(e.Root, real); seg != "" {
		return "", fmt.Errorf("path %q is inside %s, which metron will not read or write. "+
			"Edit the working tree instead", path, seg)
	}
	return real, nil
}

// forbidden names directories that are inside the project and still off limits.
// Being under the root is not the same as being safe to write: .git holds the
// configuration git itself executes, so a single edit to .git/config setting
// core.pager or core.sshCommand runs a command the next time the operator types
// `git log` -- and it shows up in no diff and no `git status`.
//
// `git apply` refuses .git paths on its own, so apply_patch was already covered;
// edit_file writes files directly and would not have been.
var forbidden = []string{".git", ".metron"}

// forbiddenSegment reports which off-limits directory a path sits under, or "".
// The comparison is case-insensitive because macOS's default filesystem is:
// ".GIT/config" and ".git/config" are the same file there.
func forbiddenSegment(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		for _, bad := range forbidden {
			if strings.EqualFold(seg, bad) {
				return bad
			}
		}
	}
	return ""
}

// evalExisting resolves symlinks in the deepest part of p that exists and
// re-appends the rest. Plain EvalSymlinks fails outright on a path that is not
// there yet, which is no good for apply_patch: it legitimately creates files.
// Resolving the existing ancestors is what stops a symlinked directory being
// used to reach outside the tree.
//
// The loop needs no explicit bound. filepath.Dir strictly shortens its argument
// until it reaches "/" (or "." for a relative path), and both of those resolve,
// so every path terminates at the success return or at a non-ENOENT error. A
// guard for "walked past the top" would be dead code: there is no filesystem on
// which the root is absent.
func evalExisting(p string) (string, error) {
	cur, rest := p, ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			// Join cleans, and Join(x, "") is x, so the fully-existing case
			// needs no special handling.
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = filepath.Dir(cur)
	}
}

// within reports whether p is root or sits beneath it. The separator matters:
// a plain prefix test would accept /srv/project-secrets for a root of
// /srv/project.
func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// rel renders an absolute path back as project-relative, so tool output the
// model reads never contains the operator's home directory.
func (e Env) rel(p string) string {
	if r, err := filepath.Rel(e.Root, p); err == nil {
		return r
	}
	return p
}
