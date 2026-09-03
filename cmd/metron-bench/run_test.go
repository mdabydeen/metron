package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMetronOutput(t *testing.T) {
	// A banner on stdout must not sink the run: the last line is the report.
	out, err := parseMetronOutput("loading model...\n" + okOutput + "\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if out.Turns != 2 || out.Usage.Prompt != 1200 || out.Tools[0].Name != "view_slice" {
		t.Fatalf("parsed %+v", out)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != "a.go" {
		t.Fatalf("files changed = %v", out.FilesChanged)
	}
}

func TestParseMetronOutputErrors(t *testing.T) {
	if _, err := parseMetronOutput("  \n\n"); err == nil ||
		!strings.Contains(err.Error(), "no JSON on stdout") {
		t.Fatalf("error = %v", err)
	}
	if _, err := parseMetronOutput("not json\n"); err == nil ||
		!strings.Contains(err.Error(), "parse metron output") {
		t.Fatalf("error = %v", err)
	}
}

func TestFirstLineAndSanitize(t *testing.T) {
	if got := firstLine(" one\ntwo\n"); got != "one" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine(" solo "); got != "solo" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := sanitize("qwen2.5-coder:32b/x"); got != "qwen2_5-coder_32b_x" {
		t.Fatalf("sanitize = %q", got)
	}
}

func TestDescribeInvocation(t *testing.T) {
	runErr := os.ErrClosed
	parseErr := os.ErrInvalid
	if got := describeInvocation(runErr, parseErr, "boom\nmore\n"); !strings.Contains(got, "boom") {
		t.Fatalf("got %q", got)
	}
	if got := describeInvocation(runErr, parseErr, "  "); !strings.Contains(got, "metron: file already closed") {
		t.Fatalf("got %q", got)
	}
	if got := describeInvocation(nil, parseErr, ""); got != parseErr.Error() {
		t.Fatalf("got %q", got)
	}
}

func TestPassFail(t *testing.T) {
	if passFail(true) != "PASS" || passFail(false) != "FAIL" {
		t.Fatal("passFail is wrong")
	}
}

// editTask is a task whose verify.sh only passes if a file was actually
// rewritten. It is the fixture the end-to-end runner tests use.
func editTask(t *testing.T, root string, meta map[string]any) Task {
	t.Helper()
	dir := writeTask(t, root, "edit", meta, "change it\n",
		"#!/bin/sh\ngrep -q hola greet.go || { echo 'still says hello'; exit 1; }\n",
		map[string]string{"greet.go": "package a\n\nconst g = \"hello\"\n"})
	task, err := loadTask(dir)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRunOncePassesWhenTheWorkWasDone(t *testing.T) {
	requireGit(t)
	task := editTask(t, t.TempDir(), nil)
	// The fake metron does the edit, then prints the JSON contract -- exactly
	// what the real binary does under -p ... --yes --json.
	bin := fakeMetron(t, `
sed 's/hello/hola/' greet.go > greet.tmp && mv greet.tmp greet.go
echo '`+okOutput+`'
`)
	r := &Runner{MetronBin: bin, Endpoint: "http://x/api/chat", WorkDir: t.TempDir()}
	run := r.runOnce(t.Context(), task, "m", "diff", 0)
	if !run.Pass {
		t.Fatalf("run failed: %+v", run)
	}
	if run.PromptTokens != 1200 || run.GeneratedTokens != 80 || run.Turns != 2 {
		t.Fatalf("usage not carried through: %+v", run)
	}
	if len(run.Tools) != 1 || run.Tools[0] != "view_slice" {
		t.Fatalf("tools = %v", run.Tools)
	}
}

func TestRunOnceFailsWhenVerifyRejectsTheTree(t *testing.T) {
	requireGit(t)
	task := editTask(t, t.TempDir(), nil)
	// This one claims success in prose and changes nothing. The benchmark
	// must not believe it.
	bin := fakeMetron(t, "echo '"+okOutput+"'\n")
	r := &Runner{MetronBin: bin, Endpoint: "http://x/api/chat", WorkDir: t.TempDir()}
	run := r.runOnce(t.Context(), task, "m", "diff", 0)
	if run.Pass {
		t.Fatal("a run that changed nothing was scored as a pass")
	}
	if !strings.Contains(run.Reason, "still says hello") {
		t.Fatalf("reason = %q", run.Reason)
	}
}

func TestRunOnceExplainsASilentVerifier(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	// `grep -q` prints nothing when it fails, which is the common case.
	dir := writeTask(t, root, "quiet", nil, "change it\n",
		"#!/bin/sh\ngrep -q hola greet.go\n",
		map[string]string{"greet.go": "package a\n\nconst g = \"hello\"\n"})
	task, err := loadTask(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := fakeMetron(t, "echo '"+okOutput+"'\n")
	r := &Runner{MetronBin: bin, WorkDir: t.TempDir()}
	run := r.runOnce(t.Context(), task, "m", "diff", 0)
	if run.Pass || run.Reason != "verify: failed without output" {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunOnceHonoursExpectNoChanges(t *testing.T) {
	requireGit(t)
	task := editTask(t, t.TempDir(), map[string]any{"expect_no_changes": true})
	bin := fakeMetron(t, "echo '"+okOutput+"'\n")
	r := &Runner{MetronBin: bin, Endpoint: "http://x/api/chat", WorkDir: t.TempDir()}
	run := r.runOnce(t.Context(), task, "m", "diff", 0)
	if run.Pass || !strings.Contains(run.Reason, "expected no file changes") {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunOnceReportsABrokenBinary(t *testing.T) {
	requireGit(t)
	task := editTask(t, t.TempDir(), nil)
	bin := fakeMetron(t, "echo 'model exploded' >&2\nexit 1\n")
	r := &Runner{MetronBin: bin, Endpoint: "http://x/api/chat", WorkDir: t.TempDir()}
	run := r.runOnce(t.Context(), task, "m", "diff", 0)
	if run.Pass || !strings.Contains(run.Reason, "model exploded") {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunOnceReportsABrokenScratch(t *testing.T) {
	task := editTask(t, t.TempDir(), nil)
	task.Dir = filepath.Join(t.TempDir(), "gone")
	r := &Runner{MetronBin: "/nonexistent", WorkDir: t.TempDir()}
	run := r.runOnce(t.Context(), task, "m", "diff", 0)
	if run.Pass || !strings.Contains(run.Reason, "scratch:") {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunOnceKeepsScratchWhenAsked(t *testing.T) {
	requireGit(t)
	task := editTask(t, t.TempDir(), nil)
	bin := fakeMetron(t, "echo '"+okOutput+"'\n")
	work := t.TempDir()
	r := &Runner{MetronBin: bin, WorkDir: work, Keep: true}
	r.runOnce(t.Context(), task, "m", "diff", 0)
	if _, err := os.Stat(filepath.Join(work, "edit-m-diff-0")); err != nil {
		t.Fatalf("scratch was removed despite Keep: %v", err)
	}
}

func TestRunCellSummarisesRepetitions(t *testing.T) {
	requireGit(t)
	// A ceiling of 1 prompt token that the fake blows every time, so the
	// large-file-edit guard is exercised without a large file.
	task := editTask(t, t.TempDir(), map[string]any{"max_prompt_tokens": 1})
	bin := fakeMetron(t, `
sed 's/hello/hola/' greet.go > greet.tmp && mv greet.tmp greet.go
echo '`+okOutput+`'
`)
	var progress strings.Builder
	r := &Runner{MetronBin: bin, WorkDir: t.TempDir(), Progress: &progress}
	cell := r.runCell(t.Context(), task, "m", "diff", 3)

	if cell.PassRate != 1 || len(cell.Runs) != 3 {
		t.Fatalf("cell = %+v", cell)
	}
	if cell.MedianPrompt != 1200 || cell.P95Prompt != 1200 || cell.MedianTurns != 2 {
		t.Fatalf("stats = %+v", cell)
	}
	if !cell.CeilingBreached {
		t.Fatal("a blown token ceiling was not reported")
	}
	if !strings.Contains(progress.String(), "rep 3/3 PASS") {
		t.Fatalf("progress = %q", progress.String())
	}
}

func TestRunAllSkipsUninstalledModels(t *testing.T) {
	requireGit(t)
	task := editTask(t, t.TempDir(), nil)
	bin := fakeMetron(t, `
sed 's/hello/hola/' greet.go > greet.tmp && mv greet.tmp greet.go
echo '`+okOutput+`'
`)
	r := &Runner{MetronBin: bin, WorkDir: t.TempDir()}
	m := Matrix{
		Endpoint:    "http://x/api/chat",
		Models:      []string{"here", "absent"},
		EditFormats: []string{"diff"},
		Repetitions: 1,
	}
	rep := r.runAll(t.Context(), []Task{task}, m, map[string]bool{"here": true})

	if len(rep.Cells) != 2 {
		t.Fatalf("cells = %+v", rep.Cells)
	}
	if rep.Cells[0].Skipped || rep.Cells[0].PassRate != 1 {
		t.Fatalf("installed cell = %+v", rep.Cells[0])
	}
	if !rep.Cells[1].Skipped || rep.Cells[1].SkipReason != "model not installed" {
		t.Fatalf("skipped cell = %+v", rep.Cells[1])
	}
	// A skipped cell must not drag the headline number down.
	if rep.OverallPass != 1 {
		t.Fatalf("overall pass rate = %v", rep.OverallPass)
	}
}

func TestWriteReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "results")
	path, err := writeReport(dir, Report{GeneratedAt: "now"}, "2026-01-02T030405Z")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "2026-01-02T030405Z.json" {
		t.Fatalf("path = %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"generated_at": "now"`) {
		t.Fatalf("report = %q, err %v", data, err)
	}
}

func TestWriteReportFailures(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	writeFile(t, blocker, "", 0o644)
	if _, err := writeReport(filepath.Join(blocker, "results"), Report{}, "x"); err == nil {
		t.Fatal("want an error for an uncreatable directory")
	}

	requireNonRoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := writeReport(dir, Report{}, "x"); err == nil {
		t.Fatal("want an error for an unwritable directory")
	}
}
