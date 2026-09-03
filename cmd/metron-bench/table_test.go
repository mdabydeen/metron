package main

import (
	"strings"
	"testing"
)

func TestRenderTable(t *testing.T) {
	rep := Report{
		OverallPass: 0.5,
		Cells: []Cell{
			{
				Task: "rename-symbol", Model: "m", EditFormat: "diff",
				PassRate: 1, MedianPrompt: 900, P95Prompt: 1100, MedianTurns: 3,
				Runs: []Run{{Pass: true}},
			},
			{
				Task: "large-file-edit", Model: "m", EditFormat: "search_replace",
				PassRate: 0, MedianPrompt: 40000, P95Prompt: 41000, MedianTurns: 9,
				TokenCeiling: 20000, CeilingBreached: true,
				Runs: []Run{{Pass: false, Reason: "verify: churn too large"}},
			},
			{Task: "no-such-symbol", Model: "absent", EditFormat: "diff", Skipped: true, SkipReason: "model not installed"},
		},
	}
	out := renderTable(rep)
	for _, want := range []string{
		"TASK", "rename-symbol", "100%",
		"prompt p50 40000 > ceiling 20000",
		"verify: churn too large",
		"skip", "model not installed",
		"overall pass rate: 50%",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("table\n%s\ndoes not contain %q", out, want)
		}
	}
}

func TestCellNoteIsEmptyForACleanCell(t *testing.T) {
	if got := cellNote(Cell{PassRate: 1, Runs: []Run{{Pass: true}}}); got != "" {
		t.Fatalf("note = %q", got)
	}
	// A failure with no reason recorded still leaves the note empty rather
	// than printing an orphan separator.
	if got := cellNote(Cell{Runs: []Run{{Pass: false}}}); got != "" {
		t.Fatalf("note = %q", got)
	}
}

func TestFormatRate(t *testing.T) {
	if got := formatRate(0.6667); got != "67%" {
		t.Fatalf("formatRate = %q", got)
	}
}
