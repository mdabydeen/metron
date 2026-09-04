package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unicode/utf8"
)

// testEnv returns an Env rooted at the test's working directory. Root is taken
// from the process rather than resolved through git, so the temp directory a
// test is chdir'd into is the boundary regardless of whether it happens to sit
// inside a repository. repoRoot's own behaviour is tested separately.
func testEnv(t *testing.T, b Budgets) Env {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	real, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatalf("resolve %s: %v", wd, err)
	}
	return Env{Root: real, Budgets: b}
}

// defaultEnv is testEnv with the built-in budgets.
func defaultEnv(t *testing.T) Env {
	t.Helper()
	return testEnv(t, DefaultBudgets())
}

// shimDir creates a directory containing executable shell-script stand-ins for
// external binaries and prepends it to PATH for the duration of the test.
// Passing no scripts yields an empty PATH entry, which makes every external
// lookup fail -- that is how the "binary missing" branches are exercised.
func shimDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script shims require a POSIX shell")
	}
	dir := t.TempDir()
	for name, body := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatalf("write shim %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// workdir switches the test into a scratch directory, since the tools operate
// on relative paths (".tags", ".") in the process working directory.
func workdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fileExists is a small helper for tests asserting that something was *not*
// created.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// utf8ValidString is re-exported for tests so they do not each import unicode/utf8.
func utf8ValidString(s string) bool { return utf8.ValidString(s) }
