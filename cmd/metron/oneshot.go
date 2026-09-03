package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"github.com/mdabydeen/metron/internal/agent"
	"github.com/mdabydeen/metron/internal/tools"
)

// Result is what `metron -p ... --json` prints: exactly one object on stdout,
// and nothing else. It exists so a caller -- the benchmark first among them --
// can read what a run cost without scraping prose.
type Result struct {
	Answer       string          `json:"answer"`
	OK           bool            `json:"ok"`
	Error        string          `json:"error,omitempty"`
	Turns        int             `json:"turns"`
	Tools        []agent.ToolRun `json:"tools"`
	Usage        usage           `json:"usage"`
	FilesChanged []string        `json:"files_changed"`
}

type usage struct {
	Prompt    int `json:"prompt"`
	Generated int `json:"generated"`
}

// oneShot runs a single request. Without --json it prints only the answer on
// stdout, so `metron -p ... 2>/dev/null` is pipeable; with --json it prints one
// object instead. Progress and warnings always go to stderr.
func oneShot(ctx context.Context, out, errOut io.Writer, env tools.Env, bot stepper, sess *recorder, f flags) int {
	for _, w := range env.Preflight() {
		fmt.Fprintf(errOut, "warning: %s\n", w)
	}

	before := trackedChanges(env.Root)
	answer, err := step(ctx, bot, f.prompt)
	sess.save(bot)

	if !f.asJSON {
		if err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, answer)
		return 0
	}

	res := Result{
		Answer:       answer,
		OK:           err == nil,
		Turns:        len(bot.LastTools()),
		Tools:        bot.LastTools(),
		FilesChanged: changedSince(env.Root, before),
	}
	if res.Tools == nil {
		// An empty list reads better than null for anything consuming this.
		res.Tools = []agent.ToolRun{}
	}
	u, _ := bot.LastUsage()
	res.Usage = usage{Prompt: u.PromptTokens, Generated: u.GenTokens}
	if err != nil {
		res.Error = err.Error()
	}

	// A failed run still emits valid JSON. A caller that has to distinguish
	// "metron failed" from "metron produced no output" has a worse job than it
	// needs to, and the exit code already carries the verdict.
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(res); encErr != nil {
		fmt.Fprintf(errOut, "error: %v\n", encErr)
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}

// trackedChanges snapshots what git already considers modified, so files the
// operator had touched before the run are not reported as metron's doing.
// Outside a repository it returns nil and changedSince reports nothing, which
// is honest: without git there is no way to tell what changed.
func trackedChanges(root string) map[string]bool {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	// Split without trimming the output first: the first two columns of a
	// porcelain line are the status, and for a plain unstaged edit the first of
	// them is a space. Trimming the whole blob eats it and shifts every path by
	// one character.
	for _, line := range strings.Split(string(out), "\n") {
		if path := porcelainPath(line); path != "" {
			seen[path] = true
		}
	}
	return seen
}

// changedSince reports the paths git sees as changed that were not already
// changed before the run. Deriving it from git rather than from the tools means
// it is true whichever edit format was used, and true for a file a permitted
// command wrote as a side effect.
func changedSince(root string, before map[string]bool) []string {
	if before == nil {
		return []string{}
	}
	after := trackedChanges(root)
	var changed []string
	for path := range after {
		if !before[path] {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	if changed == nil {
		return []string{}
	}
	return changed
}

// porcelainPath extracts the path from one `git status --porcelain` line. The
// two status characters are followed by a space, and a rename is reported as
// "old -> new", of which the new name is the one that now exists.
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	path := strings.TrimSpace(line[3:])
	if _, after, found := strings.Cut(path, " -> "); found {
		path = after
	}
	return strings.Trim(path, `"`)
}
