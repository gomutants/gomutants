// Package schemata implements mutant schemata: instead of recompiling the
// program once per mutant, every mutant of a package is emitted into the
// source at once, each one inert behind a guard keyed to the
// GOMUTANTS_ACTIVE environment variable. The package is then compiled once
// and its test binary run N times, which removes the per-mutant compile and
// link that dominates a cold run.
//
// Not every mutation can be expressed that way in Go, which has no
// conditional expression and — deliberately, to keep the module
// dependency-free — no type information available here. Anything this
// package cannot prove safe from syntax alone is reported as a fallback and
// runs on the existing per-mutant overlay path, so a schemata run and a
// plain run reach the same verdict for every mutant.
package schemata

import (
	"go/ast"
	"go/token"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// siteKind is the shape of the rewrite at a chosen location.
type siteKind int

const (
	// siteBoolWrap replaces an expression that is bool by syntax alone with
	// an immediately-invoked closure selecting between alternatives. Only
	// bool works without type information: Go needs a written result type
	// for a function literal, and a comparison, a logical operator and an
	// `if`/`for` condition are the only expressions whose type is knowable
	// from the parse tree.
	siteBoolWrap siteKind = iota
	// siteStmtWrap duplicates one simple statement into an if/else chain.
	// Laziness, evaluation order and short-circuiting are preserved because
	// exactly one copy ever runs.
	siteStmtWrap
	// siteBlockGuard wraps an existing statement list in `if !on(id) { … }`.
	// It is emitted as two zero-width insertions rather than a span
	// replacement, so mutants nested inside the block keep their original
	// offsets and can be rewritten independently.
	siteBlockGuard
	// siteInsert adds a guarded statement at a single offset (RANGE_BREAK).
	siteInsert
)

// site is one rewrite location: a span of the original file plus every
// mutant routed into it. Sites never nest — selectSites stops descending
// once it commits to one — which is what lets the whole file be emitted as
// a flat list of non-overlapping byte edits.
type site struct {
	kind  siteKind
	start int // Byte offset of the span start.
	end   int // Byte offset of the span end (exclusive).
	// node is the statement a siteStmtWrap duplicates, or the block or
	// clause a siteBlockGuard wraps. It is kept so the terminating-statement
	// analysis can ask what the rewrite does to the enclosing function.
	node    ast.Node
	mutants []mutator.Mutant
}

// fallback is a mutant schemata declined, with the reason. The reason is
// carried for diagnostics and for the tests, which assert on *why* a mutant
// was declined rather than only that it was.
type fallback struct {
	mutant mutator.Mutant
	reason string
}

// blockGuardTypes are the mutators whose mutation empties a block or a
// statement list. They are guarded in place rather than duplicated, so they
// cost no extra source and compose with mutants nested inside them.
var blockGuardTypes = map[mutator.MutationType]bool{
	mutator.BranchIf:   true,
	mutator.BranchElse: true,
	mutator.BranchCase: true,
}

// simpleStmt reports whether a statement can be duplicated into an if/else
// chain without changing what is in scope after it. Everything that can
// introduce a name (`:=`, var, const, type) is excluded, because a copy
// inside a branch would scope that name to the branch. Compound statements
// are excluded too: their bodies are statement lists handled on their own,
// and duplicating them would multiply the enclosed source for no gain.
func simpleStmt(s ast.Stmt) bool {
	switch st := s.(type) {
	case *ast.ExprStmt, *ast.IncDecStmt, *ast.ReturnStmt, *ast.SendStmt,
		*ast.DeferStmt, *ast.GoStmt:
		return true
	case *ast.AssignStmt:
		// `:=` declares; every other assignment token only writes.
		return st.Tok != token.DEFINE
	case *ast.BranchStmt:
		// break/continue/goto duplicate safely. `fallthrough` does not: it
		// must be the final statement of a case clause, and wrapping it in
		// an if makes it illegal.
		return st.Tok != token.FALLTHROUGH
	default:
		return false
	}
}

// boolExpr reports whether an expression's type is bool by syntax alone,
// independent of any declaration. A comparison and a logical operator
// always yield bool; so does `!x`, which is only legal on a bool.
func boolExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		switch x.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ,
			token.LAND, token.LOR:
			return true
		}
	case *ast.UnaryExpr:
		return x.Op == token.NOT
	case *ast.ParenExpr:
		return boolExpr(x.X)
	}
	return false
}

// containsLabel reports whether n declares a label. A labelled statement
// cannot be duplicated: two copies would declare the same label twice.
func containsLabel(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(c ast.Node) bool {
		if _, ok := c.(*ast.LabeledStmt); ok {
			found = true
		}
		return !found
	})
	return found
}

// containsRecover reports whether n calls the predeclared recover. Such an
// expression cannot be moved into a closure: recover only returns the
// panic value when called directly by the deferred function, so wrapping it
// silently turns a working recovery into a nil one. (Verified: an `if
// recover() != nil` guarded by a closure never recovers.)
//
// The check is syntactic, in keeping with the rest of the package, so a
// local variable shadowing `recover` reads as a recover call here. That
// costs one mutant the fast path and is the safe direction to be wrong in.
func containsRecover(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(c ast.Node) bool {
		call, ok := c.(*ast.CallExpr)
		if !ok {
			return !found
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recover" {
			found = true
		}
		return !found
	})
	return found
}

// endsWithFallthrough reports whether the last statement of a list is a
// `fallthrough`. Guarding such a list would move the fallthrough out of
// final position in its case clause, which does not compile.
func endsWithFallthrough(list []ast.Stmt) bool {
	if len(list) == 0 {
		return false
	}
	br, ok := list[len(list)-1].(*ast.BranchStmt)
	return ok && br.Tok == token.FALLTHROUGH
}
