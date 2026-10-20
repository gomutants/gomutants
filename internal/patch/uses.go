package patch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"
)

// ApplyMutant is Apply for a mutant of the Go file filename: it also puts
// back to use any import or local variable whose last use the replacement
// removed. Go rejects an unused import or local, so without this a mutant
// such as RETURN_ZERO's `return "n" + strconv.Itoa(n)` → `return ""` fails
// to compile on the now-unused strconv and reads NOT VIABLE, though the
// mutation itself is valid and the tests never saw it.
//
// The repairs change no behaviour: an import is blanked (`_ "strconv"`),
// which still runs its init, and a local is read once (`_ = x`). Both are
// written on the line they belong to, so no line number moves.
func ApplyMutant(filename string, original []byte, start, end int, replacement string) ([]byte, error) {
	patched, err := Apply(original, start, end, replacement)
	if err != nil {
		return nil, err
	}
	return keepUses(filename, original, patched, start, end, len(replacement)), nil
}

// edit replaces patched[start:end) with text.
type edit struct {
	start, end int
	text       string
}

// keepUses returns patched with a repair for each import or local that
// go/types reports unused in it but not in original. Comparing the two is
// what makes the single-file check sound: it runs without the package's
// other files or its dependencies, so it misreads some code, but the same
// way in both versions. An import whose package name guessName gets wrong
// reads unused in original too, and is left alone; a local is matched by
// its declaration's offset, mapped across the replaced span.
//
// Every miss leaves the mutant as it compiles today, and an extra `_ = x`
// is harmless, so no reading of the code can break a mutant that compiles.
func keepUses(filename string, original, patched []byte, start, end, replLen int) []byte {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, patched, parser.SkipObjectResolution)
	if err != nil {
		return patched
	}
	lost := unusedOffsets(fset, file)
	if len(lost) == 0 {
		return patched
	}
	orig, err := parser.ParseFile(fset, filename, original, parser.SkipObjectResolution)
	if err != nil {
		return patched
	}
	before := make(map[int]bool)
	for _, off := range unusedOffsets(fset, orig) {
		before[off] = true
	}
	replEnd := start + replLen
	var edits []edit
	for _, off := range lost {
		origOff := off
		switch {
		case off >= replEnd:
			origOff = off - replLen + (end - start)
		case off >= start:
			continue // declared by the replacement itself
		}
		if before[origOff] {
			continue
		}
		if e, ok := repair(fset, file, off); ok && (e.end <= start || e.start >= replEnd) {
			edits = append(edits, e)
		}
	}
	return applyEdits(patched, edits)
}

// unusedOffsets type-checks file alone and returns the byte offsets go/types
// reports an unused import or local at: the import's path, or the local's
// declaring identifier. Every other error is the missing context (sibling
// files, dependencies) and is ignored.
func unusedOffsets(fset *token.FileSet, file *ast.File) []int {
	var offs []int
	conf := types.Config{
		Importer: stubImporter{},
		Error: func(err error) {
			if te, ok := err.(types.Error); ok && te.Soft && strings.Contains(te.Msg, "not used") {
				offs = append(offs, fset.Position(te.Pos).Offset)
			}
		},
	}
	_, _ = conf.Check(file.Name.Name, fset, []*ast.File{file}, nil)
	return offs
}

// stubImporter imports every path as an empty package named by guessName,
// enough for go/types to tell which imports a file uses.
type stubImporter struct{}

func (stubImporter) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, guessName(path))
	pkg.MarkComplete()
	return pkg, nil
}

// guessName guesses the package name of import path the way the go command
// lays packages out: the last element, skipping a /vN major-version suffix,
// with a gopkg.in .vN suffix and a go- prefix or -go suffix dropped.
func guessName(path string) string {
	elems := strings.Split(path, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && isMajorVersion(name) {
		name = elems[len(elems)-2]
	}
	if i := strings.LastIndex(name, "."); i > 0 && isMajorVersion(name[i+1:]) {
		name = name[:i]
	}
	name = strings.TrimPrefix(name, "go-")
	return strings.TrimSuffix(name, "-go")
}

// isMajorVersion reports whether s is a major-version element: v2, v10.
func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(s[1:])
	return err == nil
}

// repair returns the edit that puts the import or local reported unused at
// off back to use, or false where no same-line repair exists: a cgo import,
// a type switch's symbol, a switch's init. Those stay NOT VIABLE.
func repair(fset *token.FileSet, file *ast.File, off int) (edit, bool) {
	at := func(p token.Pos) int { return fset.Position(p).Offset }
	for _, spec := range file.Imports {
		if at(spec.Pos()) > off || off >= at(spec.End()) {
			continue
		}
		if spec.Path.Value == `"C"` {
			return edit{}, false
		}
		if spec.Name != nil {
			return edit{at(spec.Name.Pos()), at(spec.Name.End()), "_"}, true
		}
		p := at(spec.Path.Pos())
		return edit{p, p, "_ "}, true
	}

	path := pathTo(fset, file, off)
	if len(path) < 2 {
		return edit{}, false
	}
	id, ok := path[len(path)-1].(*ast.Ident)
	if !ok {
		return edit{}, false
	}
	use := "_ = " + id.Name
	// after inserts the use behind stmt, which must sit in a statement list.
	after := func(stmt ast.Node, list ast.Node) (edit, bool) {
		switch list.(type) {
		case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
			p := at(stmt.End())
			return edit{p, p, "; " + use}, true
		}
		return edit{}, false
	}
	// into opens body with the use.
	into := func(body *ast.BlockStmt) (edit, bool) {
		p := at(body.Lbrace) + 1
		return edit{p, p, " " + use + ";"}, true
	}

	switch decl := path[len(path)-2].(type) {
	case *ast.AssignStmt:
		if decl.Tok != token.DEFINE || len(path) < 3 {
			return edit{}, false
		}
		switch owner := path[len(path)-3].(type) {
		case *ast.IfStmt:
			return into(owner.Body)
		case *ast.ForStmt:
			return into(owner.Body)
		case *ast.CommClause:
			if owner.Comm == decl {
				p := at(owner.Colon) + 1
				return edit{p, p, " " + use + ";"}, true
			}
			return after(decl, owner)
		default:
			return after(decl, owner)
		}
	case *ast.RangeStmt:
		if decl.Tok == token.DEFINE {
			return into(decl.Body)
		}
	case *ast.ValueSpec:
		// ValueSpec → GenDecl → DeclStmt → statement list.
		if len(path) >= 5 {
			if stmt, ok := path[len(path)-4].(*ast.DeclStmt); ok {
				return after(stmt, path[len(path)-5])
			}
		}
	}
	return edit{}, false
}

// pathTo returns the chain of nodes from file down to the identifier that
// starts at off, or nil when none does.
func pathTo(fset *token.FileSet, file *ast.File, off int) []ast.Node {
	var stack, found []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		if id, ok := n.(*ast.Ident); ok && fset.Position(id.Pos()).Offset == off {
			found = append([]ast.Node(nil), stack...)
		}
		return true
	})
	return found
}

// applyEdits applies non-overlapping edits to src, last first so earlier
// offsets stay valid. Edits at one offset keep their order.
func applyEdits(src []byte, edits []edit) []byte {
	if len(edits) == 0 {
		return src
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	out := append([]byte(nil), src...)
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	return out
}
