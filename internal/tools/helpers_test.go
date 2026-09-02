package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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
