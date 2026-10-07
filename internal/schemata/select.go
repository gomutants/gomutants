package schemata

import (
	"go/ast"
	"go/token"
)

// guardSpot is a statement list that a deletion-shaped mutator empties, so
// schemata can guard it in place. It is recorded during the walk because
// eligibility (a trailing `fallthrough`) and the inner span are properties
// of the AST node, not of the mutant's byte range.
type guardSpot struct {
	innerStart int // First byte inside the guarded region.
	innerEnd   int // One past the last byte inside the guarded region.
	// node is the block or clause whose statement list is guarded, for the
	// terminating-statement analysis.
	node     ast.Node
	eligible bool
	reason   string
}

// selector walks a file choosing non-nested rewrite locations. Descent
// stops as soon as a site is committed, which is what guarantees the sites
// cannot nest and so can be emitted as a flat list of byte edits.
type selector struct {
	fset  *token.FileSet
	sites []site
	// spots is keyed by the byte offset a deletion-shaped mutant reports as
	// its StartOffset, so a mutant can be matched to its AST context.
	spots map[int]guardSpot
	// inserts is keyed by the offset RANGE_BREAK inserts at (one past the
	// opening brace of a range body).
	inserts map[int]bool
}

func (s *selector) off(p token.Pos) int { return s.fset.Position(p).Offset }

func (s *selector) add(kind siteKind, start, end int, node ast.Node) {
	s.sites = append(s.sites, site{kind: kind, start: start, end: end, node: node})
}

// selectSites returns the rewrite locations of one file, plus the guard and
// insert spots that deletion-shaped mutators can use.
func selectSites(fset *token.FileSet, file *ast.File) ([]site, map[int]guardSpot, map[int]bool) {
	s := &selector{fset: fset, spots: map[int]guardSpot{}, inserts: map[int]bool{}}
	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if decl.Body != nil {
				s.walkStmtList(decl.Body.List)
			}
		case *ast.GenDecl:
			// Only `var` initializers are live expressions. `const` values
			// and array lengths must stay constant, and a closure is not a
			// constant, so nothing inside them can be schematized. Type and
			// import declarations hold no runtime expressions at all.
			//
			// Package-level `var` initializers *are* schematizable in Go,
			// unlike the "static mutants" Stryker documents for C# and JS:
			// package-level initialization is dependency-ordered, so an
			// initializer that calls the guard observes the parsed value.
			if decl.Tok != token.VAR {
				continue
			}
			for _, spec := range decl.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, v := range vs.Values {
						s.walkExpr(v)
					}
				}
			}
		}
	}
	return s.sites, s.spots, s.inserts
}

// walkStmtList visits the elements of a statement list — the only place a
// statement can be replaced by an if/else chain, since that chain has to go
// somewhere a statement is allowed.
func (s *selector) walkStmtList(list []ast.Stmt) {
	for _, st := range list {
		if simpleStmt(st) && !containsLabel(st) {
			s.add(siteStmtWrap, s.off(st.Pos()), s.off(st.End()), st)
			continue
		}
		s.walkStmt(st)
	}
}

// walkStmt descends a statement that is not itself a rewrite site.
func (s *selector) walkStmt(st ast.Stmt) {
	switch n := st.(type) {
	case nil:
		return
	case *ast.BlockStmt:
		s.walkStmtList(n.List)
	case *ast.IfStmt:
		s.walkStmt(n.Init)
		s.walkCond(n.Cond)
		s.recordGuard(n.Body)
		s.walkStmtList(n.Body.List)
		// An `else if` chains to another IfStmt; only a plain else block is
		// a BRANCH_ELSE target, which recordGuard checks for itself.
		if blk, ok := n.Else.(*ast.BlockStmt); ok {
			s.recordGuard(blk)
		}
		s.walkStmt(n.Else)
	case *ast.ForStmt:
		s.walkStmt(n.Init)
		s.walkCond(n.Cond)
		s.walkStmt(n.Post)
		s.walkStmtList(n.Body.List)
	case *ast.RangeStmt:
		s.walkExpr(n.X)
		// RANGE_BREAK inserts one past the opening brace.
		s.inserts[s.off(n.Body.Lbrace)+1] = true
		s.walkStmtList(n.Body.List)
	case *ast.SwitchStmt:
		s.walkStmt(n.Init)
		// A tag-less switch compares each case expression against true, so
		// those expressions are bool and can be wrapped. A tagged switch
		// compares them against the tag's type, which is not knowable here.
		s.walkSwitchBody(n.Body, n.Tag == nil)
		s.walkExpr(n.Tag)
	case *ast.TypeSwitchStmt:
		s.walkStmt(n.Init)
		// Case values are types, never bool expressions.
		s.walkSwitchBody(n.Body, false)
	case *ast.SelectStmt:
		for _, c := range n.Body.List {
			cc, ok := c.(*ast.CommClause)
			if !ok {
				continue
			}
			// Comm is the send or receive itself, not a list element, so it
			// cannot be replaced by an if/else chain.
			s.walkStmt(cc.Comm)
			s.recordCaseGuard(cc, cc.Body)
			s.walkStmtList(cc.Body)
		}
	case *ast.LabeledStmt:
		s.walkStmt(n.Stmt)
	case *ast.DeclStmt:
		// A local `var` initializer is a live expression; `const` and `type`
		// are not.
		gd, ok := n.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return
		}
		for _, spec := range gd.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				for _, v := range vs.Values {
					s.walkExpr(v)
				}
			}
		}
	case *ast.AssignStmt:
		// Reached only for `:=`, which walkStmtList declined to wrap. The
		// right-hand side can still hold a wrappable bool expression.
		for _, v := range n.Rhs {
			s.walkExpr(v)
		}
	default:
		// Remaining statements are either simple ones reached outside a
		// statement list (a for-loop post, an if init) or have no
		// expressions worth descending into.
		ast.Inspect(st, func(c ast.Node) bool {
			if e, ok := c.(ast.Expr); ok && boolExpr(e) && !containsRecover(e) {
				s.add(siteBoolWrap, s.off(e.Pos()), s.off(e.End()), e)
				return false
			}
			return true
		})
	}
}

