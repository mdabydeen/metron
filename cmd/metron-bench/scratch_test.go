package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	mkdir(t, filepath.Join(src, "pkg"))
	writeFile(t, filepath.Join(src, "a.go"), "package a\n", 0o644)
	writeFile(t, filepath.Join(src, "pkg", "b.go"), "package b\n", 0o644)
	// A symlink is skipped rather than reproduced: a seed must not depend on
	// anything outside its own directory.
	if err := os.Symlink(filepath.Join(src, "a.go"), filepath.Join(src, "link.go")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "pkg", "b.go")); err != nil || string(got) != "package b\n" {
		t.Fatalf("copied file = %q, err %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link.go")); err == nil {
		t.Fatal("symlink was copied")
	}
}

func TestCopyTreeErrors(t *testing.T) {
	t.Run("missing source", func(t *testing.T) {
		if err := copyTree(filepath.Join(t.TempDir(), "nope"), t.TempDir()); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		requireNonRoot(t)
		src := t.TempDir()
		secret := filepath.Join(src, "secret.go")
		writeFile(t, secret, "package a\n", 0o000)
		t.Cleanup(func() { _ = os.Chmod(secret, 0o644) })
		if err := copyTree(src, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("unwritable destination", func(t *testing.T) {
		requireNonRoot(t)
		src := t.TempDir()
		writeFile(t, filepath.Join(src, "a.go"), "package a\n", 0o644)
		dst := t.TempDir()
		if err := os.Chmod(dst, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dst, 0o755) })
		if err := copyTree(src, dst); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestGitRunReportsFailure(t *testing.T) {
	requireGit(t)
	err := gitRun(t.TempDir(), "rev-parse", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "git rev-parse HEAD") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareScratch(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	dir := writeTask(t, root, "demo", nil, "p\n", "#!/bin/sh\nexit 0\n",
		map[string]string{"a.go": "package a\n"})
	task, err := loadTask(dir)
	if err != nil {
		t.Fatal(err)
	}

	scratch := filepath.Join(t.TempDir(), "scratch")
	cfg := cellConfig{
		Endpoint: "http://x/api/chat", Model: "m", EditFormat: "diff",
		AutoApprovePatches: true, TimeoutSeconds: 30,
	}
	if err := prepareScratch(scratch, task, cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(scratch, "a.go")); err != nil {
		t.Fatalf("seed not copied: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(scratch, ".metron.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"model": "m"`, `"edit_format": "diff"`, `"allowed_commands": []`, `"auto_approve_patches": true`} {
		if !strings.Contains(string(written), want) {
			t.Fatalf(".metron.json %s lacks %s", written, want)
		}
	}
	// The seed is committed, so verify.sh can ask git what the model changed.
	if err := gitRun(scratch, "diff", "--quiet", "HEAD"); err != nil {
		t.Fatalf("seed commit is not clean: %v", err)
	}
	ignore, err := os.ReadFile(filepath.Join(scratch, ".gitignore"))
	if err != nil || !strings.Contains(string(ignore), ".tags") {
		t.Fatalf(".gitignore = %q, err %v", ignore, err)
	}
}

func TestPrepareScratchErrors(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	dir := writeTask(t, root, "demo", nil, "p\n", "#!/bin/sh\nexit 0\n",
		map[string]string{"a.go": "package a\n"})
	task, err := loadTask(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("directory cannot be created", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		writeFile(t, blocker, "", 0o644)
		if err := prepareScratch(filepath.Join(blocker, "scratch"), task, cellConfig{}); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("seed cannot be copied", func(t *testing.T) {
		broken := task
		broken.Dir = filepath.Join(t.TempDir(), "gone")
		err := prepareScratch(filepath.Join(t.TempDir(), "scratch"), broken, cellConfig{})
		if err == nil || !strings.Contains(err.Error(), "copy seed") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("scratch is read-only", func(t *testing.T) {
		requireNonRoot(t)
		scratch := filepath.Join(t.TempDir(), "scratch")
		if err := prepareScratch(scratch, task, cellConfig{}); err != nil {
			t.Fatal(err)
		}
		// Re-preparing a read-only directory fails at the config write.
		if err := os.Chmod(scratch, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(scratch, 0o755) })
		if err := prepareScratch(scratch, task, cellConfig{}); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("config cannot be written", func(t *testing.T) {
		scratch := filepath.Join(t.TempDir(), "scratch")
		mkdir(t, filepath.Join(scratch, ".metron.json"))
		if err := prepareScratch(scratch, task, cellConfig{}); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("gitignore cannot be written", func(t *testing.T) {
		scratch := filepath.Join(t.TempDir(), "scratch")
		mkdir(t, scratch)
		writeFile(t, filepath.Join(scratch, ".metron.json"), "{}", 0o644)
		// A directory where the file belongs: the seed copy and the config
		// write both succeed, and .gitignore is what fails.
		mkdir(t, filepath.Join(scratch, ".gitignore"))
		if err := prepareScratch(scratch, task, cellConfig{}); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("git refuses", func(t *testing.T) {
		scratch := filepath.Join(t.TempDir(), "scratch")
		// A file where .git must go makes `git init` fail.
		mkdir(t, scratch)
		writeFile(t, filepath.Join(scratch, ".git"), "not a directory\n", 0o644)
		err := prepareScratch(scratch, task, cellConfig{})
		if err == nil || !strings.Contains(err.Error(), "git") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunVerify(t *testing.T) {
	dir := t.TempDir()
	pass := filepath.Join(dir, "pass.sh")
	writeFile(t, pass, "#!/bin/sh\npwd\nexit 0\n", 0o755)
	fail := filepath.Join(dir, "fail.sh")
	writeFile(t, fail, "#!/bin/sh\necho nope\nexit 3\n", 0o755)

	work := t.TempDir()
	ok, out := runVerify(t.Context(), pass, work)
	if !ok {
		t.Fatalf("expected a pass, got %q", out)
	}
	ok, out = runVerify(t.Context(), fail, work)
	if ok || out != "nope" {
		t.Fatalf("ok = %v, out = %q", ok, out)
	}
}
