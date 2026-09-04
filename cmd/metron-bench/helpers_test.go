package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireGit skips a test on a machine without git. The benchmark cannot work
// without it -- every scratch repository is a real one -- but the default test
// suite still has to run on a machine that lacks it.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// requireNonRoot skips the permission-failure cases, which root does not hit.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
}

// writeTask lays out a minimal task directory and returns its path.
func writeTask(t *testing.T, root, name string, meta map[string]any, prompt, verify string, seed map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if meta == nil {
		meta = map[string]any{}
	}
	if _, ok := meta["name"]; !ok {
		meta["name"] = name
	}
	if _, ok := meta["timeout_seconds"]; !ok {
		meta["timeout_seconds"] = 30
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(dir, "seed"))
	writeFile(t, filepath.Join(dir, "task.json"), string(data), 0o644)
	writeFile(t, filepath.Join(dir, "prompt.txt"), prompt, 0o644)
	writeFile(t, filepath.Join(dir, "verify.sh"), verify, 0o755)
	for path, content := range seed {
		full := filepath.Join(dir, "seed", path)
		mkdir(t, filepath.Dir(full))
		writeFile(t, full, content, 0o644)
	}
	return dir
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// fakeMetron writes an executable stand-in for the metron binary. The body is
// a shell script, the same shimming trick internal/tools uses: it proves the
// runner end to end -- argv, working directory, stdout contract, exit code --
// without an Ollama server anywhere in sight.
func fakeMetron(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metron")
	writeFile(t, path, "#!/bin/sh\n"+body, 0o755)
	return path
}

// okOutput is a well-formed --json report.
const okOutput = `{"answer":"done","ok":true,"error":"","turns":2,` +
	`"tools":[{"name":"view_slice","ms":3}],` +
	`"usage":{"prompt":1200,"generated":80},"files_changed":["a.go"]}`
