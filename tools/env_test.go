package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultBudgetsAreAllPositive(t *testing.T) {
	b := DefaultBudgets()
	for name, v := range map[string]int{
		"MaxSliceLines":    b.MaxSliceLines,
		"MaxLineChars":     b.MaxLineChars,
		"SearchMaxMatches": b.SearchMaxMatches,
		"SearchMaxPerFile": b.SearchMaxPerFile,
		"ListMaxEntries":   b.ListMaxEntries,
	} {
		if v <= 0 {
			t.Errorf("DefaultBudgets().%s = %d, want a positive budget", name, v)
		}
	}
}

func TestNewEnvPrefersTheGitWorkTree(t *testing.T) {
	dir := workdir(t)
	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// A git that reports the tree root regardless of where it is invoked, which
	// is what the real one does from a subdirectory.
	shimDir(t, map[string]string{"git": "echo " + dir + "\n"})
	t.Chdir(sub)

	env := NewEnv(DefaultBudgets())

	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if env.Root != real {
		t.Fatalf("NewEnv().Root = %q, want the work-tree root %q", env.Root, real)
	}
	if env.Budgets != DefaultBudgets() {
		t.Fatalf("NewEnv().Budgets = %+v, want the budgets it was given", env.Budgets)
	}
}

func TestRepoRootFallsBackToTheWorkingDirectory(t *testing.T) {
	dir := workdir(t)
	// No git on PATH at all: the working directory is the only answer left.
	shimDir(t, nil)

	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := repoRoot(); got != real {
		t.Fatalf("repoRoot() = %q, want the working directory %q", got, real)
	}
}

func TestRepoRootIgnoresEmptyGitOutput(t *testing.T) {
	workdir(t)
	// git present and succeeding, but saying nothing -- treated as no answer.
	shimDir(t, map[string]string{"git": "exit 0\n"})

	if got := repoRoot(); got != "." {
		t.Fatalf("repoRoot() = %q, want %q when git reports no work tree", got, ".")
	}
}

func TestRepoRootKeepsAnUnresolvablePath(t *testing.T) {
	workdir(t)
	shimDir(t, map[string]string{"git": "echo /no/such/directory\n"})

	if got := repoRoot(); got != "/no/such/directory" {
		t.Fatalf("repoRoot() = %q, want the path git reported even though it does not exist", got)
	}
}

func TestResolveRejectsAnEmptyPath(t *testing.T) {
	dir := workdir(t)
	env := Env{Root: dir}

	for _, path := range []string{"", "   "} {
		if _, err := env.resolve(path); err == nil {
			t.Fatalf("resolve(%q) = nil error, want a refusal", path)
		}
	}
}

func TestResolveAcceptsPathsInsideTheProject(t *testing.T) {
	dir := workdir(t)
	writeFile(t, "inside.go", "package main\n")
	env := defaultEnv(t)

	for _, path := range []string{"inside.go", "./inside.go", filepath.Join(dir, "inside.go")} {
		got, err := env.resolve(path)
		if err != nil {
			t.Fatalf("resolve(%q) error = %v, want it accepted", path, err)
		}
		if !strings.HasSuffix(got, "inside.go") {
			t.Fatalf("resolve(%q) = %q, want it to land on the file", path, got)
		}
	}
}

func TestResolveRefusesPathsOutsideTheProject(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "shh\n")

	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	env := defaultEnv(t)

	for _, path := range []string{
		filepath.Join(outside, "secret.txt"), // absolute, elsewhere
		"../",                                // straight out of the tree
		"../../etc/passwd",                   // and further
	} {
		if _, err := env.resolve(path); err == nil {
			t.Fatalf("resolve(%q) = nil error, want it refused as outside the project", path)
		}
	}
}

func TestResolveRefusesASiblingWithAMatchingPrefix(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	sibling := filepath.Join(base, "project-secrets")
	for _, d := range []string{project, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(sibling, "keys.txt"), "shh\n")
	t.Chdir(project)

	// A plain string-prefix test would accept this; the separator is what makes
	// the boundary a directory boundary.
	if _, err := defaultEnv(t).resolve(filepath.Join(sibling, "keys.txt")); err == nil {
		t.Fatal("resolve() accepted a sibling directory sharing the project's prefix")
	}
}

