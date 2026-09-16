package schemata

import (
	"go/ast"
	"go/token"
)

// Terminating-statement analysis, per the Go spec section of the same name.
//
// It is needed because two of the rewrites can turn a terminating statement
// into a non-terminating one. Emptying a case body that consisted of a
// single `return` leaves `_ = 0`, and guarding a block in place leaves an
// `if` with no else. If the enclosing function has results, the compiler
// then reports "missing return" — for the *whole package*, since schemata
// compiles every mutant together.
//
// The build-error loop would eventually demote those mutants, but each
// round costs a package compile, and the shapes involved (a case body that
// is a single return) are common enough to be worth ruling out up front.
// The verdict is unchanged either way: such a mutation does not compile on
// the overlay path either, and is reported NOT VIABLE there.

// nonTerm marks nodes the analysis should pretend do not terminate: a
// statement replaced by a non-terminating alternative, or a clause whose
// body has been guarded.
type nonTerm map[ast.Node]bool

// terminates reports whether a statement is a terminating statement.
func (nt nonTerm) terminates(s ast.Stmt) bool {
	if s == nil || nt[s] {
		return false
	}
	switch n := s.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return n.Tok == token.GOTO
	case *ast.ExprStmt:
		return isPanicCall(n.X)
	case *ast.BlockStmt:
		return nt.terminatesList(n.List)
	case *ast.IfStmt:
		return n.Else != nil && nt.terminates(n.Body) && nt.terminates(n.Else)
	case *ast.ForStmt:
		// A `for {}` with no condition and no break out of it.
		return n.Cond == nil && !hasEscapingBreak(n.Body)
	case *ast.LabeledStmt:
		return nt.terminates(n.Stmt)
	case *ast.SwitchStmt:
		return nt.clausesTerminate(n.Body, true)
	case *ast.TypeSwitchStmt:
		return nt.clausesTerminate(n.Body, true)
	case *ast.SelectStmt:
		// A select needs no default: every arm blocks until one is ready.
		return nt.clausesTerminate(n.Body, false)
	}
	return false
}

// terminatesList reports whether a statement list ends in a terminating
// statement.
func (nt nonTerm) terminatesList(list []ast.Stmt) bool {
	if len(list) == 0 {
		return false
	}
	return nt.terminates(list[len(list)-1])
}

// clausesTerminate implements the switch and select rules: no break
// referring to the statement, a default clause when one is required, and
// every clause body ending in a terminating statement or a fallthrough.
func (nt nonTerm) clausesTerminate(body *ast.BlockStmt, needDefault bool) bool {
	if body == nil {
		return false
	}
	hasDefault := false
	for _, c := range body.List {
		var list []ast.Stmt
		switch cc := c.(type) {
		case *ast.CaseClause:
			hasDefault = hasDefault || cc.List == nil
			list = cc.Body
		case *ast.CommClause:
			hasDefault = hasDefault || cc.Comm == nil
			list = cc.Body
		default:
			return false
		}
		if nt[c] {
			return false
		}
		if endsWithFallthrough(list) {
			continue
		}
		if !nt.terminatesList(list) {
			return false
		}
	}
	if needDefault && !hasDefault {
		return false
	}
	// A break aimed at this statement escapes it, so it does not terminate.
	return !hasEscapingBreak(body)
}

func isPanicCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "panic"
}

// hasEscapingBreak reports whether a breakable statement's body contains a
// break that binds to it. An unlabelled break binds to the innermost
// enclosing for, switch or select, so descent stops at any of those; it
// cannot cross a function literal either. A labelled break may target an
// outer statement, and is treated as escaping — the safe direction, since
// it only costs a mutant the fast path.
func hasEscapingBreak(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil || found {
			return false
		}
		if n == ast.Node(body) {
			return true
		}
		switch st := n.(type) {
		case *ast.BranchStmt:
			if st.Tok == token.BREAK {
				found = true
			}
			return false
		case *ast.FuncLit, *ast.ForStmt, *ast.RangeStmt,
			*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			return false
		}
		return true
	})
	return found
}

// funcInfo is the enclosing function of a rewrite site: its body and
// whether Go requires that body to terminate.
type funcInfo struct {
	body        *ast.BlockStmt
	needsReturn bool
}

// enclosingFuncs indexes every function body in a file by byte span so a
// rewrite site can find the one it sits in.
func enclosingFuncs(fset *token.FileSet, file *ast.File) []funcSpan {
	var out []funcSpan
	ast.Inspect(file, func(n ast.Node) bool {
		var body *ast.BlockStmt
		var results *ast.FieldList
		switch fn := n.(type) {
		case *ast.FuncDecl:
			body, results = fn.Body, fn.Type.Results
		case *ast.FuncLit:
			body, results = fn.Body, fn.Type.Results
		default:
			return true
		}
		if body == nil {
			return true
		}
		out = append(out, funcSpan{
			start: fset.Position(body.Pos()).Offset,
			end:   fset.Position(body.End()).Offset,
			info:  funcInfo{body: body, needsReturn: results != nil && len(results.List) > 0},
		})
		return true
	})
	return out
}

type funcSpan struct {
	start, end int
	info       funcInfo
}

// innermost returns the tightest function body containing the span.
func innermost(spans []funcSpan, start, end int) (funcInfo, bool) {
	best := funcInfo{}
	bestSize := -1
	for _, s := range spans {
		if s.start > start || s.end < end {
			continue
		}
		if size := s.end - s.start; bestSize < 0 || size < bestSize {
			best, bestSize = s.info, size
		}
	}
	return best, bestSize >= 0
}

// breaksReturn reports whether treating node as non-terminating would leave
// the enclosing function without a terminating body.
func breaksReturn(spans []funcSpan, start, end int, node ast.Node) bool {
	fn, ok := innermost(spans, start, end)
	if !ok || !fn.needsReturn {
		return false
	}
	nt := nonTerm{node: true}
	return !nt.terminatesList(fn.body.List)
}
