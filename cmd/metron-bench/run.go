package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// metronOutput is the JSON contract metron prints on stdout under --json.
type metronOutput struct {
	Answer string `json:"answer"`
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Turns  int    `json:"turns"`
	Tools  []struct {
		Name string `json:"name"`
		MS   int    `json:"ms"`
	} `json:"tools"`
	Usage struct {
		Prompt    int `json:"prompt"`
		Generated int `json:"generated"`
	} `json:"usage"`
	FilesChanged []string `json:"files_changed"`
}

// parseMetronOutput reads the report from a run's stdout.
//
// It decodes the whole stream rather than a single line. `metron -p --json`
// guarantees one object on stdout and nothing else, but it prints that object
// indented for the human who runs the flag directly -- so a line-oriented parse
// sees a closing brace and calls a working run a parse failure, which in a
// results table is indistinguishable from a model that cannot code.
//
// Anything after the object is ignored rather than fatal: a stray trailing
// banner should not turn a good cell into a failed one.
func parseMetronOutput(stdout string) (metronOutput, error) {
	var out metronOutput
	if strings.TrimSpace(stdout) == "" {
		return out, fmt.Errorf("no JSON on stdout")
	}
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&out); err != nil {
		return out, fmt.Errorf("parse metron output: %w", err)
	}
	return out, nil
}

// Run is one repetition of one cell.
type Run struct {
	Pass            bool     `json:"pass"`
	Reason          string   `json:"reason,omitempty"`
	Turns           int      `json:"turns"`
	PromptTokens    int      `json:"prompt_tokens"`
	GeneratedTokens int      `json:"generated_tokens"`
	Tools           []string `json:"tools"`
	FilesChanged    []string `json:"files_changed"`
	WallMS          int64    `json:"wall_ms"`
}

// Cell is one (task, model, edit format) combination across its repetitions.
type Cell struct {
	Task       string `json:"task"`
	Model      string `json:"model"`
	EditFormat string `json:"edit_format"`

	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason,omitempty"`

	Runs []Run `json:"runs"`

	PassRate        float64 `json:"pass_rate"`
	MedianPrompt    int     `json:"median_prompt_tokens"`
	P95Prompt       int     `json:"p95_prompt_tokens"`
	MedianTurns     int     `json:"median_turns"`
	TokenCeiling    int     `json:"prompt_token_ceiling,omitempty"`
	CeilingBreached bool    `json:"prompt_token_ceiling_breached,omitempty"`
}

// Report is the whole run, written to bench/results/<date>.json.
type Report struct {
	GeneratedAt string  `json:"generated_at"`
	Matrix      Matrix  `json:"matrix"`
	MetronBin   string  `json:"metron_binary"`
	Cells       []Cell  `json:"cells"`
	OverallPass float64 `json:"overall_pass_rate"`
}

// Runner holds what every cell needs. Everything it touches is passed in, so
// tests can drive it against a fake metron binary and a scratch tree.
type Runner struct {
	MetronBin string
	Endpoint  string
	WorkDir   string
	Progress  io.Writer

	// Keep leaves scratch directories behind for inspection after a failure.
	Keep bool
}

