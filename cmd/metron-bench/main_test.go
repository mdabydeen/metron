package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchFixture is a whole runnable benchmark: one task, a fake Ollama that
// reports one installed model, and a fake metron binary that does the work.
type benchFixture struct {
	tasksDir   string
	matrixPath string
	resultsDir string
	bin        string
}

func newFixture(t *testing.T, metronBody string, installed []string) benchFixture {
	t.Helper()
	tasksDir := t.TempDir()
	writeTask(t, tasksDir, "edit", nil, "change it\n",
		"#!/bin/sh\ngrep -q hola greet.go || { echo 'still says hello'; exit 1; }\n",
		map[string]string{"greet.go": "package a\n\nconst g = \"hello\"\n"})

	models := make([]string, 0, len(installed))
	for _, m := range installed {
		models = append(models, fmt.Sprintf(`{"name":%q,"model":%q}`, m, m))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(models, ","))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	matrixPath := filepath.Join(dir, "matrix.json")
	writeFile(t, matrixPath, fmt.Sprintf(
		`{"endpoint":%q,"models":["m"],"edit_formats":["diff"],"repetitions":2}`,
		srv.URL+"/api/chat"), 0o644)

	return benchFixture{
		tasksDir:   tasksDir,
		matrixPath: matrixPath,
		resultsDir: filepath.Join(dir, "results"),
		bin:        fakeMetron(t, metronBody),
	}
}

func (f benchFixture) args(extra ...string) []string {
	return append([]string{
		"-tasks", f.tasksDir,
		"-matrix", f.matrixPath,
		"-results", f.resultsDir,
		"-bin", f.bin,
	}, extra...)
}

const workingMetron = `
sed 's/hello/hola/' greet.go > greet.tmp && mv greet.tmp greet.go
echo '` + okOutput + `'
`

func TestParseFlags(t *testing.T) {
	var errOut strings.Builder
	f, code, ok := parseFlags([]string{"-reps", "5", "-only", "a,b"}, &errOut)
	if !ok || code != 0 || f.reps != 5 || f.only != "a,b" {
		t.Fatalf("flags = %+v, code %d, ok %v", f, code, ok)
	}

	_, code, ok = parseFlags([]string{"-h"}, &errOut)
	if ok || code != 0 {
		t.Fatalf("-h gave code %d, ok %v", code, ok)
	}
	if !strings.Contains(errOut.String(), "usage: metron-bench") {
		t.Fatalf("usage not printed: %q", errOut.String())
	}

	_, code, ok = parseFlags([]string{"-nope"}, &errOut)
	if ok || code != 2 {
		t.Fatalf("bad flag gave code %d, ok %v", code, ok)
	}
}

func TestRunMainHappyPath(t *testing.T) {
	requireGit(t)
	fx := newFixture(t, workingMetron, []string{"m"})
	var out, errOut strings.Builder
	if code := runMain(fx.args("-reps", "1", "-min-pass-rate", "1"), &out, &errOut); code != 0 {
		t.Fatalf("code = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "overall pass rate: 100%") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	entries, err := os.ReadDir(fx.resultsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("results = %v, err %v", entries, err)
	}
	if !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("report name = %s", entries[0].Name())
	}
	if !strings.Contains(out.String(), "report: ") {
		t.Fatal("the report path was not announced")
	}
}

// A relative -tasks path must still work. It did not once: verify.sh and the
// seed are used with the scratch repository as the working directory, so a
// relative task path resolved against the scratch tree and every task failed
// with "no such file or directory" -- which reads exactly like a model that
// cannot code.
func TestRunMainAcceptsARelativeTasksPath(t *testing.T) {
	requireGit(t)
	fx := newFixture(t, workingMetron, []string{"m"})
	parent := filepath.Dir(fx.tasksDir)
	t.Chdir(parent)

	var out, errOut strings.Builder
	code := runMain([]string{
		"-tasks", filepath.Base(fx.tasksDir),
		"-matrix", fx.matrixPath, "-results", fx.resultsDir,
		"-bin", fx.bin, "-reps", "1", "-min-pass-rate", "1",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
}

func TestRunMainKeepsScratchAndSkipsMissingModels(t *testing.T) {
	requireGit(t)
	fx := newFixture(t, workingMetron, nil) // nothing installed
	var out, errOut strings.Builder
	if code := runMain(fx.args("-keep"), &out, &errOut); code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "model not installed") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "scratch repositories kept in") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
	// A suite where every cell was skipped has nothing to gate on.
	if strings.Contains(errOut.String(), "below threshold") {
		t.Fatal("a skipped cell was gated")
	}
}

