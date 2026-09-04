// Package repomap renders a small, ranked structural summary of a repository:
// the files most likely to matter, each with its top-level symbols, trimmed to
// fit a token budget.
//
// metron never lets the model read a whole file, so without a map the model
// starts every session blind -- it does not know what the project contains, and
// spends turns guessing filenames and listing directories before it does any
// work. The map is the cheapest possible orientation.
//
// The budget is a hard ceiling, not a target. A repo map is injected once and
// then paid for on every request of the session, so an overrun is not a one-off
// cost: it is a tax on every turn until the session ends.
package repomap

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// bytesPerToken converts the caller's token budget into bytes. The usual
	// rule of thumb is ~4 bytes per token for English prose, but a repo map is
	// not prose: it is paths and identifiers, and separators, camelCase humps
	// and underscores all split into tokens of their own. metron ships no
	// tokeniser, so the estimate cannot be checked at runtime and the only safe
	// direction to be wrong in is under. Three bytes per token buys roughly a
	// 25% margin against the rule of thumb.
	bytesPerToken = 3

	// maxLineBytes caps a single entry, so one generated file with six hundred
	// symbols cannot eat a whole map that was meant to show fifty files.
	maxLineBytes = 160

	// minParsed and maxParsed bound how many files are opened and parsed. The
	// map is built at session start and the operator is waiting on it, so the
	// work has to be bounded by something other than the size of the tree.
	minParsed = 32
	maxParsed = 512

	headerChurn = "# repo map: top files by recent churn, with top-level symbols"
	// Outside a git repository there is no churn to rank by, and the ordering
	// is weaker as a result. Saying which ranking produced the map keeps the
	// model from reading more into the order than is there.
	headerFlat = "# repo map: files by symbol density (no git history), with top-level symbols"
)

// entry is one candidate line of the map.
type entry struct {
	path  string   // slash-separated, relative to the root
	churn int      // commits touching this path in the recent window
	syms  []string // top-level declarations, exported first; empty for non-Go
}

// Build returns a ranked structural summary of the project at root, rendered to
// fit within roughly budgetTokens tokens. A zero or negative budget returns "".
//
// It never returns an error. Every failure mode -- no git, no readable files, a
// file that will not parse, a root that does not exist -- degrades the map
// rather than failing the session that asked for it. A partial map is worth
// having; a startup error because a repository has no commits yet is not.
func Build(root string, budgetTokens int) string {
	if budgetTokens <= 0 {
		return ""
	}
	// Resolve once, here, so that every later path comparison is between two
	// real paths. On macOS this is routine rather than exotic: /tmp is a
	// symlink to /private/tmp. A root that does not exist fails here.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}

	paths := discover(real, maxFilesExamined)
	if len(paths) == 0 {
		return ""
	}
	churn, haveChurn := churnCounts(real)

	entries := make([]entry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, entry{path: p, churn: churn[p]})
	}

	// Two sorts, with the parsing between them. Symbol density is a ranking
	// input but it is also the expensive one, so the cheap ordering runs first
	// and only its head is parsed: the alternative is parsing a 3,000-file tree
	// to decide which fifty files to show.
	slices.SortFunc(entries, byCheapRank)
	budget := budgetTokens * bytesPerToken
	entries = entries[:min(parseCap(budget), len(entries))]
	for i := range entries {
		if parseable(entries[i].path) {
			entries[i].syms = symbols(filepath.Join(real, filepath.FromSlash(entries[i].path)))
		}
	}
	slices.SortFunc(entries, byRank)

	return render(entries, budget, haveChurn)
}

// parseCap turns a byte budget into a number of files to parse. A rendered
// entry averages well under 60 bytes, so budget/20 leaves roughly a threefold
// margin for entries that rank into the candidate set and then do not fit.
func parseCap(budget int) int {
	return max(minParsed, min(budget/20, maxParsed))
}

