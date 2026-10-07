package schemata

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"slices"
	"strings"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// orphanReason reports why a mutant cannot be schematized because doing so
// would change its verdict, or "" when it is safe.
//
// Deletion-shaped mutations can orphan a declaration. Today
//
//	x := compute()
//	fmt.Println(x)   // STATEMENT_REMOVE -> "_ = 0"
//
// fails to compile with "x declared and not used" and is reported NOT
// VIABLE. Under schemata the original statement survives in the else
// branch, so the same mutation compiles and the mutant actually runs — a
// different verdict, and one the build-error loop cannot catch because the
// build succeeds. The existing mutators already reason this way:
// statement_remove.go keeps the right-hand side specifically "to avoid
// unused-import errors".
//
// The test is syntactic, in keeping with the rest of the package: a name
// the mutation drops is an orphan risk when it names a function-local
// declaration or an import and occurs nowhere else in scope. A *shadowed*
// local whose only real use is inside the removed span but whose name
// appears elsewhere reads as safe here; that residual is documented rather
// than chased, since closing it needs full scope resolution.
func orphanReason(fset *token.FileSet, file *ast.File, src []byte, m mutator.Mutant) string {
	dropped := droppedNames(src, m)
	if len(dropped) == 0 {
		return ""
	}

	imports := importNames(file)
	fn := enclosingBody(fset, file, m.StartOffset, m.EndOffset)

	var locals map[string]bool
	if fn != nil {
		locals = declaredLocals(fn)
	}

	for _, name := range dropped {
		switch {
		case locals[name]:
			if countUses(fn, fset, name, m.StartOffset, m.EndOffset) == 0 {
				return fmt.Sprintf("would orphan the declaration of %q", name)
			}
		case imports[name]:
			// An import must be used somewhere in its file.
			if countUses(file, fset, name, m.StartOffset, m.EndOffset) == 0 {
				return fmt.Sprintf("would orphan the import %q", name)
			}
		}
	}
	return ""
}

// droppedNames lists identifiers present in the span a mutation replaces
// but absent from its replacement text. Both sides are scanned as Go token
// streams rather than parsed, because neither a partial span nor a
// replacement fragment is necessarily a parseable expression on its own.
func droppedNames(src []byte, m mutator.Mutant) []string {
	if m.StartOffset >= m.EndOffset || m.EndOffset > len(src) {
		return nil // A pure insertion drops nothing.
	}
	kept := scanIdents(m.Replacement)
	var dropped []string
	seen := map[string]bool{}
	for name := range scanIdents(string(src[m.StartOffset:m.EndOffset])) {
		if kept[name] || seen[name] {
			continue
		}
		seen[name] = true
		dropped = append(dropped, name)
	}
	// Map iteration is random; sort so a demotion reason is reproducible.
	slices.Sort(dropped)
	return dropped
}

// scanIdents returns the identifiers in a fragment of Go source. Scanning
// rather than parsing keeps it total: a span such as an operator token or a
// bare statement list is not a valid expression or declaration.
func scanIdents(text string) map[string]bool {
	out := map[string]bool{}
	fset := token.NewFileSet()
	f := fset.AddFile("", fset.Base(), len(text))
	var sc scanner.Scanner
	// A nil error handler: a fragment need not be well-formed Go, and a
	// scan error still yields the tokens before it, which is all we need.
	sc.Init(f, []byte(text), nil, 0)
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.IDENT {
			out[lit] = true
		}
	}
	return out
}

// importNames returns the identifier each import is referred to by,
// skipping dot and blank imports, which cannot be orphaned by name.
func importNames(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Name != nil {
			if imp.Name.Name != "_" && imp.Name.Name != "." {
				out[imp.Name.Name] = true
			}
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		if i := strings.LastIndex(path, "/"); i >= 0 {
			path = path[i+1:]
		}
		if path != "" {
			out[path] = true
		}
	}
	return out
}

// enclosingBody returns the innermost function body containing the span, or
// nil when the span sits at package level (where Go has no
// "declared and not used" rule).
func enclosingBody(fset *token.FileSet, file *ast.File, start, end int) ast.Node {
	var best ast.Node
	bestSize := -1
	ast.Inspect(file, func(n ast.Node) bool {
		var body *ast.BlockStmt
		switch fn := n.(type) {
		case *ast.FuncDecl:
			body = fn.Body
		case *ast.FuncLit:
			body = fn.Body
		default:
			return true
		}
		if body == nil {
			return true
		}
		lo, hi := fset.Position(body.Pos()).Offset, fset.Position(body.End()).Offset
		if lo > start || hi < end {
			return true
		}
		if size := hi - lo; bestSize < 0 || size < bestSize {
			best, bestSize = body, size
		}
		return true
	})
	return best
}

// declaredLocals returns the names Go requires to be used: those
// introduced by `:=`, by a local `var`, by a `range` clause, and by a type
// switch guard. Parameters, results and constants are excluded — none of
// them has to be used.
func declaredLocals(fn ast.Node) map[string]bool {
	out := map[string]bool{}
	add := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
			out[id.Name] = true
		}
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			if d.Tok == token.DEFINE {
				for _, lhs := range d.Lhs {
					add(lhs)
				}
			}
		case *ast.RangeStmt:
			// `for i, v := range xs` declares i and v, and an unused one is
			// an error exactly as `:=` is.
			if d.Tok == token.DEFINE {
				add(d.Key)
				add(d.Value)
			}
		case *ast.TypeSwitchStmt:
			if a, ok := d.Assign.(*ast.AssignStmt); ok && a.Tok == token.DEFINE {
				for _, lhs := range a.Lhs {
					add(lhs)
				}
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				return true
			}
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, id := range vs.Names {
						add(id)
					}
				}
			}
		}
		return true
	})
	return out
}

// countUses counts the *uses* of a name outside the byte span
// [start, end).
//
// Being written to is not a use. Go accepts neither `x := 1` nor
// `x := 1; x = 2` if x is never read, so the declaring occurrence and the
// left-hand side of a plain assignment both have to be excluded — counting
// raw identifier occurrences would miss exactly the mutants that orphan a
// variable whose only reader the mutation removes.
//
// A compound assignment (`x += 1`) does read x, and an indexed or selected
// target (`a[i] = v`) reads a and i, so only a bare identifier on the left
// of `=` or `:=` is skipped.
func countUses(root ast.Node, fset *token.FileSet, name string, start, end int) int {
	skip := map[*ast.Ident]bool{}
	markBare := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok {
			skip[id] = true
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			if d.Tok == token.DEFINE || d.Tok == token.ASSIGN {
				for _, lhs := range d.Lhs {
					markBare(lhs)
				}
			}
		case *ast.RangeStmt:
			if d.Tok == token.DEFINE {
				markBare(d.Key)
				markBare(d.Value)
			}
		case *ast.ValueSpec:
			for _, id := range d.Names {
				skip[id] = true
			}
		}
		return true
	})

	n := 0
	ast.Inspect(root, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok || id.Name != name || skip[id] {
			return true
		}
		if off := fset.Position(id.Pos()).Offset; off < start || off >= end {
			n++
		}
		return true
	})
	return n
}

// parseFragment reports whether a generated fragment parses as Go. Used by
// the generator's self-check.
func parseFragment(text string) error {
	_, err := parser.ParseFile(token.NewFileSet(), "schema.go", text, parser.SkipObjectResolution)
	return err
}
