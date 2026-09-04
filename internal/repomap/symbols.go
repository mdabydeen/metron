package repomap

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// symbols returns the top-level declarations of a Go file, exported names
// first, then unexported ones, each group in declaration order.
//
// Names only, with no signatures. A signature is the more useful answer to
// "what does this do?" and by some distance the more expensive one: parameter
// lists and result types routinely triple the width of a line, and the map's
// job is orientation, not documentation. The model that wants the signature
// knows the file now and can spend one view_slice on it.
//
// Method receivers are not qualified either -- `Step`, not `Agent.Step`. It
// reads as well in a list of one file's symbols and costs less.
func symbols(path string) []string {
	// The source is read by the parser rather than by us, so an unreadable or
	// non-regular file is the same branch as a file with no AST. A syntax error
	// is not: the parser returns the declarations it did manage to read, which
	// is why one broken file cannot sink the map.
	f, _ := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if f == nil {
		return nil
	}

	var exported, unexported []string
	seen := make(map[string]bool)
	add := func(name string) {
		// The blank identifier names nothing; a name declared twice (a method
		// on two types, say) is worth one line, not two.
		if name == "_" || seen[name] {
			return
		}
		seen[name] = true
		if ast.IsExported(name) {
			exported = append(exported, name)
			return
		}
		unexported = append(unexported, name)
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			add(d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add(s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						add(n.Name)
					}
				}
			}
		}
	}
	return append(exported, unexported...)
}
