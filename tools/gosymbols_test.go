package tools

import (
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleGo exercises every declaration form the index knows about. Line
// numbers are asserted elsewhere, on a file small enough to count by eye.
const sampleGo = `package sample

import "fmt"

const (
	Alpha = 1
	Beta  = 2
)

var Gamma = fmt.Sprint("x")

type Widget struct {
	Name string
}

type Speaker interface {
	Speak() string
}

type Alias = Widget

func Greet(name string) string { return "hi " + name }

func (w Widget) Speak() string { return w.Name }

func (w *Widget) Rename(n string) { w.Name = n }

type Pair[K any, V any] struct{ k K }

func (p Pair[K, V]) First() K { return p.k }

func (p *Pair[K]) Second() K { return p.k }
`

// goProject writes a project the built-in index can read: sampleGo at the root,
// plus the noise it has to ignore.
func goProject(t *testing.T) string {
	t.Helper()
	dir := workdir(t)
	writeFile(t, "sample.go", sampleGo)
	writeFile(t, "README.md", "not Go\n")
	for _, sub := range []string{"vendor", "testdata", ".hidden"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(sub, "excluded.go"), "package x\n\nfunc Greet() {}\n")
	}
	return dir
}

func TestFindGoSymbolReportsKindsAndReceivers(t *testing.T) {
	goProject(t)
	env := defaultEnv(t)

	for _, tc := range []struct{ symbol, want string }{
		{"Alpha", "Alpha [const] -> sample.go:"},
		{"Beta", "Beta [const] -> sample.go:"},
		{"Gamma", "Gamma [var] -> sample.go:"},
		{"Widget", "Widget [struct] -> sample.go:"},
		{"Speaker", "Speaker [interface] -> sample.go:"},
		{"Alias", "Alias [type] -> sample.go:"},
		{"Greet", "Greet [func] -> sample.go:"},
		{"Speak", "Speak [method(Widget)] -> sample.go:"},
		{"Rename", "Rename [method(*Widget)] -> sample.go:"},
		{"First", "First [method(Pair)] -> sample.go:"},
		{"Second", "Second [method(*Pair)] -> sample.go:"},
	} {
		got := strings.Join(env.findGoSymbol(tc.symbol), "\n")
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("findGoSymbol(%q) = %q, want it to start with %q", tc.symbol, got, tc.want)
		}
		// Every declaration in sampleGo is at the top level of one file, so a
		// second match would mean the excluded directories were indexed.
		if strings.Count(got, "\n") != 0 {
			t.Errorf("findGoSymbol(%q) = %q, want a single match", tc.symbol, got)
		}
	}
}

func TestFindGoSymbolReportsSpans(t *testing.T) {
	workdir(t)
	writeFile(t, "span.go", "package span\n"+ // 1
		"\n"+ // 2
		"func Multi() {\n"+ // 3
		"\tprintln(1)\n"+ // 4
		"}\n"+ // 5
		"\n"+ // 6
		"var One = 1\n") // 7

	env := defaultEnv(t)
	if got := env.findGoSymbol("Multi"); len(got) != 1 || got[0] != "Multi [func] -> span.go:3-5" {
		t.Errorf("findGoSymbol(Multi) = %v, want the span reported", got)
	}
	// A one-line declaration collapses to a single number: "7-7" would be two
	// tokens spent saying nothing.
	if got := env.findGoSymbol("One"); len(got) != 1 || got[0] != "One [var] -> span.go:7" {
		t.Errorf("findGoSymbol(One) = %v, want a single line number", got)
	}
	if got := env.findGoSymbol("fmt"); len(got) != 0 {
		t.Errorf("findGoSymbol = %v, want imports left out of the index", got)
	}
}

func TestFindGoSymbolSkipsUnparseableFiles(t *testing.T) {
	workdir(t)
	writeFile(t, "broken.go", "package broken\n\nfunc Target( {\n")
	writeFile(t, "good.go", "package good\n\nfunc Target() {}\n")

	got := defaultEnv(t).findGoSymbol("Target")
	if len(got) != 1 || !strings.Contains(got[0], "good.go") {
		t.Fatalf("findGoSymbol = %v, want the parseable file still indexed", got)
	}
}

func TestFindGoSymbolSkipsFilesOutsideTheProject(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.go"), "package secret\n\nfunc Target() {}\n")
	dir := workdir(t)
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}

	if got := defaultEnv(t).findGoSymbol("Target"); len(got) != 0 {
		t.Fatalf("findGoSymbol = %v, want a symlink out of the project refused", got)
	}
}

func TestFindGoSymbolSurvivesUnreadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := workdir(t)
	writeFile(t, "good.go", "package good\n\nfunc Target() {}\n")
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(locked, "hidden.go"), "package locked\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if got := defaultEnv(t).findGoSymbol("Target"); len(got) != 1 {
		t.Fatalf("findGoSymbol = %v, want an unreadable directory to cost only itself", got)
	}
}

func TestHasGoSources(t *testing.T) {
	dir := workdir(t)
	env := defaultEnv(t)
	if env.hasGoSources() {
		t.Fatal("hasGoSources() = true for an empty project")
	}
	// A Go file inside an excluded directory does not make this a Go project
	// as far as the index is concerned: nothing in it would ever be indexed.
	if err := os.Mkdir(filepath.Join(dir, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join("vendor", "dep.go"), "package dep\n")
	if env.hasGoSources() {
		t.Fatal("hasGoSources() = true for vendored Go only")
	}
	writeFile(t, "main.go", "package main\n")
	if !env.hasGoSources() {
		t.Fatal("hasGoSources() = false with a Go file at the root")
	}
}

// TestDeclSymbolsIgnoresUnknownForms covers the shapes no valid source can
// produce: a declaration the parser gave up on, and a receiver that is not a
// name. Both are reachable only from a syntax tree built by hand, and both have
// to return rather than panic on a malformed index.
func TestDeclSymbolsIgnoresUnknownForms(t *testing.T) {
	if got := declSymbols(&ast.BadDecl{}); got != nil {
		t.Errorf("declSymbols(BadDecl) = %v, want nothing indexed", got)
	}
	if got := receiverName(&ast.BadExpr{}); got != "?" {
		t.Errorf("receiverName(BadExpr) = %q, want the unknown-receiver placeholder", got)
	}
}

func TestFindSymbolCapsTheNumberOfMatches(t *testing.T) {
	workdir(t)
	var b strings.Builder
	b.WriteString("package many\n")
	for i := range maxSymbolMatches + 5 {
		fmt.Fprintf(&b, "\ntype T%d struct{}\n\nfunc (t T%d) Target() {}\n", i, i)
	}
	writeFile(t, "many.go", b.String())

	got, err := defaultEnv(t).FindSymbol("Target")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != maxSymbolMatches+1 {
		t.Fatalf("FindSymbol() returned %d lines, want %d matches plus a note",
			len(lines), maxSymbolMatches)
	}
	if !strings.Contains(lines[len(lines)-1], "not shown") {
		t.Fatalf("last line = %q, want the truncation noted", lines[len(lines)-1])
	}
}
