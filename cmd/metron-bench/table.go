package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// renderTable formats a report for a terminal. One row per cell, because the
// point of the exercise is comparing cells: the same task under two edit
// formats is the argument the table has to be able to settle.
func renderTable(rep Report) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tMODEL\tFORMAT\tPASS\tPROMPT p50\tPROMPT p95\tTURNS p50\tNOTE")
	for _, c := range rep.Cells {
		if c.Skipped {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				c.Task, c.Model, c.EditFormat, "skip", "-", "-", "-", c.SkipReason)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			c.Task, c.Model, c.EditFormat,
			formatRate(c.PassRate), c.MedianPrompt, c.P95Prompt, c.MedianTurns,
			cellNote(c))
	}
	_ = w.Flush()
	fmt.Fprintf(&b, "\noverall pass rate: %s\n", formatRate(rep.OverallPass))
	return b.String()
}

func formatRate(r float64) string { return fmt.Sprintf("%.0f%%", r*100) }

// cellNote surfaces the two things a bare pass rate hides: a blown token
// ceiling, and the first reason a repetition failed.
func cellNote(c Cell) string {
	var notes []string
	if c.CeilingBreached {
		notes = append(notes, fmt.Sprintf("prompt p50 %d > ceiling %d", c.MedianPrompt, c.TokenCeiling))
	}
	for _, run := range c.Runs {
		if !run.Pass && run.Reason != "" {
			notes = append(notes, run.Reason)
			break
		}
	}
	return strings.Join(notes, "; ")
}