func (s *selector) walkSwitchBody(body *ast.BlockStmt, caseExprsAreBool bool) {
	if body == nil {
		return
	}
	for _, c := range body.List {
		cc, ok := c.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, e := range cc.List {
			if caseExprsAreBool {
				s.walkCond(e)
			} else {
				s.walkExpr(e)
			}
		}
		s.recordCaseGuard(cc, cc.Body)
		s.walkStmtList(cc.Body)
	}
}

// walkCond handles an expression whose position proves it is bool — an
// `if` or `for` condition, or a case of a tag-less switch — even when its
// own shape does not (a call, an identifier).
func (s *selector) walkCond(e ast.Expr) {
	if e == nil {
		return
	}
	if containsRecover(e) {
		s.walkExpr(e)
		return
	}
	s.add(siteBoolWrap, s.off(e.Pos()), s.off(e.End()), e)
}

// walkExpr descends an expression, committing to a wrap at the outermost
// bool-valued subexpression it finds.
func (s *selector) walkExpr(e ast.Expr) {
	if e == nil {
		return
	}
	if boolExpr(e) && !containsRecover(e) {
		s.add(siteBoolWrap, s.off(e.Pos()), s.off(e.End()), e)
		return
	}
	switch n := e.(type) {
	case *ast.FuncLit:
		// A closure body is a statement list like any other.
		s.walkStmtList(n.Body.List)
	case *ast.CallExpr:
		s.walkExpr(n.Fun)
		for _, a := range n.Args {
			s.walkExpr(a)
		}
	case *ast.BinaryExpr:
		s.walkExpr(n.X)
		s.walkExpr(n.Y)
	case *ast.UnaryExpr:
		s.walkExpr(n.X)
	case *ast.ParenExpr:
		s.walkExpr(n.X)
	case *ast.SelectorExpr:
		s.walkExpr(n.X)
	case *ast.IndexExpr:
		s.walkExpr(n.X)
		s.walkExpr(n.Index)
	case *ast.SliceExpr:
		s.walkExpr(n.X)
		s.walkExpr(n.Low)
		s.walkExpr(n.High)
		s.walkExpr(n.Max)
	case *ast.StarExpr:
		s.walkExpr(n.X)
	case *ast.KeyValueExpr:
		// An array or struct key must stay constant; only the value is live.
		s.walkExpr(n.Value)
	case *ast.CompositeLit:
		// Elts only — Type can carry an array length, which must stay
		// constant.
		for _, el := range n.Elts {
			s.walkExpr(el)
		}
	case *ast.TypeAssertExpr:
		s.walkExpr(n.X)
	}
	// Other expression kinds are types or literals with nothing to descend
	// into; mutants there fall back.
}

// recordGuard notes an if-statement block that BRANCH_IF or BRANCH_ELSE can
// empty. The mutant's StartOffset for those mutators is the opening brace.
func (s *selector) recordGuard(b *ast.BlockStmt) {
	if b == nil {
		return
	}
	start := s.off(b.Lbrace)
	spot := guardSpot{innerStart: start + 1, innerEnd: s.off(b.Rbrace), node: b, eligible: true}
	if len(b.List) == 0 {
		spot.eligible, spot.reason = false, "empty block"
	}
	s.spots[start] = spot
}

// recordCaseGuard notes a case or comm clause body that BRANCH_CASE can
// empty. Unlike a block the span has no braces: the mutator reports the
// first statement's start and the last statement's end.
func (s *selector) recordCaseGuard(clause ast.Node, list []ast.Stmt) {
	if len(list) == 0 {
		return
	}
	start := s.off(list[0].Pos())
	spot := guardSpot{innerStart: start, innerEnd: s.off(list[len(list)-1].End()), node: clause, eligible: true}
	if endsWithFallthrough(list) {
		// Guarding would move the fallthrough out of final position in its
		// clause, which does not compile.
		spot.eligible, spot.reason = false, "case body ends in fallthrough"
	}
	s.spots[start] = spot
}
