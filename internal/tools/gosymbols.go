package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// The Go symbol index. It exists because the most common first run of metron is
// on a stock macOS, where `ctags` is BSD ctags and rejects the field flags
// EnsureTags needs -- so find_symbol, the tool that makes the whole
// never-read-a-whole-file contract workable, was unavailable on the very
// language metron is written in.
//
// go/ast, go/parser and go/token are standard library, so this costs the
// project nothing it cares about: no third-party module, no cgo, and the
// CGO_ENABLED=0 release builds are unaffected.
//
// Unlike .tags this index is never written to disk and never cached. It is
// rebuilt per call, which costs a parse of the tree but removes the whole class
// of "the index is from before that rename" failures that /tags exists to
// paper over.

// skippedDirs are directories the index never descends into. Hidden
// directories are skipped separately, which covers .git and .metron.
var skippedDirs = map[string]bool{
	"vendor":       true,
	"testdata":     true,
	"node_modules": true,
}

// skipDir reports whether a directory is excluded from the Go index. Hidden
// directories go first: they hold caches and metron's own state, and .git in
// particular is off limits to every tool.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || skippedDirs[name]
}

// walkGoFiles calls visit for every Go source file under Root, stopping early
// if visit returns false. Walk errors are swallowed on purpose: an unreadable
// directory should cost the symbols in it and nothing more.
func (e Env) walkGoFiles(visit func(path string) bool) {
	_ = filepath.WalkDir(e.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != e.Root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if !visit(path) {
			return fs.SkipAll
		}
		return nil
	})
}

// hasGoSources reports whether the project contains any Go source at all, which
// is what decides whether the built-in index can stand in for ctags. It stops
// at the first file rather than walking the whole tree.
func (e Env) hasGoSources() bool {
	found := false
	e.walkGoFiles(func(string) bool {
		found = true
		return false
	})
	return found
}

// findGoSymbol returns the same lines FindSymbol builds from a ctags index, for
// top-level Go declarations named exactly symbol. It stops once it has one more
// match than it will print, so a name like Err in a large tree does not cost a
// full parse of the project.
func (e Env) findGoSymbol(symbol string) []string {
	var matches []string
	fset := token.NewFileSet()
	e.walkGoFiles(func(path string) bool {
		rel, ok := e.projectPath(path)
		if !ok {
			return true
		}
		// A file that does not parse is skipped whole. Half a syntax tree
		// yields positions that no longer line up with the file on disk, and a
		// wrong line number is worse than a missing one: the model spends a
		// view_slice finding out.
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return true
		}
		for _, decl := range file.Decls {
			for _, d := range declSymbols(decl) {
				if d.name != symbol {
					continue
				}
				matches = append(matches, formatSymbol(d.name, d.kind, rel,
					strconv.Itoa(fset.Position(d.pos).Line),
					strconv.Itoa(fset.Position(d.end).Line)))
			}
		}
		return len(matches) <= maxSymbolMatches
	})
	return matches
}

// projectPath turns a walked path into the project-relative one find_symbol
// reports. It goes through resolve so the index can never name a file the other
// tools would refuse to open -- a .go symlink pointing out of the tree is the
// case that matters.
func (e Env) projectPath(path string) (string, bool) {
	real, err := e.resolve(path)
	if err != nil {
		return "", false
	}
	rel, _ := filepath.Rel(e.Root, real)
	return filepath.ToSlash(rel), true
}

// goDecl is one indexed declaration: what ctags would call a tag, with the span
// it covers.
type goDecl struct {
	name string
	kind string
	pos  token.Pos
	end  token.Pos
}

// declSymbols extracts the indexable names from one top-level declaration.
// Anything else -- an import, a declaration the parser could not make sense of
// -- contributes nothing.
func declSymbols(decl ast.Decl) []goDecl {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return []goDecl{{name: d.Name.Name, kind: funcKind(d), pos: d.Pos(), end: d.End()}}
	case *ast.GenDecl:
		return genDeclSymbols(d)
	}
	return nil
}

// genDeclSymbols expands a type/const/var block. The span recorded is the
// spec's, not the block's, so `const ( A = 1; B = 2 )` reports two one-line
// symbols rather than two copies of the whole block.
func genDeclSymbols(d *ast.GenDecl) []goDecl {
	var out []goDecl
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			out = append(out, goDecl{name: s.Name.Name, kind: typeKind(s), pos: s.Pos(), end: s.End()})
		case *ast.ValueSpec:
			kind := "var"
			if d.Tok == token.CONST {
				kind = "const"
			}
			for _, name := range s.Names {
				out = append(out, goDecl{name: name.Name, kind: kind, pos: s.Pos(), end: s.End()})
			}
		}
	}
	return out
}

// typeKind matches the kinds Universal Ctags reports for Go, so find_symbol
// reads the same whichever index answered.
func typeKind(s *ast.TypeSpec) string {
	switch s.Type.(type) {
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	}
	return "type"
}

// funcKind distinguishes a method from a function and names the receiver.
// Method names repeat across types far more than function names do, so
// `Step [method(*Agent)]` is what saves the model a view_slice spent working
// out which Step it found.
func funcKind(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return "func"
	}
	return "method(" + receiverName(d.Recv.List[0].Type) + ")"
}

// receiverName renders a receiver type as it is written in the source:
// pointers keep their star, and a generic receiver drops its type parameters,
// since Tree[K, V] and Tree are the same type to someone looking for it.
func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}