func TestResolveRefusesEscapeThroughASymlink(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "shh\n")

	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Chdir(project)

	if _, err := defaultEnv(t).resolve("escape/secret.txt"); err == nil {
		t.Fatal("resolve() followed a symlink out of the project")
	}
}

func TestResolveAllowsAFileThatDoesNotExistYet(t *testing.T) {
	workdir(t)
	if err := os.MkdirAll("pkg", 0o755); err != nil {
		t.Fatal(err)
	}

	// apply_patch legitimately creates files, so a path whose leaf is missing
	// has to resolve rather than error.
	got, err := defaultEnv(t).resolve("pkg/brand-new.go")
	if err != nil {
		t.Fatalf("resolve() error = %v, want a not-yet-existing file accepted", err)
	}
	if !strings.HasSuffix(got, filepath.Join("pkg", "brand-new.go")) {
		t.Fatalf("resolve() = %q, want it to name the new file", got)
	}
}

func TestResolveRefusesANewFileBehindASymlink(t *testing.T) {
	outside := t.TempDir()
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Chdir(project)

	// The leaf does not exist, so only resolving the *ancestors* catches this.
	if _, err := defaultEnv(t).resolve("escape/planted.txt"); err == nil {
		t.Fatal("resolve() would have created a file outside the project")
	}
}

func TestEvalExistingSurfacesNonExistenceErrors(t *testing.T) {
	dir := workdir(t)
	writeFile(t, "regular.txt", "not a directory\n")

	// Walking through a regular file yields ENOTDIR rather than ENOENT, and
	// that is a real error rather than "keep looking further up".
	if _, err := evalExisting(filepath.Join(dir, "regular.txt", "child")); err == nil {
		t.Fatal("evalExisting() = nil error, want the ENOTDIR surfaced")
	}
}

func TestWithinTreatsTheRootItselfAsInside(t *testing.T) {
	if !within("/srv/project", "/srv/project") {
		t.Error("within() rejected the root itself")
	}
	if !within("/srv/project", "/srv/project/pkg/file.go") {
		t.Error("within() rejected a descendant")
	}
	if within("/srv/project", "/srv/project-secrets/keys.txt") {
		t.Error("within() accepted a sibling with a matching prefix")
	}
}

func TestResolveSurfacesAnUnwalkablePath(t *testing.T) {
	workdir(t)
	writeFile(t, "regular.txt", "not a directory\n")

	// The ancestor exists but is a file, so the walk cannot continue. That is a
	// real error rather than "keep looking further up".
	if _, err := defaultEnv(t).resolve("regular.txt/child.go"); err == nil {
		t.Fatal("resolve() = nil error, want the failure surfaced")
	}
}

func TestResolveWalksUpToAnExistingAncestor(t *testing.T) {
	// repoRoot keeps whatever git reported even when it cannot be resolved, so
	// a root that does not exist is a real configuration. Resolution still
	// terminates: the walk climbs to "/", which always resolves.
	env := Env{Root: "/no/such/project", Budgets: DefaultBudgets()}

	got, err := env.resolve("main.go")
	if err != nil {
		t.Fatalf("resolve() error = %v, want the walk to terminate at the filesystem root", err)
	}
	if got != "/no/such/project/main.go" {
		t.Fatalf("resolve() = %q, want the path under the configured root", got)
	}
}

func TestForbiddenSegmentHandlesAnUnrelatedPath(t *testing.T) {
	// filepath.Rel fails when the paths share no root; that must not be read
	// as "no forbidden segment found" on a path that was already refused by
	// the within check anyway.
	if got := forbiddenSegment("/a/b", "relative/path"); got != "" {
		t.Fatalf("forbiddenSegment() = %q, want no segment for an unrelatable path", got)
	}
}

func TestRelFallsBackToTheAbsolutePath(t *testing.T) {
	// Rel fails when the paths share no root. Tool output should still say
	// something rather than nothing.
	if got := (Env{Root: "/a/b"}).rel("relative/path"); got != "relative/path" {
		t.Fatalf("rel() = %q, want the path passed through", got)
	}
}