// byCheapRank orders on what is known before any file is opened: churn first,
// then shallow paths, which are a decent proxy for centrality in the absence of
// anything better. Path breaks the last tie so the map is deterministic.
func byCheapRank(a, b entry) int {
	return cmp.Or(
		cmp.Compare(b.churn, a.churn),
		cmp.Compare(depth(a.path), depth(b.path)),
		cmp.Compare(a.path, b.path),
	)
}

// byRank is the final order: churn, tie-broken by symbol density.
//
// Churn first is a deliberately simple choice. aider ranks with PageRank over
// the symbol reference graph, which is better and much more expensive -- it has
// to parse the whole tree to build the graph, which is exactly the cost this
// package is trying to avoid. Churn needs one `git log` and answers a closely
// related question: the files this project actually works on are the files the
// next request is most likely to be about.
func byRank(a, b entry) int {
	return cmp.Or(
		cmp.Compare(b.churn, a.churn),
		cmp.Compare(len(b.syms), len(a.syms)),
		cmp.Compare(depth(a.path), depth(b.path)),
		cmp.Compare(a.path, b.path),
	)
}

func depth(path string) int { return strings.Count(path, "/") }

// parseable reports whether a file's symbols are worth the tokens they cost.
//
// Test files are listed but not expanded, which is a judgement about what a
// repo map is for. A line reading `loop_test.go  TestStepReturnsAnswer,
// TestStepAdvertisesEveryTool (+63 more)` is one of the widest in the map and
// tells the model nothing it could not have guessed from the filename, whereas
// the same tokens spent on another package's declarations are new information.
// The path is still shown, so the model knows the tests exist and can go and
// read them; it just does not pay to have them enumerated.
//
// Since an unexpanded file also carries no symbol count, this doubles as a
// ranking demotion: at equal churn a test file now sorts below the source it
// tests.
func parseable(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

// render lays out as many entries as fit. The accounting is deliberately one
// byte per line pessimistic -- it charges every line for a trailing newline,
// including the last, which has none -- so the result is always inside the
// budget rather than exactly at it.
func render(entries []entry, budget int, haveChurn bool) string {
	head := headerFlat
	if haveChurn {
		head = headerChurn
	}
	avail := budget - len(head) - 1

	var lines []string
	for _, e := range entries {
		if avail <= 0 {
			break
		}
		line := renderEntry(e, min(avail, maxLineBytes))
		if line == "" {
			// One path too long for the remaining room is not a reason to stop:
			// a shorter one further down the ranking may still fit.
			continue
		}
		lines = append(lines, line)
		avail -= len(line) + 1
	}
	if len(lines) == 0 {
		// A header on its own describes nothing and still costs tokens.
		return ""
	}
	return head + "\n" + strings.Join(lines, "\n")
}

// renderEntry formats one file as `path  Sym, Sym, Sym (+N more)`, or "" if not
// even the bare path fits in width. Symbols are dropped from the tail, since they
// arrive exported-first and the exported ones are what a caller of this file
// would need to know.
func renderEntry(e entry, width int) string {
	if len(e.path) > width {
		return ""
	}
	// Reserving the widest possible elision up front costs a symbol or two in
	// the worst case and removes the alternative: deciding to elide after the
	// line is already over budget.
	elision := len(elide(len(e.syms)))

	line, shown := e.path, 0
	for i, s := range e.syms {
		sep := ", "
		if i == 0 {
			sep = "  "
		}
		need := elision
		if i == len(e.syms)-1 {
			need = 0
		}
		if len(line)+len(sep)+len(s)+need > width {
			break
		}
		line += sep + s
		shown++
	}
	// The elision has to fit too. When no symbol did, the reservation above was
	// never applied and the marker can be wider than the room left -- in which
	// case the bare path is the honest answer, and the same one a non-Go file
	// gets.
	if rest := len(e.syms) - shown; rest > 0 {
		if suffix := elide(rest); len(line)+len(suffix) <= width {
			line += suffix
		}
	}
	return line
}

func elide(n int) string { return fmt.Sprintf(" (+%d more)", n) }
