package patch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"slices"
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
// written on the line they belong to, so no line number moves. Likewise a
// replacement that would fuse with a neighbouring token in unformatted
// source is spaced off from it first (see keepApart).
func ApplyMutant(filename string, original []byte, start, end int, replacement string) ([]byte, error) {
	patched, err := Apply(original, start, end, keepApart(original, start, end, replacement))
	if err != nil {
		return nil, err
	}
	return keepUses(filename, original, patched), nil
}

// edit replaces patched[start:end) with text. The zero edit changes nothing.
type edit struct {
	start, end int
	text       string
}

// keepUses returns patched with a repair for each import or local that
// go/types reports unused in it but not, at the same offset, in original.
// Comparing the two is what makes the single-file check sound: it runs
// without the package's other files or its dependencies, so it misreads
// some code, but the same way in both versions. An import whose package
// name guessName gets wrong reads unused in original too, and is left
// alone.
//
// Offsets agree between the versions only ahead of the replacement, which
// is where it matters. Imports always sit there, and so does every local
// whose last use a mutation can remove, as a use follows its declaration.
// A local reported past the replacement gets a harmless `_ = x`, as does
// one the check misreads: no repair can break a mutant that compiles.
func keepUses(filename string, original, patched []byte) []byte {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, patched, parser.SkipObjectResolution)
	if err != nil {
		return patched
	}
	lost := unusedOffsets(fset, file)
	// gomutants:disable-next-line BRANCH_IF,INTEGER_DECREMENT reason="fast-path optimisation; identical observable when removed (with nothing lost there is nothing to repair, and applyEdits of no edits returns the same bytes)"
	if len(lost) == 0 {
		return patched
	}
	// original parsed at discovery, or it would have no mutants.
	orig, _ := parser.ParseFile(fset, filename, original, parser.SkipObjectResolution)
	before := make(map[int]bool)
	for _, off := range unusedOffsets(fset, orig) {
		before[off] = true
	}
	var edits []edit
	for _, off := range lost {
		if !before[off] {
			edits = append(edits, repair(fset, file, off))
		}
	}
	return applyEdits(patched, edits)
}

// unusedOffsets type-checks file alone and returns the byte offsets go/types
// reports an unused import or local at: the import spec, or the local's
// declaring identifier. Every other error is the missing context (sibling
// files, dependencies) and is ignored.
func unusedOffsets(fset *token.FileSet, file *ast.File) []int {
	var offs []int
	conf := types.Config{
		Importer: stubImporter{},
		// go/types documents every error it reports as a types.Error.
		Error: func(err error) {
			if te := err.(types.Error); strings.Contains(te.Msg, "and not used") {
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

func (stubImporter) Import(importPath string) (*types.Package, error) {
	pkg := types.NewPackage(importPath, guessName(importPath))
	pkg.MarkComplete()
	return pkg, nil
}

// guessName guesses the package name of an import path the way the go
// command lays packages out: the last element, skipping a /vN
// major-version suffix, with a gopkg.in .vN suffix and a go- prefix or -go
// suffix dropped.
func guessName(importPath string) string {
	name := path.Base(importPath)
	if isMajorVersion(name) {
		name = path.Base(path.Dir(importPath))
	}
	if ext := path.Ext(name); isMajorVersion(strings.TrimPrefix(ext, ".")) {
		name = strings.TrimSuffix(name, ext)
	}
	name = strings.TrimPrefix(name, "go-")
	return strings.TrimSuffix(name, "-go")
}

// isMajorVersion reports whether s is a major-version element: v2, v10.
func isMajorVersion(s string) bool {
	digits, ok := strings.CutPrefix(s, "v")
	_, err := strconv.Atoi(digits)
	return ok && err == nil
}

// repair returns the edit that puts the import or local reported unused at
// off back to use, or the zero edit where no same-line repair exists: a cgo
// import, a type switch's symbol, a switch's init, a label. Those stay NOT
// VIABLE.
func repair(fset *token.FileSet, file *ast.File, off int) edit {
	at := func(p token.Pos) int { return fset.Position(p).Offset }
	for _, spec := range file.Imports {
		switch {
		case at(spec.Pos()) != off, spec.Path.Value == `"C"`:
		case spec.Name != nil:
			return edit{at(spec.Name.Pos()), at(spec.Name.End()), "_"}
		default:
			return edit{off, off, "_ "}
		}
	}

	// go/types reports a local unused at its declaring identifier, so the
	// path ends at it, under a := or a var spec inside a function.
	var path []ast.Node
	ast.PreorderStack(file, nil, func(n ast.Node, stack []ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && at(id.Pos()) == off {
			path = append(slices.Clone(stack), id)
		}
		return true
	})
	if path == nil {
		return edit{}
	}
	nth := func(i int) ast.Node { return path[len(path)-i] }
	use := "_ = " + nth(1).(*ast.Ident).Name
	// into opens body with the use.
	into := func(body *ast.BlockStmt) edit {
		p := at(body.Lbrace) + 1
		return edit{p, p, " " + use + ";"}
	}
	// behind follows node with the use. Behind a var spec that is a
	// statement of its own, or, in a `var ( … )` group, a spec `_ = x`.
	behind := func(node ast.Node) edit {
		p := at(node.End())
		return edit{p, p, "; " + use}
	}

	switch decl := nth(2).(type) {
	case *ast.AssignStmt:
		switch owner := nth(3).(type) {
		case *ast.IfStmt:
			return into(owner.Body)
		case *ast.ForStmt:
			return into(owner.Body)
		case *ast.CommClause:
			if owner.Comm == decl {
				p := at(owner.Colon) + 1
				return edit{p, p, " " + use + ";"}
			}
		case *ast.SwitchStmt, *ast.TypeSwitchStmt:
			return edit{}
		}
		return behind(decl)
	case *ast.RangeStmt:
		return into(decl.Body)
	case *ast.ValueSpec:
		return behind(decl)
	}
	return edit{}
}

// applyEdits applies non-overlapping edits to a copy of src, last first so
// earlier offsets stay valid. go/types reports unused imports after unused
// locals, so the edits come unsorted; edits at one offset keep their order.
// Each is kept apart from its neighbours like the mutant itself: the `_ `
// that blanks the import in `import"strconv"` would otherwise make
// `import_`.
func applyEdits(src []byte, edits []edit) []byte {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	out := slices.Clone(src)
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		out = slices.Replace(out, e.start, e.end, []byte(keepApart(out, e.start, e.end, e.text))...)
	}
	return out
}
