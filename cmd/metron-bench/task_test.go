package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTask(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "demo",
		map[string]any{"tags": []string{"edit"}, "timeout_seconds": 42, "allowed_commands": []string{"go test"}},
		"do the thing\n", "#!/bin/sh\nexit 0\n", map[string]string{"a.go": "package a\n"})
	task, err := loadTask(filepath.Join(root, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Name != "demo" || task.TimeoutSeconds != 42 || task.Prompt != "do the thing" {
		t.Fatalf("unexpected task: %+v", task)
	}
	if len(task.AllowedCommands) != 1 {
		t.Fatalf("allowed commands = %v", task.AllowedCommands)
	}
}

func TestLoadTaskRejectsBadInput(t *testing.T) {
	root := t.TempDir()

	t.Run("no task.json", func(t *testing.T) {
		_, err := loadTask(filepath.Join(root, "missing"))
		if err == nil || !strings.Contains(err.Error(), "read task") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("malformed task.json", func(t *testing.T) {
		dir := filepath.Join(root, "bad")
		mkdir(t, dir)
		writeFile(t, filepath.Join(dir, "task.json"), `{"name":`, 0o644)
		_, err := loadTask(dir)
		if err == nil || !strings.Contains(err.Error(), "parse") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no prompt.txt", func(t *testing.T) {
		dir := filepath.Join(root, "noprompt")
		mkdir(t, dir)
		writeFile(t, filepath.Join(dir, "task.json"), `{"name":"x","timeout_seconds":1}`, 0o644)
		_, err := loadTask(dir)
		if err == nil || !strings.Contains(err.Error(), "read prompt") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("fails validation", func(t *testing.T) {
		dir := filepath.Join(root, "invalid")
		mkdir(t, dir)
		writeFile(t, filepath.Join(dir, "task.json"), `{"name":"","timeout_seconds":0}`, 0o644)
		writeFile(t, filepath.Join(dir, "prompt.txt"), "  \n", 0o644)
		_, err := loadTask(dir)
		if err == nil {
			t.Fatal("want an error")
		}
		for _, want := range []string{
			"name must not be empty",
			"timeout_seconds must be > 0",
			"prompt.txt must not be empty",
			"seed/ is missing",
			"verify.sh is missing",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %v does not mention %q", err, want)
			}
		}
	})
}

func TestLoadTasks(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "alpha", nil, "a\n", "#!/bin/sh\n", map[string]string{"a.go": "package a\n"})
	writeTask(t, root, "beta", nil, "b\n", "#!/bin/sh\n", map[string]string{"b.go": "package b\n"})
	// A stray file next to the task directories must be ignored.
	writeFile(t, filepath.Join(root, "README.md"), "notes\n", 0o644)

	all, err := loadTasks(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "alpha" {
		t.Fatalf("tasks = %+v", all)
	}

	only, err := loadTasks(root, []string{"beta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Name != "beta" {
		t.Fatalf("filtered tasks = %+v", only)
	}
}

func TestLoadTasksErrors(t *testing.T) {
	t.Run("no such directory", func(t *testing.T) {
		if _, err := loadTasks(filepath.Join(t.TempDir(), "nope"), nil); err == nil ||
			!strings.Contains(err.Error(), "read tasks dir") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		if _, err := loadTasks(t.TempDir(), nil); err == nil ||
			!strings.Contains(err.Error(), "no tasks found") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("broken task", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "broken"))
		if _, err := loadTasks(root, nil); err == nil ||
			!strings.Contains(err.Error(), "read task") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestSplitList(t *testing.T) {
	if got := splitList(""); got != nil {
		t.Fatalf("splitList(\"\") = %v, want nil", got)
	}
	got := splitList(" a , ,b ")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitList = %v", got)
	}
}

// TestTaskSuiteIsWellFormed loads the shipped suite and asserts that every
// verifier actually discriminates: it must reject the untouched seed. A
// verifier that passes before the model has done anything measures nothing.
//
// no-such-symbol is the deliberate exception -- an untouched tree is the
// correct outcome there, which is exactly what makes it worth having.
func TestTaskSuiteIsWellFormed(t *testing.T) {
	requireGit(t)
	root, err := filepath.Abs(filepath.Join("..", "..", "bench", "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := loadTasks(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 10 {
		t.Fatalf("expected 10 tasks, found %d", len(tasks))
	}
	for _, task := range tasks {
		t.Run(task.Name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "scratch")
			if err := prepareScratch(dir, task, cellConfig{Model: "m", EditFormat: "diff"}); err != nil {
				t.Fatal(err)
			}
			// The task's own path, exactly as the runner uses it: the
			// scratch repository is the working directory, so anything
			// relative here would resolve against the wrong tree.
			ok, out := runVerify(t.Context(), task.verifyPath(), dir)
			wantOK := task.Name == "no-such-symbol"
			if ok != wantOK {
				t.Fatalf("verify on the untouched seed = %v, want %v: %s", ok, wantOK, out)
			}
		})
	}
}