// runOnce prepares a scratch repository, runs metron in it once, and lets
// verify.sh decide whether the task was accomplished.
func (r *Runner) runOnce(ctx context.Context, t Task, model, format string, rep int) Run {
	var run Run
	dir := filepath.Join(r.WorkDir, fmt.Sprintf("%s-%s-%d", t.Name, sanitize(model+"-"+format), rep))
	if !r.Keep {
		defer func() { _ = os.RemoveAll(dir) }()
	}

	cfg := cellConfig{
		Endpoint:           r.Endpoint,
		Model:              model,
		EditFormat:         format,
		AllowedCommands:    t.AllowedCommands,
		AutoApprovePatches: true,
		TimeoutSeconds:     t.TimeoutSeconds,
	}
	if err := prepareScratch(dir, t, cfg); err != nil {
		run.Reason = "scratch: " + err.Error()
		return run
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.MetronBin, "-p", t.Prompt, "--yes", "--json")
	cmd.Dir = dir
	// The model and endpoint are pinned in the environment as well as in the
	// config file: metron lets OLLAMA_MODEL override the file, so an operator
	// with that set in their shell would otherwise silently benchmark one
	// model under every model's name.
	cmd.Env = append(os.Environ(),
		"METRON_CONFIG="+filepath.Join(dir, ".metron.json"),
		"OLLAMA_HOST="+r.Endpoint,
		"OLLAMA_MODEL="+model,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	run.WallMS = time.Since(start).Milliseconds()

	out, parseErr := parseMetronOutput(stdout.String())
	if parseErr != nil {
		run.Reason = describeInvocation(runErr, parseErr, stderr.String())
		return run
	}
	run.Turns = out.Turns
	run.PromptTokens = out.Usage.Prompt
	run.GeneratedTokens = out.Usage.Generated
	run.FilesChanged = out.FilesChanged
	for _, tc := range out.Tools {
		run.Tools = append(run.Tools, tc.Name)
	}

	// A cross-check, not the verdict: the tasks whose right answer is to
	// change nothing must not have touched a file, whatever verify.sh makes
	// of the tree afterwards.
	if t.ExpectNoChanges && len(out.FilesChanged) > 0 {
		run.Reason = "expected no file changes, got " + strings.Join(out.FilesChanged, ", ")
		return run
	}

	ok, verifyOut := runVerify(ctx, t.verifyPath(), dir)
	run.Pass = ok
	if !ok {
		// A verifier built out of `grep -q` says nothing when it fails, and a
		// bare "verify:" in the report is worse than useless.
		reason := firstLine(verifyOut)
		if reason == "" {
			reason = "failed without output"
		}
		run.Reason = "verify: " + reason
	}
	return run
}

// describeInvocation explains an unparseable run: whichever of the process
// error, the parse error and stderr is actually informative.
func describeInvocation(runErr, parseErr error, stderr string) string {
	if runErr != nil {
		if s := firstLine(stderr); s != "" {
			return fmt.Sprintf("metron: %v: %s", runErr, s)
		}
		return "metron: " + runErr.Error()
	}
	return parseErr.Error()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// sanitize makes a model name safe as a path segment.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// runCell repeats one cell and summarises it.
func (r *Runner) runCell(ctx context.Context, t Task, model, format string, reps int) Cell {
	c := Cell{Task: t.Name, Model: model, EditFormat: format, TokenCeiling: t.MaxPromptTokens}
	var prompts, turns []int
	passes := 0
	for i := range reps {
		run := r.runOnce(ctx, t, model, format, i)
		c.Runs = append(c.Runs, run)
		if run.Pass {
			passes++
		}
		prompts = append(prompts, run.PromptTokens)
		turns = append(turns, run.Turns)
		r.report("  %-18s %-24s %-14s rep %d/%d %s (%d prompt tokens)\n",
			t.Name, model, format, i+1, reps, passFail(run.Pass), run.PromptTokens)
	}
	c.PassRate = rate(passes, reps)
	c.MedianPrompt = median(prompts)
	c.P95Prompt = percentile(prompts, 0.95)
	c.MedianTurns = median(turns)
	if t.MaxPromptTokens > 0 && c.MedianPrompt > t.MaxPromptTokens {
		c.CeilingBreached = true
	}
	return c
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func (r *Runner) report(format string, args ...any) {
	if r.Progress != nil {
		fmt.Fprintf(r.Progress, format, args...)
	}
}

// runAll walks the matrix. Cells whose model is not installed are recorded as
// skipped so the report says why a column is empty instead of leaving a hole.
func (r *Runner) runAll(ctx context.Context, tasks []Task, m Matrix, installed map[string]bool) Report {
	rep := Report{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Matrix:      m,
		MetronBin:   r.MetronBin,
	}
	executed, passed := 0, 0
	for _, model := range m.Models {
		for _, format := range m.EditFormats {
			for _, t := range tasks {
				if !installed[model] {
					rep.Cells = append(rep.Cells, Cell{
						Task: t.Name, Model: model, EditFormat: format,
						Skipped: true, SkipReason: "model not installed",
					})
					continue
				}
				c := r.runCell(ctx, t, model, format, m.Repetitions)
				rep.Cells = append(rep.Cells, c)
				executed += len(c.Runs)
				for _, run := range c.Runs {
					if run.Pass {
						passed++
					}
				}
			}
		}
	}
	rep.OverallPass = rate(passed, executed)
	return rep
}

// writeReport saves the report as bench/results/<date>.json.
func writeReport(dir string, rep Report, stamp string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Report holds only scalars, strings and slices of them; marshalling it
	// cannot fail.
	data, _ := json.MarshalIndent(rep, "", "  ")
	path := filepath.Join(dir, stamp+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
