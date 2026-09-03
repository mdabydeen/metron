package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
}

// DefaultBudgets matches metron's built-in configuration.
func DefaultBudgets() Budgets {
	return Budgets{
		MaxSliceLines:    120,
		MaxLineChars:     500,
		SearchMaxMatches: 10,
		SearchMaxPerFile: 2,
		ListMaxEntries:   60,
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
}

// NewEnv builds an Env rooted at the current repository.
func NewEnv(b Budgets) Env {
	return Env{Root: repoRoot(), Budgets: b}
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
	return real, nil
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
