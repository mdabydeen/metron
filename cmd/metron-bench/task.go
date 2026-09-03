package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Task is one benchmark task, loaded from bench/tasks/<name>/.
//
// The directory holds four things: seed/ (the starting repo), prompt.txt (what
// a user would type), verify.sh (the only judge of success) and this file.
type Task struct {
	Name           string   `json:"name"`
	Tags           []string `json:"tags"`
	TimeoutSeconds int      `json:"timeout_seconds"`

	// AllowedCommands is written into the cell's .metron.json, so a task that
	// wants the agent to be able to check its own work with `go test` says so
	// here rather than every task getting a shell.
	AllowedCommands []string `json:"allowed_commands"`

	// MaxPromptTokens, when > 0, is a ceiling on the cell's median prompt
	// tokens. This is what makes large-file-edit a test of the thesis rather
	// than just another edit: succeeding by reading the whole file is a
	// failure.
	MaxPromptTokens int `json:"max_prompt_tokens"`

	// ExpectNoChanges asserts the run reported no modified files. It is a
	// cross-check on verify.sh for the tasks whose correct answer is to
	// change nothing at all.
	ExpectNoChanges bool `json:"expect_no_changes"`

	// Dir and Prompt are filled in by loadTask, not read from JSON.
	Dir    string `json:"-"`
	Prompt string `json:"-"`
}

func (t Task) seedDir() string    { return filepath.Join(t.Dir, "seed") }
func (t Task) verifyPath() string { return filepath.Join(t.Dir, "verify.sh") }

// loadTask reads one task directory.
func loadTask(dir string) (Task, error) {
	var t Task
	data, err := os.ReadFile(filepath.Join(dir, "task.json"))
	if err != nil {
		return t, fmt.Errorf("read task: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, fmt.Errorf("parse %s/task.json: %w", dir, err)
	}
	t.Dir = dir
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		return t, fmt.Errorf("read prompt: %w", err)
	}
	t.Prompt = strings.TrimSpace(string(prompt))
	if err := t.validate(); err != nil {
		return t, fmt.Errorf("task %s: %w", dir, err)
	}
	return t, nil
}

func (t Task) validate() error {
	var problems []string
	if strings.TrimSpace(t.Name) == "" {
		problems = append(problems, "name must not be empty")
	}
	if t.TimeoutSeconds <= 0 {
		problems = append(problems, fmt.Sprintf("timeout_seconds must be > 0 (got %d)", t.TimeoutSeconds))
	}
	if t.Prompt == "" {
		problems = append(problems, "prompt.txt must not be empty")
	}
	if _, err := os.Stat(t.seedDir()); err != nil {
		problems = append(problems, "seed/ is missing")
	}
	if _, err := os.Stat(t.verifyPath()); err != nil {
		problems = append(problems, "verify.sh is missing")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// loadTasks reads every task directory under root, in name order. A non-empty
// only list restricts the suite to those task directory names, which is how a
// single task is iterated on without paying for the other nine.
func loadTasks(root string, only []string) ([]Task, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read tasks dir: %w", err)
	}
	var tasks []Task
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if len(only) > 0 && !slices.Contains(only, e.Name()) {
			continue
		}
		t, err := loadTask(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks found in %s", root)
	}
	return tasks, nil
}

// splitList parses a comma-separated flag value into a list, dropping empties
// so that "" means "no restriction" rather than "one task named nothing".
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
