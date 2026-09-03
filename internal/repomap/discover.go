package repomap

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// maxFilesExamined bounds discovery. A tree larger than this is one where
	// the map was never going to show more than its first page anyway.
	maxFilesExamined = 5000

	// churnCommits is the window churn is counted over. Long enough to see what
	// a project has been working on, short enough that a rewrite three years
	// ago does not still dominate the ranking.
	churnCommits = 200

	// gitTimeout keeps a wedged git (a stale index lock, a filesystem that is
	// not answering) from holding up session start. Losing the ranking is a
	// much smaller cost than hanging before the first prompt.
	gitTimeout = 5 * time.Second
)

// skipped are directories never descended into. They are either metron's own
// state, or trees that are large, generated and uninteresting to a model
// orienting itself. Inside a git repository .gitignore already excludes most of
// this, so the list matters most in the walk fallback.
var skipped = []string{".git", ".metron", "vendor", "testdata", "node_modules"}

func skipDir(name string) bool {
	for _, s := range skipped {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

// discover returns the project's files as slash-separated paths relative to
// root, preferring git's view of the tree. `git ls-files` is the cheapest
// correct answer to "what is source here?" -- it applies .gitignore, the global
// excludes and .git/info/exclude, none of which is worth reimplementing -- and
// the walk is the fallback for a directory that is not a repository.
// The limit is threaded through rather than read from the constant so that a
// test can exercise the truncation without laying down five thousand files.
func discover(root string, limit int) []string {
	if paths, ok := gitFiles(root, limit); ok {
		return paths
	}
	return walkFiles(root, limit)
}

// gitFiles lists tracked and untracked-but-not-ignored files. Untracked ones
// are included on purpose: a file the operator has just created is likely to be
// exactly what the session is about.
//
// Every path is checked with Lstat rather than trusted. The index can name
// files that are no longer on disk, and it can name symlinks -- which are how a
// path inside the repository points at something outside it.
func gitFiles(root string, limit int) ([]string, bool) {
	out, err := git(root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, false
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if len(paths) >= limit {
			break
		}
		if !usable(root, p) {
			continue
		}
		paths = append(paths, p)
	}
	return paths, true
}

// usable rejects anything that is not a plain file inside the tree. The lexical
// checks come first because they are the security-relevant ones: a path that
// escapes root must be refused whether or not it exists.
func usable(root, p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || skipDir(seg) {
			return false
		}
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
	return err == nil && info.Mode().IsRegular()
}

// walkFiles is the fallback outside a repository. It uses WalkDir, which reads
// directory entries without following symlinks, so a link pointing out of the
// tree is reported as a link and skipped rather than traversed.
func walkFiles(root string, limit int) []string {
	var paths []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory costs its own subtree, not the map.
			return nil
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(paths) >= limit {
			return fs.SkipAll
		}
		paths = append(paths, filepath.ToSlash(p[len(root)+1:]))
		return nil
	})
	return paths
}

// churnCounts returns how many of the last churnCommits commits touched each
// path, and whether git could answer at all. It reports false outside a
// repository and inside one with no commits yet, which is the signal render
// uses to say the map is not churn-ranked.
func churnCounts(root string) (map[string]int, bool) {
	out, err := git(root, "log", "--name-only", "--pretty=format:", "-n", strconv.Itoa(churnCommits))
	if err != nil {
		return nil, false
	}
	counts := make(map[string]int)
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			counts[p]++
		}
	}
	return counts, true
}

// git runs one git command in root. cmd.Dir is what confines it: without it git
// would act on whatever directory the process happens to be in, which for a
// long-lived REPL is not necessarily the project it was started for.
func git(root string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	return cmd.Output()
}
