// Command metron-bench measures metron against a matrix of models and edit
// formats over a suite of seeded repair tasks.
//
// Nobody in this space publishes numbers, so metron's central claim -- that a
// budgeted tool surface beats pasting files into the context -- has until now
// been an assertion. This runner turns it into a measurement: for every
// (task, model, edit format) cell it builds a fresh git repository from the
// task's seed, runs the real metron binary against it once per repetition,
// and lets the task's own verify.sh decide whether the work was done.
//
// Two design rules matter more than the rest:
//
//   - The model never judges itself. verify.sh inspects the repository, never
//     the answer text, so a confident sentence cannot pass a task.
//   - A missing model is a skip, not a failure -- the same posture the
//     -tags=live tests take, because which models are pulled is a fact about
//     the machine rather than about metron.
//
// It needs a live Ollama server, so it is deliberately not part of `make
// check` or `make test`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// exit is indirected so tests can exercise main without killing the process.
var exit = os.Exit

func main() { exit(runMain(os.Args[1:], os.Stdout, os.Stderr)) }

type benchFlags struct {
	tasksDir   string
	matrixPath string
	resultsDir string
	metronBin  string
	only       string
	reps       int
	minPass    float64
	keep       bool
}

func parseFlags(args []string, errOut io.Writer) (benchFlags, int, bool) {
	var f benchFlags
	fs := flag.NewFlagSet("metron-bench", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&f.tasksDir, "tasks", "bench/tasks", "directory of task directories")
	fs.StringVar(&f.matrixPath, "matrix", "bench/matrix.json", "models x edit formats x repetitions")
	fs.StringVar(&f.resultsDir, "results", "bench/results", "where the JSON report is written")
	fs.StringVar(&f.metronBin, "bin", "bin/metron", "the metron binary under test")
	fs.StringVar(&f.only, "only", "", "comma-separated task names to run (default: all)")
	fs.IntVar(&f.reps, "reps", 0, "override the matrix repetition count")
	fs.Float64Var(&f.minPass, "min-pass-rate", 0, "exit non-zero if any executed cell falls below this rate")
	fs.BoolVar(&f.keep, "keep", false, "keep scratch repositories for inspection")
	fs.Usage = func() {
		fmt.Fprintf(errOut, "usage: metron-bench [flags]\n\nRun from the root of the metron repository.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return f, 0, false
		}
		return f, 2, false
	}
	return f, 0, true
}

func runMain(args []string, out, errOut io.Writer) int {
	f, code, ok := parseFlags(args, errOut)
	if !ok {
		return code
	}

	matrix, err := loadMatrix(f.matrixPath)
	if err != nil {
		fmt.Fprintf(errOut, "metron-bench: %v\n", err)
		return 1
	}
	if f.reps > 0 {
		matrix.Repetitions = f.reps
	}

	// Task paths have to be absolute: every task's seed and verify.sh are
	// used with the scratch repository as the working directory, so a
	// relative path would resolve against the wrong tree.
	var tasks []Task
	tasksDir, err := filepath.Abs(f.tasksDir)
	if err == nil {
		tasks, err = loadTasks(tasksDir, splitList(f.only))
	}
	if err != nil {
		fmt.Fprintf(errOut, "metron-bench: %v\n", err)
		return 1
	}

	bin, err := filepath.Abs(f.metronBin)
	if err == nil {
		_, err = os.Stat(bin)
	}
	if err != nil {
		fmt.Fprintf(errOut, "metron-bench: %v (build it first: make build)\n", err)
		return 1
	}

	ctx := context.Background()
	installed, err := installedModels(ctx, matrix.Endpoint)
	if err != nil {
		fmt.Fprintf(errOut, "metron-bench: %v\n", err)
		return 1
	}

	work, err := os.MkdirTemp("", "metron-bench-")
	if err != nil {
		fmt.Fprintf(errOut, "metron-bench: %v\n", err)
		return 1
	}
	if f.keep {
		fmt.Fprintf(errOut, "scratch repositories kept in %s\n", work)
	} else {
		defer func() { _ = os.RemoveAll(work) }()
	}

	runner := &Runner{
		MetronBin: bin,
		Endpoint:  matrix.Endpoint,
		WorkDir:   work,
		Progress:  errOut,
		Keep:      f.keep,
	}
	rep := runner.runAll(ctx, tasks, matrix, installed)

	fmt.Fprint(out, renderTable(rep))

	path, err := writeReport(f.resultsDir, rep, time.Now().UTC().Format("2006-01-02T150405Z"))
	if err != nil {
		fmt.Fprintf(errOut, "metron-bench: write report: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "report: %s\n", path)

	if below := belowThreshold(rep, f.minPass); len(below) > 0 {
		for _, c := range below {
			fmt.Fprintf(errOut, "below threshold: %s/%s/%s at %s\n",
				c.Task, c.Model, c.EditFormat, formatRate(c.PassRate))
		}
		return 1
	}
	return 0
}

// belowThreshold lists executed cells that missed the gate, including any that
// breached a task's prompt-token ceiling. A cell can pass every repetition and
// still be a regression if it did so by reading more of the file than metron
// is supposed to let it.
func belowThreshold(rep Report, min float64) []Cell {
	var below []Cell
	for _, c := range rep.Cells {
		if c.Skipped {
			continue
		}
		if c.PassRate < min || c.CeilingBreached {
			below = append(below, c)
		}
	}
	return below
}
