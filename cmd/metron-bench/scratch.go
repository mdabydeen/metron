package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// copyTree copies src to dst, creating dst. Only regular files and
// directories are copied: a seed is a repository, not a place for devices or
// symlinks, and refusing to reproduce them keeps a seed from depending on
// anything outside its own directory.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, strings.TrimPrefix(path, src))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// gitRun runs one git command in dir. Identity and signing are forced on the
// command line rather than read from the operator's global config, so a
// machine with no user.email -- or with commit signing on -- still produces a
// seed commit.
func gitRun(dir string, args ...string) error {
	argv := append([]string{
		"-c", "user.name=metron-bench",
		"-c", "user.email=bench@metron.invalid",
		"-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// prepareScratch builds a fresh working copy of the task's seed at dir: a git
// repository with the seed committed, plus the cell's .metron.json.
//
// The commit is what lets verify.sh ask git what changed instead of being
// handed a claim by the thing it is judging.
func prepareScratch(dir string, t Task, cfg cellConfig) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := copyTree(t.seedDir(), dir); err != nil {
		return fmt.Errorf("copy seed: %w", err)
	}
	if err := writeCellConfig(filepath.Join(dir, ".metron.json"), cfg); err != nil {
		return err
	}
	// find_symbol writes a .tags index into the working directory. Without
	// this it shows up as an untracked file, and the tasks that pass only
	// when the tree is untouched would fail because the agent looked.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".tags\n"), 0o644); err != nil {
		return err
	}
	for _, argv := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
	} {
		if err := gitRun(dir, argv...); err != nil {
			return err
		}
	}
	return nil
}

// cellConfig is the .metron.json written for one cell. Only the fields the
// benchmark varies are set; everything else stays at metron's own defaults,
// because a benchmark that quietly retunes the budgets is not measuring the
// shipped product.
type cellConfig struct {
	Endpoint           string   `json:"endpoint"`
	Model              string   `json:"model"`
	EditFormat         string   `json:"edit_format"`
	AllowedCommands    []string `json:"allowed_commands"`
	AutoApprovePatches bool     `json:"auto_approve_patches"`
	TimeoutSeconds     int      `json:"timeout_seconds"`
}

func writeCellConfig(path string, cfg cellConfig) error {
	if cfg.AllowedCommands == nil {
		cfg.AllowedCommands = []string{}
	}
	// cellConfig is plain scalars and a string slice: marshalling it cannot
	// fail, and an error branch here would be dead code the tests could
	// never reach.
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// runVerify runs a task's verify.sh with the scratch directory as its working
// directory and reports whether it exited 0.
//
// The script is passed to /bin/sh as an argument rather than interpolated into
// a shell string, and it is handed no information about what the model said --
// the only evidence it gets is the repository the model left behind.
func runVerify(ctx context.Context, script, dir string) (bool, string) {
	cmd := exec.CommandContext(ctx, "/bin/sh", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	return err == nil, strings.TrimSpace(string(out))
}