func TestRunMainGatesOnPassRate(t *testing.T) {
	requireGit(t)
	fx := newFixture(t, "echo '"+okOutput+"'\n", []string{"m"}) // claims success, changes nothing
	var out, errOut strings.Builder
	if code := runMain(fx.args("-reps", "1", "-min-pass-rate", "1"), &out, &errOut); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "below threshold: edit/m/diff at 0%") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

func TestRunMainReportsSetupFailures(t *testing.T) {
	fx := newFixture(t, workingMetron, []string{"m"})

	t.Run("bad flag", func(t *testing.T) {
		var out, errOut strings.Builder
		if code := runMain([]string{"-nope"}, &out, &errOut); code != 2 {
			t.Fatalf("code = %d", code)
		}
	})

	t.Run("no matrix", func(t *testing.T) {
		var out, errOut strings.Builder
		code := runMain([]string{"-matrix", filepath.Join(t.TempDir(), "nope.json")}, &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "read matrix") {
			t.Fatalf("code %d, stderr %s", code, errOut.String())
		}
	})

	t.Run("no tasks", func(t *testing.T) {
		var out, errOut strings.Builder
		code := runMain([]string{"-matrix", fx.matrixPath, "-tasks", filepath.Join(t.TempDir(), "nope")}, &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "read tasks dir") {
			t.Fatalf("code %d, stderr %s", code, errOut.String())
		}
	})

	t.Run("no binary", func(t *testing.T) {
		var out, errOut strings.Builder
		code := runMain([]string{
			"-matrix", fx.matrixPath, "-tasks", fx.tasksDir,
			"-bin", filepath.Join(t.TempDir(), "metron"),
		}, &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "make build") {
			t.Fatalf("code %d, stderr %s", code, errOut.String())
		}
	})

	t.Run("ollama unreachable", func(t *testing.T) {
		dir := t.TempDir()
		matrix := filepath.Join(dir, "matrix.json")
		writeFile(t, matrix, `{"endpoint":"http://127.0.0.1:1/api/chat","models":["m"],"edit_formats":["diff"],"repetitions":1}`, 0o644)
		var out, errOut strings.Builder
		code := runMain([]string{"-matrix", matrix, "-tasks", fx.tasksDir, "-bin", fx.bin}, &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "model list") {
			t.Fatalf("code %d, stderr %s", code, errOut.String())
		}
	})

	t.Run("no scratch space", func(t *testing.T) {
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
		var out, errOut strings.Builder
		if code := runMain(fx.args(), &out, &errOut); code != 1 {
			t.Fatalf("code = %d, stderr %s", code, errOut.String())
		}
	})

	t.Run("report cannot be written", func(t *testing.T) {
		requireGit(t)
		blocker := filepath.Join(t.TempDir(), "file")
		writeFile(t, blocker, "", 0o644)
		var out, errOut strings.Builder
		code := runMain([]string{
			"-matrix", fx.matrixPath, "-tasks", fx.tasksDir, "-bin", fx.bin,
			"-results", filepath.Join(blocker, "results"), "-reps", "1",
		}, &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "write report") {
			t.Fatalf("code %d, stderr %s", code, errOut.String())
		}
	})
}

func TestBelowThresholdCountsACeilingBreach(t *testing.T) {
	rep := Report{Cells: []Cell{
		{Task: "a", PassRate: 1},
		{Task: "b", PassRate: 1, CeilingBreached: true},
		{Task: "c", Skipped: true},
	}}
	below := belowThreshold(rep, 1)
	if len(below) != 1 || below[0].Task != "b" {
		t.Fatalf("below = %+v", below)
	}
}

func TestMainExitsWithRunMainCode(t *testing.T) {
	oldExit, oldArgs := exit, os.Args
	t.Cleanup(func() { exit, os.Args = oldExit, oldArgs })

	got := -1
	exit = func(code int) { got = code }
	os.Args = []string{"metron-bench", "-nope"}
	main()
	if got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
}
